// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/Alexnex31/Norite/backend/apicontract"
	"github.com/Alexnex31/Norite/cli/internal/apiclient"
	"github.com/Alexnex31/Norite/cli/internal/clierr"
	"github.com/Alexnex31/Norite/cli/internal/daemonclient"
	"github.com/Alexnex31/Norite/cli/internal/ops"
	"github.com/Alexnex31/Norite/cli/internal/output"
	"github.com/Alexnex31/Norite/daemon/credentials"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// `norite setup` (easier-setup): the first run, from nothing to a guild, as one guided command that can be
// run again. It was a shell function in install.sh and a PowerShell one in install.ps1, and nothing at all
// after a package install. Each step it finds done already it says and skips.
//
// It is `norite register`, `norite login` and the daemon's offer, in that order, and the steps are theirs:
// what it adds is the order, the instance named once, a password chosen at registration used for the
// sign-in rather than asked again, and an invite link taken whole. It lives in this package for that
// reason, so it calls those flows rather than copies of them.

// SetupOptions is what `norite setup` was asked to do.
type SetupOptions struct {
	// Invite is the invite link, or a code with Instance, given as the command's argument. Empty means no
	// guild to join.
	Invite string
	// Instance is --instance. A link names its own, and the two must agree.
	Instance string
	// Email is --email, for whichever of registering and signing in is done.
	Email string
	// InviteCode is --invite-code: the instance's own invite code, which registering on an instance that
	// takes accounts by invite needs. Asked for when the instance says so and this is empty.
	InviteCode string
	// Provider and DeviceCode pass through to the sign-in, as `norite login` takes them.
	Provider   string
	DeviceCode bool
	// NoDaemon leaves the daemon alone.
	NoDaemon bool
}

// Connect attaches to this account's daemon, as verbs.Daemon does. The function it returns detaches.
type Connect func(ctx context.Context) (daemonclient.Caller, func(), error)

// Setup performs a `norite setup`. Its dependencies are injected for the tests, as Runner's are.
type Setup struct {
	Options SetupOptions

	Store       *credentials.Store
	Out         io.Writer
	ReadLine    func(prompt string) (string, error)
	ReadSecret  func(prompt string) (string, error)
	Interactive bool
	Hostname    func() (string, error)

	// Daemon is the daemon's service. Connect reaches the daemon itself, for the invite.
	Daemon  Daemon
	Connect Connect

	// newClient, browserReachable and openBrowser reach the sign-in's own seams. Nil in production.
	newClient        func(baseURL string) *client
	browserReachable func() bool
	openBrowser      func(ctx context.Context, target string) error
	// pollEvery and waitFor bound the wait for the daemon to take the sign-in up. Zero means the defaults.
	pollEvery, waitFor time.Duration
	// unanswered is the account step having said why no daemon answered, which the join then does not say
	// again (easier-setup's manual pass: the same four lines twice).
	unanswered bool
}

// signInAttempts is how many times a sign-in straight after registering is tried: once, then again after
// the person has had the chance to open a confirmation link. Each refused try of a correct password on an
// unconfirmed account mails a fresh link (M10), so the count is bounded, and low.
const signInAttempts = 3

// inviteCodeAttempts is how many times the instance's own invite code is asked for, when the instance asks
// for one or refuses the one given.
const inviteCodeAttempts = 3

// daemonWait is how long the daemon is given to read a new sign-in and say so: it watches the credential
// store and reacts within a moment, so this is generous.
const daemonWait = 20 * time.Second

// Run performs the setup.
func (s *Setup) Run(ctx context.Context) error {
	// What was typed is checked first, so a mistake in it is reported as one wherever this runs.
	invite, flag, err := s.arguments()
	if err != nil {
		return err
	}
	if !s.Interactive {
		return fmt.Errorf("%w: from a script, `norite register`, `norite login` and `norite daemon "+
			"install` do setup's steps one at a time", ErrNoTerminal)
	}
	previous, err := s.previous()
	if err != nil {
		return err
	}
	instance, err := s.instance(invite, flag, previous)
	if err != nil {
		return err
	}

	// Signed in to it already, the instance is the one stored, written as it was stored: a link may write it
	// another way, with a final dot or its scheme's port, and the comparison that calls the two one instance
	// is not a reason to send a password to the link's spelling unasked, which with a final dot may be
	// resolved to another server. Re-parsed, as resolveInstanceURL does, since the record is a file.
	signedIn := false
	if storedAt, err := credentials.ParseInstanceURL(previous.InstanceURL); err == nil && ipc.SameInstance(storedAt, instance) {
		signedIn, instance = true, storedAt
	}

	s.printf("Setting up Norite on this machine, for %s.\n", instance)
	if apiclient.LooksLikeHTTP(instance) {
		s.printf("Warning: %s is plain HTTP, so everything this sends — including your password —\n"+
			"crosses the network unencrypted.\n", instance)
	}
	if invite.Instance != "" && !signedIn {
		// A link is a stranger's text, and it decides where a password is about to go. Said before anything
		// is asked or sent, so the address can be read and the run stopped.
		if !s.yes(fmt.Sprintf("The invite is for %s: your account and password go there. Is that the "+
			"instance you meant? [Y/n] ", instance), true) {
			return errors.New("stopped; nothing was sent to " + instance)
		}
	}

	// With a sign-in stored for this instance, the daemon is what can say whether the instance still accepts
	// it, so it comes up first. Otherwise the account comes first, as `norite login` offers the daemon after.
	stored := signedIn && previous.Username != ""
	var daemon daemonUp
	if stored {
		daemon = s.daemonStep(ctx)
	}
	s.step("Your account")
	confirmed, err := s.account(ctx, previous, instance, signedIn, stored, daemon)
	if err != nil {
		return err
	}
	if !stored {
		daemon = s.daemonStep(ctx)
	}

	if invite.Code != "" {
		s.step("The guild")
		// A bare code names no instance, and is for the one set up for: either way the daemon sends it there
		// or nowhere.
		if invite.Instance == "" {
			invite.Instance = instance
		}
		if err := s.join(ctx, invite, daemon, confirmed); err != nil {
			return err
		}
	}

	s.step("Done")
	s.printf("Run `norite` to open Norite, and `norite --help` for everything else.\n")
	return nil
}

// previous reads the stored record, treating "nothing stored yet" as an empty one, as a login does.
func (s *Setup) previous() (credentials.Record, error) { return loadPrevious(s.Store) }

// arguments reads the invite and --instance, and refuses the two disagreeing. flag is "" without one.
func (s *Setup) arguments() (invite ops.Invite, flag string, err error) {
	// Refused as `norite login` refuses them: --email belongs to a password's sign-in. And a provider or a
	// device code makes the account in the browser, where an instance invite code given here never goes.
	// The provider first, as `norite login` checks it: a typo is reported before anything is asked.
	if err := checkProvider(s.Options.Provider); err != nil {
		return ops.Invite{}, "", clierr.Usage("--provider: %s", err)
	}
	browser := s.Options.Provider != "" || s.Options.DeviceCode
	switch {
	case s.Options.Provider != "" && s.Options.Email != "":
		return ops.Invite{}, "", clierr.Usage("--email is for signing in with a password; --provider does not use one")
	case s.Options.DeviceCode && s.Options.Email != "":
		return ops.Invite{}, "", clierr.Usage("--email is for signing in with a password here; --device-code " +
			"asks in a browser on another device")
	case browser && s.Options.InviteCode != "":
		return ops.Invite{}, "", clierr.Usage("--invite-code is for creating an account here; with --provider " +
			"or --device-code the account is made in the browser, which asks for the code itself")
	}
	if s.Options.Invite != "" {
		if invite, err = ops.ParseInvite(s.Options.Invite); err != nil {
			return ops.Invite{}, "", clierr.Usage("%s", err)
		}
	}
	if s.Options.Instance != "" {
		if flag, err = credentials.ParseInstanceURL(s.Options.Instance); err != nil {
			return ops.Invite{}, "", clierr.Usage("--instance: %s", err)
		}
		if invite.Instance != "" && !ipc.SameInstance(invite.Instance, flag) {
			return ops.Invite{}, "", clierr.Usage("the invite is for %s, and --instance names %s",
				termsafe.Text(invite.Instance), termsafe.Text(flag))
		}
	}
	return invite, flag, nil
}

// instance is the instance to set up for: --instance, the link's, NORITE_INSTANCE, the last login's, or
// asked. --instance comes before the link, which arguments has held to the same instance: what the person
// typed is their own spelling, and a link's may write it another way, with a final dot that a resolver can
// send elsewhere.
func (s *Setup) instance(invite ops.Invite, flag string, previous credentials.Record) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if invite.Instance != "" {
		return invite.Instance, nil
	}
	if url, err := resolveInstanceURL("", previous); err == nil {
		return url, nil
	} else if strings.TrimSpace(os.Getenv(instanceEnvVar)) != "" {
		// Named and unusable: said, rather than asked over. A blank variable names nothing, as
		// resolveInstanceURL reads it.
		return "", err
	}
	answer, err := s.ReadLine("Instance address, such as https://chat.example.com: ")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(answer) == "" {
		return "", errors.New("an instance address is required: the person who invited you, " +
			"or the instance's administrator, has it")
	}
	return credentials.ParseInstanceURL(answer)
}

// account signs this machine in to instance, registering first if the person has no account there.
// signedIn is the stored record being for instance, and stored is that record naming an account too, which
// Run decided and brought the daemon up first for: one decision, made there. daemon is where the daemon step
// left the daemon. confirmed is the daemon having just said the instance accepts the stored sign-in.
func (s *Setup) account(ctx context.Context, previous credentials.Record, instance string, signedIn, stored bool,
	daemon daemonUp) (confirmed bool, err error) {
	if stored {
		who := termsafe.Text(previous.Username)
		switch standing, why, next := s.standing(ctx, instance, previous.UserID, daemon); standing {
		case signInAccepted:
			s.printf("Signed in as @%s already.\n", who)
			return true, nil
		case signInUnknown:
			// Nothing says it was refused, so it is not replaced: a sign-in done again for nothing would ask
			// for a password, or a browser, for a sign-in that may be perfectly good.
			s.printf("Signed in as @%s, as far as this machine knows: %s, so whether the instance still accepts "+
				"it is not known. %s\n", who, why, next)
			return false, nil
		}
		// The account exists: what is missing is a sign-in the instance accepts.
		s.printf("This machine holds a sign-in as @%s, and the daemon says the instance no longer accepts it. "+
			"Signing in again.\n", who)
		if s.Options.Provider == "" && !s.Options.DeviceCode {
			// Nothing stored says how the account signs in, and the password is the default.
			s.printf("If @%s signs in through Google or GitHub, press Ctrl-C and run `norite setup "+
				"--provider google` or `--provider github` instead.\n", who)
		}
		return false, s.runner(instance, s.Options.Email, "").Run(ctx)
	}
	if previous.InstanceURL != "" && !signedIn {
		// One daemon holds one sign-in, so signing in here signs this machine out of the other instance.
		who := ""
		if previous.Username != "" {
			who = " as @" + termsafe.Text(previous.Username)
		}
		if !s.yes(fmt.Sprintf("This machine is signed in to %s%s, and holds one sign-in at a time. Sign "+
			"in to %s instead? [y/N] ", termsafe.Text(previous.InstanceURL), who, instance), false) {
			return false, errors.New("stopped; this machine is still signed in to " + termsafe.Text(previous.InstanceURL))
		}
	}
	// A provider's sign-in creates the account in the browser when there is none, and a device code is
	// completed in one, so neither has a registration to do here.
	if s.Options.Provider != "" || s.Options.DeviceCode ||
		s.yes("Do you already have an account on "+instance+"? [y/N] ", false) {
		return false, s.runner(instance, s.Options.Email, "").Run(ctx)
	}
	return false, s.register(ctx, instance)
}

// register creates the account, then signs in to it with the password just chosen.
func (s *Setup) register(ctx context.Context, instance string) error {
	r := &Registrar{
		Options: RegisterOptions{Instance: instance, Email: s.Options.Email}, Out: s.Out,
		ReadLine: s.ReadLine, ReadSecret: s.ReadSecret, Interactive: s.Interactive, warned: true,
		newClient: s.newClient,
	}
	reg, err := r.collect()
	if err != nil {
		return err
	}
	message, err := r.submit(ctx, reg, s.Options.InviteCode)
	// The instance's own code: asked for when it says one is needed, and again when it refuses the one given.
	// The answers are sent again as they were, not asked again.
	for asked := 0; errors.Is(err, errInviteRequired) || errors.Is(err, errInviteInvalid); asked++ {
		if asked == inviteCodeAttempts {
			return errors.New("the instance did not accept an instance invite code; nothing was created")
		}
		if errors.Is(err, errInviteRequired) {
			s.printf("%s takes new accounts by invite. Its administrator makes the codes, and a guild "+
				"invite is not one.\n", instance)
		} else {
			s.printf("%s.\n", errInviteInvalid)
		}
		var code string
		if code, err = s.ReadLine("Instance invite code: "); err != nil {
			return err
		}
		if strings.TrimSpace(code) == "" {
			return errors.New("no instance invite code given; nothing was created")
		}
		message, err = r.submit(ctx, reg, code)
	}
	if err != nil {
		return err
	}
	s.printf("%s\n", message)

	// The instance's sentence says whether it emailed a confirmation link, and is prose rather than a
	// signal. A sign-in tried before the link is opened is refused, and the refusal mails a new link and
	// spends the one just sent (M10), so the first try waits for Enter rather than spending it unseen.
	//
	// Quoted, as a command to paste: an address in brackets is a pattern to zsh, and an address is read from
	// a person or a link, so it may hold what a shell reads.
	login := fmt.Sprintf("`norite login --instance %s --email %s`", output.ShellWord(instance),
		output.ShellWord(termsafe.Text(reg.email)))
	if _, err := s.ReadLine("Press Enter to sign in, once you have opened the link if it emailed you one: "); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := s.runner(instance, reg.email, reg.password).Run(ctx)
		if err == nil || !errors.Is(err, ErrBadCredentials) {
			return err
		}
		// Refused: the address is not confirmed yet, or it already had an account, whose own password this is
		// not. The instance answers the two alike, deliberately (M10). The account exists either way, so the
		// way on is a sign-in, never `norite setup` again, which would offer to create it.
		if attempt == signInAttempts {
			return errors.New("the instance still refuses the sign-in. If it emailed you a link, open the " +
				"newest one, then sign in with " + login + "; if that address already had an account, the same " +
				"command signs in with its own password")
		}
		s.printf("\nThe instance did not take the sign-in. If it emails links, it has just sent a new one and "+
			"the earlier ones no longer work: open the newest, then press Enter here. If that address already "+
			"had an account, its own password is the one that signs in: press Ctrl-C and run %s.\n", login)
		if _, err := s.ReadLine("Press Enter to try again: "); err != nil {
			return err
		}
	}
}

// signInStanding is what the daemon says of a stored sign-in.
type signInStanding int

const (
	// signInUnknown: no daemon answered, or it could not ask the instance.
	signInUnknown signInStanding = iota
	// signInAccepted: the daemon is signed in to the instance, which answered it.
	signInAccepted
	// signInRefused: the daemon is signed out, which with a sign-in stored means the instance refused it.
	signInRefused
)

// standing asks the daemon whether the sign-in it holds for instance, as the account userID, is one the
// instance accepts, by asking the instance who it is signed in as. A daemon still starting is waited for by
// the relay, as for any request. When the standing is unknown, why says why, and next where to find out.
//
// Only the record's own account is accepted, so a record naming none, which a sign-in whose
// `GET /users/@me` failed writes, is not known either way. And signed out is refused only once it has
// lasted a moment (settle): the relay answers so for a sign-in the instance refused, and also for one a
// `norite login` stored a moment ago that the daemon has not read, and for one that changed during the
// probe, neither of which a fresh sign-in should replace.
func (s *Setup) standing(ctx context.Context, instance, userID string, daemon daemonUp) (standing signInStanding,
	why, next string) {
	const daemonNext = "`norite daemon status` says, once the daemon runs."
	if userID == "" {
		return signInUnknown, "this machine's record does not say which account it is",
			"`norite login` signs in again and says."
	}
	if daemon == daemonDown || s.Connect == nil {
		return signInUnknown, "the daemon is not running", daemonNext
	}
	c, detach, err := s.attach(ctx, daemon, time.Now().Add(s.wait()))
	if err != nil {
		s.unanswered = true
		return signInUnknown, termsafe.Text(err.Error()), daemonNext
	}
	defer detach()
	// The daemon answered, so what is not known is the instance's mind, which running setup again asks.
	const instanceNext = "Running `norite setup` again asks again."
	settled := time.Now().Add(s.settle())
	for {
		res, err := probe(ctx, c, instance)
		id := ""
		if err == nil {
			id = accountIn(res)
		}
		var re *ipc.RelayError
		switch {
		case err == nil && res.Status == http.StatusUnauthorized:
			// Refused again after the relay's renewal: the session is ending it.
			return signInRefused, "", ""
		case id == userID:
			return signInAccepted, "", ""
		case id != "":
			// Signed in to this instance as somebody else than the record names: a daemon that has not read
			// the store since another `norite login`.
			return signInUnknown, "the daemon is signed in to it as another account than this machine's " +
				"record names", "`norite daemon status` says which."
		case err == nil:
			// A 503 while the instance migrates, a 429, a proxy's 502, or a 200 that is not the instance's
			// answer, such as a captive portal's page: answered before the token is looked at, so they say
			// nothing of the sign-in.
			return signInUnknown, answeredWith(res), instanceNext
		case errors.As(err, &re) && re.Code == ipc.RelayNotSignedIn:
			if time.Now().Before(settled) {
				if err := s.pause(ctx); err != nil {
					return signInUnknown, termsafe.Text(err.Error()), instanceNext
				}
				continue
			}
			return signInRefused, "", ""
		}
		return signInUnknown, "the daemon could not ask the instance (" + termsafe.Text(err.Error()) + ")",
			instanceNext
	}
}

// settle is how long a daemon answering signed out is asked again before standing takes it for a refusal:
// long enough for it to read a sign-in stored a moment ago, which it is told of at once, and no longer than
// the wait for the daemon itself.
func (s *Setup) settle() time.Duration { return min(s.wait(), 3*time.Second) }

// accountIn is the id of the account res says the daemon is signed in as, or "" for an answer carrying
// none.
func accountIn(res ipc.Result) string {
	if res.Status < 200 || res.Status >= 300 || res.Body == nil {
		return ""
	}
	var me struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(res.Body, &me) != nil {
		return ""
	}
	return me.ID
}

// answeredWith says what the instance answered when it was not who the daemon is signed in as.
func answeredWith(res ipc.Result) string {
	if res.Status >= 200 && res.Status < 300 {
		return fmt.Sprintf("the instance answered HTTP %d with no account in it", res.Status)
	}
	return fmt.Sprintf("the instance answered HTTP %d", res.Status)
}

// probeTimeout bounds asking the instance who the daemon is signed in as: past the relay's own wait for a
// daemon still starting, and the request.
const probeTimeout = 45 * time.Second

// probe asks the instance, through the daemon behind c and only if it is signed in to instance, who it is
// signed in as: how setup learns whether the instance accepts a sign-in, and whether the daemon has taken
// one up. Refused on this machine, sending nothing, when the daemon is signed out or signed in elsewhere.
func probe(ctx context.Context, c daemonclient.Caller, instance string) (ipc.Result, error) {
	ask, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return daemonclient.DoFor(ask, c, instance, http.MethodGet, "/users/@me", nil)
}

// runner is a sign-in to instance as one of setup's steps.
func (s *Setup) runner(instance, email, password string) *Runner {
	return &Runner{
		Options: Options{Instance: instance, Email: email, Provider: s.Options.Provider,
			DeviceCode: s.Options.DeviceCode},
		Store: s.Store, Out: s.Out, ReadLine: s.ReadLine, ReadSecret: s.ReadSecret, Interactive: s.Interactive,
		Hostname: s.Hostname, password: password, warned: true, skipDaemon: true,
		newClient: s.newClient, browserReachable: s.browserReachable, openBrowser: s.openBrowser,
	}
}

// daemonUp is where setup left the daemon, as far as the join is concerned.
type daemonUp int

const (
	// daemonDown: none is coming, because it was declined or failed; the join is left for later.
	daemonDown daemonUp = iota
	// daemonMaybe: one may be answering, or about to: just installed or started, or a service manager that
	// could not be asked. The socket is asked again for a bounded while until it answers.
	daemonMaybe
	// daemonAlone: left alone, as asked. One may be running, started by hand, so the socket is asked once,
	// and nothing waits for a daemon nobody is starting.
	daemonAlone
)

// daemonStep is setup's step for the daemon, or the note that it was left alone.
func (s *Setup) daemonStep(ctx context.Context) daemonUp {
	if s.Options.NoDaemon {
		s.printf("Leaving the daemon alone, as asked.\n")
		return daemonAlone
	}
	s.step("The daemon")
	return s.daemon(ctx)
}

// daemon brings the daemon up, asking first.
//
// Asked with yes as the default, where `norite login` asks with no: running `norite setup` is asking to be
// set up, and the question is still put, so nothing is installed unasked (M23).
func (s *Setup) daemon(ctx context.Context) daemonUp {
	if s.Daemon == nil {
		s.printf("Start the daemon with `norite daemon install`, then `norite daemon start`.\n")
		return daemonMaybe
	}
	switch s.Daemon.State(ctx) {
	case DaemonRunning:
		s.printf("The daemon is running.\n")
		return daemonMaybe
	case DaemonStopped:
		if !s.yes("The daemon is installed and not running. Start it? [Y/n] ", true) {
			s.printf("Start it with `norite daemon start`.\n")
			return daemonDown
		}
		if err := s.Daemon.Start(ctx); err != nil {
			s.printf("The daemon could not be started: %s\n", termsafe.Text(err.Error()))
			return daemonDown
		}
		s.printf("The daemon is running.\n")
		return daemonMaybe
	case DaemonAbsent:
		s.printf("The daemon is the program that keeps you connected. Once installed it starts each time you " +
			"log in to this computer, as you, and needs no administrator rights.\n")
		if !s.yes("Install and start it? [Y/n] ", true) {
			s.printf("When you want it: `norite daemon install`, then `norite daemon start`.\n")
			return daemonDown
		}
		if err := s.Daemon.Install(ctx, s.Out); err != nil {
			s.printf("The daemon could not be installed: %s\n", termsafe.Text(err.Error()))
			return daemonDown
		}
		// Installed and started, though the service manager may not say so yet: the join's wait covers it.
		return daemonMaybe
	default:
		s.printf("Start the daemon with `norite daemon start` if it is not running already.\n")
		return daemonMaybe
	}
}

// join joins the guild the invite leads to through the daemon, which sends its code to the invite's
// instance or nowhere (daemonclient.CallFor): once it has taken up the sign-in to that instance, which it does
// a moment after one is stored. confirmed is the daemon having just answered as signed in there, so there is
// nothing to wait for.
func (s *Setup) join(ctx context.Context, invite ops.Invite, daemon daemonUp, confirmed bool) error {
	// Written as it was checked, not as it was typed (ops.Invite.Canonical), and for a bare code as a link to
	// the instance set up for, which Run named it. Quoted, since a link holds a '#', which some shells read.
	link := output.ShellWord(termsafe.Text(invite.Canonical()))
	// Said once the daemon runs when it is the daemon that is missing, the attachment included, and later
	// when it answered and the wait for a sign-in the instance takes ran out.
	once := "Join it once the daemon runs: `norite invite join " + link + "`."
	later := "Join it later: `norite invite join " + link + "`."
	if daemon == daemonDown || s.Connect == nil {
		s.printf("%s\n", once)
		return nil
	}
	deadline := time.Now().Add(s.wait())
	c, detach, err := s.attach(ctx, daemon, deadline)
	switch {
	case err != nil && s.unanswered:
		s.printf("%s\n", once)
		return nil
	case err != nil:
		s.printf("%s. %s\n", termsafe.Text(err.Error()), once)
		return nil
	}
	defer detach()
	if !confirmed {
		// The account the record names now, which the sign-in this run stored, if it stored one, wrote. A
		// record that cannot be read, or names no account, which a sign-in whose `GET /users/@me` failed
		// writes, cannot be told from another account the daemon still holds, so nothing is joined.
		rec, err := s.previous()
		if err == nil && rec.UserID == "" {
			err = errors.New("this machine's sign-in does not say which account it is; `norite login` signs " +
				"in again and says")
		}
		if err == nil {
			err = s.awaitSignIn(ctx, c, invite.Instance, rec.UserID, deadline)
		}
		switch {
		case errors.Is(err, errDaemonGone):
			s.printf("%s. %s\n", termsafe.Text(err.Error()), once)
			return nil
		case err != nil:
			s.printf("%s. %s\n", termsafe.Text(err.Error()), later)
			return nil
		}
	}

	p, err := ops.PreviewInvite(ctx, c, invite)
	if err != nil {
		return fmt.Errorf("looking the invite up: %w", err)
	}
	// A guild joined already is said and skipped, as every other step done already is: redeeming again
	// spends nothing, and answers as a first join does. A list that cannot be read asks the question.
	if in, err := ops.InGuild(ctx, c, invite.Instance, p.Guild.Id); err == nil && in {
		s.printf("Already in %s.\n", termsafe.Text(p.Guild.Name))
		return nil
	}
	s.printf("%s\n", describePreview(p))
	if !s.yes("Join it? [Y/n] ", true) {
		s.printf("Not joined. Later: `norite invite join %s`.\n", link)
		return nil
	}
	g, err := ops.JoinInvite(ctx, c, invite)
	if err != nil {
		return fmt.Errorf("joining: %w", err)
	}
	s.printf("Joined %s.\n", termsafe.Text(g.Name))
	return nil
}

// wait is how long the daemon is given to answer and to take a sign-in up.
func (s *Setup) wait() time.Duration {
	if s.waitFor != 0 {
		return s.waitFor
	}
	return daemonWait
}

// every is how often it is asked again meanwhile.
func (s *Setup) every() time.Duration {
	if s.pollEvery != 0 {
		return s.pollEvery
	}
	return 250 * time.Millisecond
}

// attach attaches to the daemon: until deadline when one is coming, and once when it was left alone. It
// returns the attachment and its detach.
func (s *Setup) attach(ctx context.Context, daemon daemonUp, deadline time.Time) (daemonclient.Caller, func(), error) {
	for {
		c, detach, err := s.Connect(ctx)
		if err == nil {
			return c, detach, nil
		}
		// The attach's own error, kept: a version to restart, or a socket in another state directory, says
		// more than that nothing answered.
		if daemon == daemonAlone {
			return nil, nil, fmt.Errorf("no daemon answered (%w)", err)
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("the daemon could not be reached within %s: %w", s.wait(), err)
		}
		if err := s.pause(ctx); err != nil {
			return nil, nil, err
		}
	}
}

// awaitSignIn waits until the daemon behind c is signed in to instance as userID, asked by a request that
// names it.
// Until the daemon has read the sign-in setup stored, it answers that it is signed out, or signed in to the
// instance it was before, and either answer is given on this machine, sending nothing; then, while it has
// no session from the instance yet, that it cannot reach it. Once it has one, the request reaches the
// instance. One attachment, asked again, rather than an attach each time: the handshake is the daemon's,
// and a request it refuses costs it nothing.
//
// An answer from the instance that is not the account, a 503 while it migrates, a 429 or another account,
// is waited out too. A sign-in the instance refuses is not told apart from one not read yet: the relay
// renews on a 401, and answers a renewal refused as signed out, so the message names both. A 401 itself
// comes back only when a renewed sign-in is refused again, and ends the wait.
//
// Anything else is returned, for the join to say and leave for later: the attachment ending, which is
// errDaemonGone, or a probe that timed out. What is said names no length of time, since each probe may
// outlast the deadline. userID is not "": an account nobody named cannot be told from another.
func (s *Setup) awaitSignIn(ctx context.Context, c daemonclient.Caller, instance, userID string,
	deadline time.Time) error {
	for {
		res, err := probe(ctx, c, instance)
		id := ""
		if err == nil {
			id = accountIn(res)
		}
		var re *ipc.RelayError
		// What the wait ends on, past the deadline: the last thing heard.
		var last error
		switch {
		case id == userID:
			return nil
		case err == nil && res.Status == http.StatusUnauthorized:
			return errors.New("the instance refused the daemon's sign-in; `norite login` signs in again")
		case id != "":
			last = errors.New("stopped waiting: the daemon is still signed in to the instance as another " +
				"account; `norite daemon status` says which")
		case err == nil:
			// A 503 while the instance migrates, a 429, a portal's 200: passing, maybe, so waited out.
			last = errors.New("stopped waiting: " + answeredWith(res))
		case !errors.As(err, &re):
			return fmt.Errorf("%w: %s", errDaemonGone, termsafe.Text(err.Error()))
		case re.Code == ipc.RelayUnreachable:
			// Said as the relay says it, which already names the instance: "could not reach the instance: …".
			last = errors.New("stopped waiting: " + termsafe.Text(re.Message))
		case re.Code == ipc.RelayNotSignedIn:
			last = errors.New("stopped waiting: the daemon is not signed in, having not read the sign-in stored " +
				"on this machine yet or had it refused by the instance; `norite daemon status` says which")
		case re.Code == ipc.RelayOtherInstance:
			last = errors.New("stopped waiting: the daemon is still signed in to another instance; " +
				"`norite daemon status` says which")
		default:
			return fmt.Errorf("the daemon could not ask the instance: %s", termsafe.Text(re.Message))
		}
		if time.Now().After(deadline) {
			return last
		}
		if err := s.pause(ctx); err != nil {
			return err
		}
	}
}

// errDaemonGone is the attachment ending under the join's wait: the daemon to start again, not the
// instance to wait for.
var errDaemonGone = errors.New("the daemon could not be asked")

// pause waits one interval, or until ctx is done.
func (s *Setup) pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.every()):
		return nil
	}
}

// describePreview is where an invite leads, as `norite invite show` says it: the guild, its description, the
// channel, the inviter and the expiry. Every value but the expiry is a stranger's text and passes termsafe
// (rule 19).
func describePreview(p apicontract.GuildInvitePreview) string {
	line := "The invite leads to " + termsafe.Text(p.Guild.Name)
	if p.Channel.Name != nil {
		line += ", #" + termsafe.Text(*p.Channel.Name)
	}
	if p.Inviter != nil {
		line += ", invited by " + termsafe.Text(p.Inviter.DisplayName) + " (@" + termsafe.Text(p.Inviter.Username) + ")"
	}
	if p.ExpiresAt != nil {
		line += ", and expires " + p.ExpiresAt.Local().Format("2006-01-02 15:04")
	}
	line += "."
	if p.Guild.Description != nil && *p.Guild.Description != "" {
		line += "\n  " + termsafe.Text(*p.Guild.Description)
	}
	return line
}

func (s *Setup) step(name string) { s.printf("\n%s\n", name) }

func (s *Setup) printf(format string, args ...any) { _, _ = fmt.Fprintf(s.Out, format, args...) }

// yes asks a question; an empty answer is byDefault, and a read that fails is no.
func (s *Setup) yes(question string, byDefault bool) bool {
	return yes(s.ReadLine, question, byDefault)
}

// SetupCommand builds `norite setup`.
func SetupCommand(daemon Daemon, connect Connect) *cli.Command {
	return &cli.Command{
		Name:      "setup",
		Usage:     "Set Norite up on this machine: your account, the daemon, and a guild to join",
		ArgsUsage: "[invite-link]",
		Description: "Takes you from a fresh install to a conversation, asking as it goes: which instance,\n" +
			"whether you have an account there (and creates one if not), the background daemon, and the\n" +
			"guild an invite link leads to. Run it again whenever: each step already done is said and\n" +
			"skipped.\n\n" +
			"An invite link, such as https://chat.example.com/join#PGTQRBXGTCDFFWBH, names its instance,\n" +
			"so it is all a newcomer needs; `norite invite create` prints one. The instance it names is\n" +
			"shown before anything is sent to it.\n\n" +
			"Passwords are read without echo and never accepted as flags. From a script, `norite\n" +
			"register`, `norite login` and `norite daemon install` do the same steps one at a time.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "instance", Usage: "`URL` of the instance, when no invite link names it"},
			&cli.StringFlag{Name: "email", Usage: "`EMAIL` of the account (default: ask)"},
			&cli.StringFlag{Name: "invite-code",
				Usage: "the instance's own invite `CODE`, when it takes new accounts by invite (default: ask)"},
			&cli.StringFlag{Name: "provider", Usage: "sign in through an OAuth `PROVIDER` (google or github)"},
			&cli.BoolFlag{Name: "device-code", Usage: "sign in with a code completed in a browser on another device"},
			&cli.BoolFlag{Name: "no-daemon", Usage: "leave the daemon alone"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() > 1 {
				return clierr.Usage("setup takes at most one argument, an invite link")
			}
			store, err := credentials.Open()
			if err != nil {
				return cli.Exit(err.Error(), 1)
			}
			store.Notify = notifier(cmd.Writer)
			readLine, readSecret, interactive := terminalReaders(os.Stdin, cmd.Writer)
			s := &Setup{
				Options: SetupOptions{
					Invite: cmd.Args().First(), Instance: cmd.String("instance"), Email: cmd.String("email"),
					InviteCode: cmd.String("invite-code"), Provider: cmd.String("provider"),
					DeviceCode: cmd.Bool("device-code"), NoDaemon: cmd.Bool("no-daemon"),
				},
				Store: store, Out: cmd.Writer, ReadLine: readLine, ReadSecret: readSecret,
				Interactive: interactive, Hostname: os.Hostname, Daemon: daemon, Connect: connect,
			}
			return setupExit(s.Run(ctx))
		},
	}
}

// setupExit is what main is handed for err. What carries its own exit code keeps it, as the verbs' errors
// do: a usage error is 2, an unavailable daemon or instance 3, a refusal such as an expired invite 4, and
// main matches each through the wrapping. Anything else is 1.
func setupExit(err error) error {
	if err == nil {
		return nil
	}
	var usage *clierr.UsageError
	var unavailable *clierr.UnavailableError
	var refused *clierr.RefusedError
	switch {
	case errors.Is(err, ErrNoTerminal), errors.As(err, &usage), errors.As(err, &unavailable), errors.As(err, &refused):
		return err
	}
	return cli.Exit(err.Error(), 1)
}
