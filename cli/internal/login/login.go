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

// ErrNoTerminal is returned when a password is needed and there is nowhere to ask for one.
//
// The same shape as instanceinit.ErrNotATerminal, and for the same reason: a command that blocks forever on
// input nobody is there to type, or reads EOF and treats an empty password as an answer, is worse than one
// that says what it needs. `main` maps it to exit code 2.
var ErrNoTerminal = errors.New(
	"a password is required and this is not an interactive terminal; set " + passwordEnvVar +
		" and re-run, or run this from a terminal")

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
	// NewDeviceID mints an identifier for a first login on this installation.
	NewDeviceID func() (string, error)

	// newClient builds the API client. Indirected for tests; production leaves it nil.
	newClient func(baseURL string) *client
}

// Run performs the login and stores the result.
func (r *Runner) Run(ctx context.Context) error {
	instanceURL, err := r.resolveInstance()
	if err != nil {
		return err
	}

	// Said before the password is asked for, never after: the point of the warning is to let someone stop.
	if looksLikeHTTP(instanceURL) {
		r.printf("Warning: %s is plain HTTP, so your password and token cross the network unencrypted.\n",
			instanceURL)
	}

	email, err := r.resolveEmail()
	if err != nil {
		return err
	}
	password, err := r.resolvePassword()
	if err != nil {
		return err
	}

	deviceID, err := r.resolveDeviceID()
	if err != nil {
		return err
	}
	deviceName, err := r.resolveDeviceName()
	if err != nil {
		return err
	}

	api := r.client(instanceURL)
	pair, err := api.login(ctx, loginRequest{
		Email:      email,
		Password:   password,
		DeviceID:   deviceID,
		DeviceName: deviceName,
	})
	if err != nil {
		return err
	}

	// Who the token belongs to, from the instance rather than from what was typed: an email address is how
	// someone signs in, and a username is how everyone else sees them. A failure here is not fatal — the
	// session is valid either way — so it degrades to showing the address instead.
	record := credentials.Record{
		InstanceURL: instanceURL,
		Username:    email,
		DeviceID:    deviceID,
		DeviceName:  deviceName,
	}
	if who, err := api.me(ctx, pair.AccessToken); err == nil {
		record.UserID = who.ID
		record.Username = who.Username
	}

	// Only the refresh token is kept. The access token in hand expires in fifteen minutes and the daemon
	// will mint its own; writing it down would add a second credential at rest to buy nothing.
	if err := r.Store.Save(record, pair.RefreshToken); err != nil {
		return err
	}

	r.printf("Signed in as %s on %s.\n", record.Username, instanceURL)
	r.printf("This device is %q; its credential is stored in %s.\n", deviceName, r.Store.SecretLocation())
	r.printf("Start the background daemon with `norite daemon start` if it is not running already.\n")
	return nil
}

// resolveInstance decides which instance to log in to.
//
// Flag, then environment, then the instance a previous login recorded. The last is what makes re-logging-in
// after a password change a single word rather than a URL someone has to remember.
func (r *Runner) resolveInstance() (string, error) {
	if r.Options.Instance != "" {
		return credentials.ParseInstanceURL(r.Options.Instance)
	}
	if fromEnv := strings.TrimSpace(os.Getenv(instanceEnvVar)); fromEnv != "" {
		return credentials.ParseInstanceURL(fromEnv)
	}

	record, err := r.Store.LoadRecord()
	switch {
	case err == nil && record.InstanceURL != "":
		return record.InstanceURL, nil
	case err != nil && !errors.Is(err, credentials.ErrNoCredential):
		return "", err
	}

	return "", fmt.Errorf(
		"no instance to log in to: pass --instance https://chat.example.com, or set %s", instanceEnvVar)
}

func (r *Runner) resolveEmail() (string, error) {
	if email := strings.TrimSpace(r.Options.Email); email != "" {
		return email, nil
	}
	if !r.Interactive {
		return "", errors.New("no email address given: pass --email, or run this from a terminal")
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
		return "", ErrNoTerminal
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

// resolveDeviceID keeps this installation's existing identifier if there is one.
//
// A new ID on every login would strand the previous refresh-token family until it expired, and would fill
// the account's session list with one entry per login. The ID is per installation, not per session
// (ADR 0011).
func (r *Runner) resolveDeviceID() (string, error) {
	record, err := r.Store.LoadRecord()
	if err == nil && record.DeviceID != "" {
		return record.DeviceID, nil
	}
	if err != nil && !errors.Is(err, credentials.ErrNoCredential) {
		return "", err
	}
	return r.NewDeviceID()
}

func (r *Runner) resolveDeviceName() (string, error) {
	if name := strings.TrimSpace(r.Options.DeviceName); name != "" {
		return truncateDeviceName(name), nil
	}
	if record, err := r.Store.LoadRecord(); err == nil && record.DeviceName != "" {
		return record.DeviceName, nil
	}

	host, err := r.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		// Not worth failing a login over. An unnamed device is a cosmetic problem in a session list; a
		// login that refuses because the machine has no hostname is not.
		return "this device", nil
	}
	return truncateDeviceName(host), nil
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
