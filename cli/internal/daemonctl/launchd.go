package daemonctl

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

const plistFileName = launchdLabel + ".plist"

// launchdAgent drives a launchd *user agent*.
//
// An agent in ~/Library/LaunchAgents, not a daemon in /Library/LaunchDaemons: agents run in the user's GUI
// session as that user, which is what the daemon needs (its keychain access, its home directory) and what
// lets it be installed without root.
type launchdAgent struct{ run Runner }

// plistTemplate is the agent definition.
//
// Written as text rather than marshaled, because Apple's plist format has no encoder in the standard
// library and the document is short and fixed. Every interpolated value goes through the `xml` function,
// so a path containing & or < cannot break the document.
var plistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{
	"xml": xmlEscape,
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{ .Label | xml }}</string>

	<key>ProgramArguments</key>
	<array>
		<string>{{ .Program | xml }}</string>
		<!-- Put the daemon's own rotating log where macOS users and Console.app look for logs, rather than
		     somewhere only Norite knows about. -->
		<string>-log-file</string>
		<string>{{ .LogPath | xml }}</string>
		<!-- And stop it mirroring to stderr. launchd writes stderr to StandardErrorPath, a plain file it
		     never rotates or truncates — mirroring every line into it would duplicate the rotated log into
		     an unbounded one, defeating the size cap on the copy that does rotate. With this off, that file
		     collects only panics and failures from before logging is up, which is what it is useful for. -->
		<string>-stderr-log=false</string>
	</array>

	<!-- Start at login. The daemon is what makes the CLI and GUI able to attach instantly rather than
	     reconnecting from cold, so it should already be up by the time either is opened. -->
	<key>RunAtLoad</key>
	<true/>

	<!-- Restart on a crash, but not after a clean exit. SuccessfulExit=false is the launchd spelling of
	     "keep it alive unless it exited 0", which is what makes ` + "`norite daemon stop`" + ` actually stop it
	     instead of being undone a second later.

	     Unlike systemd, launchd cannot exempt a specific exit code, so it will also respawn a daemon that
	     exited 3 because another instance holds the lock — for as long as that other instance runs. The
	     loop is self-correcting (the first respawn after the other daemon stops succeeds) but it is a loop,
	     so ThrottleInterval below widens it from launchd's 10-second default to keep it cheap and quiet. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>

	<!-- Background: this is a long-lived support process, not something the user is waiting on, so it
	     should not compete with foreground apps for CPU or keep the machine awake. -->
	<key>ProcessType</key>
	<string>Background</string>

	<!-- Only stderr, and only for what escapes the daemon's own logging: a panic, or a failure from before
	     the log file is open. Nothing routine reaches it (see -stderr-log=false above), so this file stays
	     small even though launchd never rotates it. stdout is not redirected because nothing is written
	     there. -->
	<key>StandardErrorPath</key>
	<string>{{ .StderrPath | xml }}</string>
</dict>
</plist>
`))

func (l *launchdAgent) DefinitionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the user's home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistFileName), nil
}

// StartsOnInstall is true: `launchctl bootstrap` loads the agent, and loading one with RunAtLoad set
// runs it immediately. launchd offers no way to register a login agent without also starting it.
func (l *launchdAgent) StartsOnInstall() bool { return true }

// LogHint names the daemon's own rotating log, which the plist points at ~/Library/Logs.
//
// Not the StandardErrorPath file next to it: nothing routine is written there (see the plist), so sending
// an operator to it would have them tail an empty file while the output they want sits in the sibling.
func (l *launchdAgent) LogHint() string {
	return "tail -f ~/Library/Logs/" + ServiceName + ".log"
}

// serviceTarget is launchd's addressing scheme: a domain plus the label.
func (l *launchdAgent) serviceTarget() string {
	return l.domainTarget() + "/" + launchdLabel
}

func (l *launchdAgent) domainTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func (l *launchdAgent) Install(ctx context.Context, daemonBinary string) error {
	path, err := l.DefinitionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	logDir, err := l.logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", logDir, err)
	}

	var plist strings.Builder
	err = plistTemplate.Execute(&plist, struct{ Label, Program, LogPath, StderrPath string }{
		Label:      launchdLabel,
		Program:    daemonBinary,
		LogPath:    filepath.Join(logDir, ServiceName+".log"),
		StderrPath: filepath.Join(logDir, ServiceName+".err.log"),
	})
	if err != nil {
		return fmt.Errorf("rendering the launchd plist: %w", err)
	}

	if err := os.WriteFile(path, []byte(plist.String()), 0o644); err != nil { //nolint:gosec // read by launchd as this user; holds no secret
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// Reinstalling over an existing agent has to boot the old one out first — bootstrap on an already
	// bootstrapped label fails, and launchd would otherwise keep running the definition it loaded before
	// the file changed. The failure is ignored precisely because the usual case is that nothing was loaded.
	_, _ = l.run.Run(ctx, "launchctl", "bootout", l.serviceTarget())

	if _, err := mustSucceed(ctx, l.run, "launchctl", "bootstrap", l.domainTarget(), path); err != nil {
		return err
	}
	// bootstrap loads the definition; disable would survive a reinstall, so clear it explicitly rather than
	// leaving an earlier `norite daemon stop` silently suppressing every future login start.
	if _, err := mustSucceed(ctx, l.run, "launchctl", "enable", l.serviceTarget()); err != nil {
		return err
	}
	return nil
}

func (l *launchdAgent) Uninstall(ctx context.Context) error {
	path, err := l.DefinitionPath()
	if err != nil {
		return err
	}

	// Best-effort, for the same reason as systemd's: uninstall's contract is that the service is gone
	// afterwards, so a failure to unload something that may not be loaded must not block removing the file.
	_, _ = l.run.Run(ctx, "launchctl", "bootout", l.serviceTarget())

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

func (l *launchdAgent) Start(ctx context.Context) error {
	if err := l.requireInstalled(); err != nil {
		return err
	}
	// kickstart starts the service if it is not running and is a no-op if it is — the idempotence this
	// interface promises. `launchctl start` is the deprecated spelling and does not report failures.
	_, err := mustSucceed(ctx, l.run, "launchctl", "kickstart", l.serviceTarget())
	return err
}

func (l *launchdAgent) Stop(ctx context.Context) error {
	if err := l.requireInstalled(); err != nil {
		return err
	}
	// SIGTERM rather than bootout: bootout unloads the definition entirely, so the daemon would not come
	// back at the next login and `norite daemon start` would fail — that is uninstall, not stop. With
	// KeepAlive set to SuccessfulExit=false, a clean exit after SIGTERM is not restarted.
	res, err := l.run.Run(ctx, "launchctl", "kill", "SIGTERM", l.serviceTarget())
	if err != nil {
		return err
	}

	// A loaded agent with no running process makes `launchctl kill` fail with "No such process". Stopping
	// something already stopped is a success by Manager's contract, and treating it as a failure would
	// break the recovery path specifically: `norite daemon restart` propagates a stop error and never
	// reaches Start, so a user restarting an already-crashed daemon would be told it failed and left with
	// it still down. systemd and Task Scheduler both tolerate this; launchd is the odd one out.
	if res.ExitCode != 0 && !launchdReportsNoSuchProcess(res) {
		return errFromResult("launchctl kill SIGTERM", res)
	}
	return nil
}

// launchdReportsNoSuchProcess reports whether a launchctl failure means "it was not running".
//
// launchctl answers with a POSIX errno both as its exit status and in its message; 3 is ESRCH. Both are
// checked because the exit status alone is ambiguous — launchctl has historically also used small integers
// for its own errors — and the message alone is not guaranteed to be phrased the same way across releases.
func launchdReportsNoSuchProcess(res Result) bool {
	if res.ExitCode == 3 {
		return true
	}
	return strings.Contains(strings.ToLower(res.Stderr+res.Stdout), "no such process")
}

func (l *launchdAgent) Status(ctx context.Context) (State, error) {
	path, err := l.DefinitionPath()
	if err != nil {
		return State{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("checking %s: %w", path, err)
	}

	// `launchctl print` on a loaded service exits 0 and prints a block including a "pid = N" line when it
	// is actually running; a loaded-but-not-running service prints the block without a pid. Anything else
	// (exit 113, "Could not find service") means loaded is false.
	res, err := l.run.Run(ctx, "launchctl", "print", l.serviceTarget())
	if err != nil {
		return State{}, err
	}
	if res.ExitCode != 0 {
		return State{Installed: true, Running: false, Detail: "not loaded"}, nil
	}

	running := launchdPrintReportsAPID(res.Stdout)
	detail := "loaded, not running"
	if running {
		detail = "running"
	}
	return State{Installed: true, Running: running, Detail: detail}, nil
}

func (l *launchdAgent) requireInstalled() error {
	path, err := l.DefinitionPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotInstalled
	}
	return nil
}

func (l *launchdAgent) logDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the user's home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Logs"), nil
}

// launchdPrintReportsAPID reports whether `launchctl print` output describes a running process.
//
// Matched on the key rather than the whole line because launchctl's indentation has changed between macOS
// releases and is not something to depend on.
func launchdPrintReportsAPID(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || strings.TrimSpace(key) != "pid" {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && pid > 0 {
			return true
		}
	}
	return false
}

func xmlEscape(s string) string {
	var sb strings.Builder
	// Errors from xml.EscapeText on a strings.Builder are impossible — Builder's Write never fails — but
	// the signature returns one, so it is checked rather than ignored to keep errcheck honest.
	if err := xml.EscapeText(&sb, []byte(s)); err != nil {
		return ""
	}
	return sb.String()
}
