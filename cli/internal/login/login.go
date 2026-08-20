package login

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
	"github.com/Alexnex31/Norite/daemon/credentials"
)

// `norite login`, the password path (Milestone M7). The OAuth paths arrive at M8 (loopback) and M9
// (device code) and reuse everything below the prompt.
//
// # Why the CLI performs the login rather than the daemon
//
// The daemon is the sole holder of its account's tokens (ADR 0011), and from M20 the CLI will hand
// credentials to it over the local IPC socket and stop touching the store at all. That socket does not
// exist yet, and login is the one flow with a real reason to run here regardless: it is the only moment a
// password exists, and it exists in the process the person typed it into. Sending a password anywhere it
// does not have to go is the thing worth avoiding.

// ErrNoTerminal is returned when an answer is needed and there is nowhere to ask for one.
//
// The same shape as instanceinit.ErrNotATerminal, and for the same reason: a command that blocks forever on
// input nobody is there to type, or reads EOF and treats an empty password as an answer, is worse than one
// that says what it needs. `main` maps it to exit code 2 and prints it without the "norite:" prefix, so a
// usage problem does not read like a crash.
//
// Wrapped rather than returned bare, so each question can say what would have answered it — an email
// address and a password are missing for the same reason and are fixed by different flags. A script that
// omits either gets the same exit code, which is what lets it tell "I did not supply an input" apart from
// "the credentials were wrong".
var ErrNoTerminal = errors.New("this command needs an interactive terminal to ask its questions")

// passwordEnvVar is the scripted way to supply a password.
//
// An environment variable and deliberately not a flag: a flag value is visible in the process list to every
// other user on the machine, and in the shell history of the person who typed it. The same rule the
// instance wizard follows for the database password.
const passwordEnvVar = "NORITE_PASSWORD"

// instanceEnvVar names the instance, for a scripted run that has no stored session to inherit one from.
const instanceEnvVar = "NORITE_INSTANCE"

// Options is what `norite login` was asked to do.
type Options struct {
	// Instance is the URL given on the command line. Empty means fall back to the environment, then to the
	// instance a previous login recorded.
	Instance string
	// Email identifies the account. Empty means ask.
	Email string
	// DeviceName is what this machine will be called in the account's session list. Empty means the
	// hostname, which is what a person will recognize.
	DeviceName string
	// Provider selects an OAuth sign-in — "google" or "github". Empty means the password path, which is
	// what bare `norite login` still does.
	Provider string
	// NoBrowser prints the sign-in URL instead of launching anything, and keeps listening.
	//
	// It does not mean "headless": a machine with no browser at all is M9's device-code flow. This is for
	// SSH with a forwarded port, and for anyone whose default browser opens the wrong profile.
	NoBrowser bool
}

// Runner performs a login. Its dependencies are injected so the whole flow is testable without a terminal,
// a keyring, or a network.
type Runner struct {
	Options Options

	// Store is where the resulting session goes.
	Store *credentials.Store

	// Out receives everything a person reads. Nothing secret is ever written here.
	Out io.Writer

	// ReadLine reads one visible line — the email address.
	ReadLine func(prompt string) (string, error)
	// ReadSecret reads without echoing. Never falls back to a visible read: a password echoed once is in
	// the scrollback of whoever walks past next.
	ReadSecret func(prompt string) (string, error)
	// Interactive reports whether questions can be asked at all.
	Interactive bool

	// Hostname names this machine, for the default device name.
	Hostname func() (string, error)

	// newClient builds the API client. Indirected for tests; production leaves it nil.
	newClient func(baseURL string) *client
	// openBrowser launches the sign-in URL. Indirected for tests; production leaves it nil.
	openBrowser func(ctx context.Context, target string) error
	// loopbackPorts overrides the built-in list, so tests never contend for a fixed port on a machine
	// running several packages at once.
	loopbackPorts []int
}

// session is what both sign-in methods need, and what the tail below consumes.
//
// A struct so that finish takes three arguments rather than six, four of which would be strings. Six
// positional strings is how the wrong one eventually gets passed.
type session struct {
	instanceURL string
	deviceID    string
	deviceName  string
	api         *client
}

// Run performs the login and stores the result.
//
// Three parts, and the split is the shape M8 needed: everything before a credential exists is shared,
// everything after it is shared, and only the middle differs between a password and a provider.
func (r *Runner) Run(ctx context.Context) error {
	s, err := r.prepare()
	if err != nil {
		return err
	}

	var pair tokenPair
	var fallbackName string
	if provider := r.Options.Provider; provider != "" {
		pair, err = r.signInWithOAuth(ctx, s, provider)
	} else {
		pair, fallbackName, err = r.signInWithPassword(ctx, s)
	}
	if err != nil {
		return err
	}

	return r.finish(ctx, s, pair, fallbackName)
}

// prepare works out where this login is going and who is doing it, before any credential exists.
func (r *Runner) prepare() (session, error) {
	// Read once, at the top. Separate LoadRecord calls used to answer a question each, taking and releasing
	// the cross-process lock every time — so a daemon's startup Save landing between two of them produced a
	// login assembled from two different records. It also meant an unreadable record failed at whichever
	// question happened to ask second.
	previous, err := r.loadPrevious()
	if err != nil {
		return session{}, err
	}

	instanceURL, err := r.resolveInstance(previous)
	if err != nil {
		return session{}, err
	}

	// Said before anything is typed or any browser opens, never after: the point of a warning is to let
	// someone stop. It covers both methods deliberately — an OAuth sign-in has no password to expose, and
	// the exchange code and the token pair cross the same cleartext hop.
	if looksLikeHTTP(instanceURL) {
		r.printf("Warning: %s is plain HTTP, so everything this sends — including your credentials —\n"+
			"crosses the network unencrypted.\n", instanceURL)
	}

	// The store owns this, not the login: it has to outlive both a logout and the record file itself.
	deviceID, err := r.Store.DeviceID()
	if err != nil {
		return session{}, err
	}

	return session{
		instanceURL: instanceURL,
		deviceID:    deviceID,
		deviceName:  r.resolveDeviceName(previous),
		api:         r.client(instanceURL),
	}, nil
}

// signInWithPassword is M7's flow.
//
// The second return is the address that was typed, shown if the /users/@me lookup fails. The OAuth path has
// no equivalent — it never learns an address — and passes an empty one.
func (r *Runner) signInWithPassword(ctx context.Context, s session) (tokenPair, string, error) {
	email, err := r.resolveEmail()
	if err != nil {
		return tokenPair{}, "", err
	}
	password, err := r.resolvePassword()
	if err != nil {
		return tokenPair{}, "", err
	}

	pair, err := s.api.login(ctx, loginRequest{
		Email:      email,
		Password:   password,
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
	})
	if err != nil {
		return tokenPair{}, "", err
	}
	return pair, email, nil
}

// finish is everything below the prompt: who the token belongs to, storing it, and saying so.
func (r *Runner) finish(ctx context.Context, s session, pair tokenPair, fallbackName string) error {
	// Who the token belongs to, from the instance rather than from what was typed: an email address is how
	// someone signs in, and a username is how everyone else sees them. A failure here is not fatal — the
	// session is valid either way — so it degrades to showing whatever the method had, or nothing.
	record := credentials.Record{
		InstanceURL: s.instanceURL,
		Username:    fallbackName,
		DeviceID:    s.deviceID,
		DeviceName:  s.deviceName,
	}
	if who, err := s.api.me(ctx, pair.AccessToken); err == nil {
		record.UserID = who.ID
		record.Username = who.Username
	}

	// Only the refresh token is kept. The access token in hand expires in fifteen minutes and the daemon
	// will mint its own; writing it down would add a second credential at rest to buy nothing.
	if err := r.Store.Save(record, pair.RefreshToken); err != nil {
		return err
	}

	// Sanitized again at the print, though api.me already cleaned it: the fallback value is the address that
	// was typed, and reading this line should not require tracing where the string came from (rule 19).
	if record.Username != "" {
		r.printf("Signed in as %s on %s.\n", termsafe.Text(record.Username), s.instanceURL)
	} else {
		r.printf("Signed in on %s.\n", s.instanceURL)
	}
	r.printf("This device is %q; its credential is stored in %s.\n", s.deviceName, r.Store.SecretLocation())
	r.printf("Start the background daemon with `norite daemon start` if it is not running already.\n")
	return nil
}

// resolveInstance decides which instance to log in to.
//
// Flag, then environment, then the instance a previous login recorded. The last is what makes re-logging-in
// after a password change a single word rather than a URL someone has to remember.
func (r *Runner) resolveInstance(previous credentials.Record) (string, error) {
	if r.Options.Instance != "" {
		return credentials.ParseInstanceURL(r.Options.Instance)
	}
	if fromEnv := strings.TrimSpace(os.Getenv(instanceEnvVar)); fromEnv != "" {
		return credentials.ParseInstanceURL(fromEnv)
	}
	if previous.InstanceURL != "" {
		// Re-parsed rather than trusted, even though this value was written by a previous run of this same
		// command. The record is a plain file a person can edit and an older build may have written, and
		// this string decides where a password is about to be POSTed — a `https://user:pass@evil.example`
		// in it is exactly what ParseInstanceURL refuses from every other source.
		return credentials.ParseInstanceURL(previous.InstanceURL)
	}

	return "", fmt.Errorf(
		"no instance to log in to: pass --instance https://chat.example.com, or set %s", instanceEnvVar)
}

// loadPrevious reads the stored record, treating "nothing stored yet" as an empty one.
func (r *Runner) loadPrevious() (credentials.Record, error) {
	record, err := r.Store.LoadRecord()
	switch {
	case err == nil:
		return record, nil
	case errors.Is(err, credentials.ErrNoCredential):
		return credentials.Record{}, nil
	default:
		// An unreadable record is reported with the way out, because logging in again is the fix and
		// `norite logout` is what clears it.
		return credentials.Record{}, fmt.Errorf(
			"%w; run `norite logout` to clear it and sign in again", err)
	}
}

func (r *Runner) resolveEmail() (string, error) {
	if email := strings.TrimSpace(r.Options.Email); email != "" {
		return email, nil
	}
	if !r.Interactive {
		return "", fmt.Errorf("%w: pass --email, or run it from a terminal. (--provider signs in "+
			"through a browser and needs one to be openable, not a terminal — M9 adds the flow for a "+
			"machine with neither.)", ErrNoTerminal)
	}

	email, err := r.ReadLine("Email: ")
	if err != nil {
		return "", err
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("an email address is required")
	}
	return email, nil
}

func (r *Runner) resolvePassword() (string, error) {
	// The environment first, so a scripted run never depends on a terminal being present.
	if password := os.Getenv(passwordEnvVar); password != "" {
		return password, nil
	}
	if !r.Interactive {
		return "", fmt.Errorf("%w: set %s, or run it from a terminal. (--provider signs in through a "+
			"browser and needs one to be openable, not a terminal — M9 adds the flow for a machine with "+
			"neither.)", ErrNoTerminal, passwordEnvVar)
	}

	password, err := r.ReadSecret("Password: ")
	if err != nil {
		return "", err
	}
	if password == "" {
		// Refused here rather than sent: an empty password cannot be right, and the instance would answer
		// with the same deliberately vague 401 it gives a wrong one, which reads as "your password is
		// wrong" to someone who simply pressed Enter too early.
		return "", errors.New("a password is required")
	}
	return password, nil
}

func (r *Runner) resolveDeviceName(previous credentials.Record) string {
	if name := strings.TrimSpace(r.Options.DeviceName); name != "" {
		return truncateDeviceName(name)
	}
	if previous.DeviceName != "" {
		return previous.DeviceName
	}

	host, err := r.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		// Not worth failing a login over. An unnamed device is a cosmetic problem in a session list; a
		// login that refuses because the machine has no hostname is not.
		return "this device"
	}
	return truncateDeviceName(host)
}

// maxDeviceName matches the backend's own limit, so an over-long name is trimmed here rather than rejected
// after the password has already crossed the network.
const maxDeviceName = 64

func truncateDeviceName(name string) string {
	runes := []rune(name)
	if len(runes) <= maxDeviceName {
		return name
	}
	return string(runes[:maxDeviceName])
}

func (r *Runner) client(baseURL string) *client {
	if r.newClient != nil {
		return r.newClient(baseURL)
	}
	return newClient(baseURL)
}

// printf writes to the output stream.
//
// The write error is dropped once, here, rather than at each call site — the same justification the
// instance wizard's prompter documents, and what keeps errcheck satisfied without a nolint per line.
func (r *Runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.Out, format, args...)
}

// terminalReaders builds the real terminal's readers.
func terminalReaders(in *os.File, out io.Writer) (readLine func(string) (string, error),
	readSecret func(string) (string, error), interactive bool,
) {
	fd := int(in.Fd())
	interactive = term.IsTerminal(fd)

	readLine = lineReader(in, out)

	readSecret = func(prompt string) (string, error) {
		_, _ = fmt.Fprint(out, prompt)
		b, err := term.ReadPassword(fd)
		// The newline the terminal did not echo, so the next line does not begin on the prompt.
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	return readLine, readSecret, interactive
}

// lineReader reads one visible line at a time from in.
//
// A buffered line read rather than fmt.Fscanln, which was the first thing here and was wrong twice over: on
// an empty line it fails with "unexpected newline" instead of returning one, so pressing Enter at the
// prompt produced a scanner error rather than "an email address is required"; and it stops at the first
// space, so a mistyped address silently became its first word. The same reader the instance wizard uses,
// for the same reasons.
func lineReader(in io.Reader, out io.Writer) func(string) (string, error) {
	buffered := bufio.NewReader(in)
	return func(prompt string) (string, error) {
		_, _ = fmt.Fprint(out, prompt)

		line, err := buffered.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		// EOF with content on the line is a final answer, not a failure — a heredoc with no trailing
		// newline is an ordinary way to drive this.
		if err != nil && strings.TrimSpace(line) == "" {
			return "", errors.New("no input: stdin ended before an answer was given")
		}
		return strings.TrimSpace(line), nil
	}
}
