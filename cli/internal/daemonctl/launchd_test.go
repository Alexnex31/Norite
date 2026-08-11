package daemonctl

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newLaunchd builds a launchd backend rooted in a temporary HOME, so tests never touch the developer's real
// ~/Library/LaunchAgents. Runs on Linux: nothing here executes launchctl, it only records what would be run.
func newLaunchd(t *testing.T) (*launchdAgent, *fakeRunner, string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	r := newFakeRunner()
	l := &launchdAgent{run: r}

	return l, r, filepath.Join(home, "Library", "LaunchAgents", plistFileName)
}

func TestLaunchdInstallWritesAValidPlist(t *testing.T) {
	l, _, plistPath := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading the plist: %v", err)
	}

	// launchd refuses to load a malformed plist with an error that names a line number and nothing else, so
	// checking it is well-formed XML here is worth more than it looks.
	var parsed any
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("the plist is not well-formed XML: %v\n%s", err, body)
	}

	plist := string(body)
	for _, want := range []string{
		"<string>" + launchdLabel + "</string>",
		"<string>/opt/norite/norite-daemon</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("the plist is missing %q:\n%s", want, plist)
		}
	}

	// KeepAlive with SuccessfulExit=false is what makes `norite daemon stop` stick: a bare <true/> would
	// have launchd restart the daemon a second after every clean stop.
	if !strings.Contains(plist, "<key>SuccessfulExit</key>") || !strings.Contains(plist, "<false/>") {
		t.Errorf("KeepAlive would restart the daemon after a clean stop:\n%s", plist)
	}
}

func TestLaunchdPlistEscapesTheBinaryPath(t *testing.T) {
	l, _, plistPath := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/a&b/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading the plist: %v", err)
	}

	// A bare & makes the document malformed and the agent unloadable.
	if err := xml.Unmarshal(body, new(any)); err != nil {
		t.Fatalf("an & in the path produced malformed XML: %v\n%s", err, body)
	}
	if !strings.Contains(string(body), "&amp;") {
		t.Errorf("the ampersand was not escaped:\n%s", body)
	}
}

func TestLaunchdInstallReplacesAnExistingAgent(t *testing.T) {
	l, r, _ := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// bootstrap fails on an already-bootstrapped label, so a reinstall has to boot the old one out first —
	// otherwise launchd keeps running the definition it loaded before the file changed.
	bootout := indexOfLine(r.lines(), "launchctl bootout "+l.serviceTarget())
	bootstrap := -1
	for i, line := range r.lines() {
		if strings.HasPrefix(line, "launchctl bootstrap ") {
			bootstrap = i
		}
	}
	switch {
	case bootout < 0:
		t.Errorf("no bootout before bootstrap; ran: %v", r.lines())
	case bootstrap < 0:
		t.Errorf("the agent was never bootstrapped; ran: %v", r.lines())
	case bootout > bootstrap:
		t.Errorf("bootout ran after bootstrap; ran: %v", r.lines())
	}
}

func TestLaunchdInstallReEnablesTheLabel(t *testing.T) {
	l, r, _ := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// launchctl disable survives a reinstall. Without an explicit enable, an earlier stop could silently
	// suppress every future login start, and reinstalling — the obvious thing to try — would not fix it.
	if !r.ran("launchctl enable " + l.serviceTarget()) {
		t.Errorf("the label was not re-enabled; ran: %v", r.lines())
	}
}

func TestLaunchdStopSendsSIGTERMRatherThanUnloading(t *testing.T) {
	l, r, _ := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := l.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// bootout would unload the definition, so the daemon would not return at the next login and
	// `norite daemon start` would fail. That is uninstall's job, not stop's.
	if !r.ran("launchctl kill SIGTERM " + l.serviceTarget()) {
		t.Errorf("stop did not signal the daemon; ran: %v", r.lines())
	}
	if _, err := os.Stat(mustDefinitionPath(t, l)); err != nil {
		t.Errorf("stop removed the agent definition: %v", err)
	}
}

func TestLaunchdStartUsesKickstart(t *testing.T) {
	l, r, _ := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := l.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// `launchctl start` is the deprecated spelling and reports no failures; kickstart both starts a stopped
	// service and no-ops on a running one, which is the idempotence Manager promises.
	if !r.ran("launchctl kickstart " + l.serviceTarget()) {
		t.Errorf("start did not kickstart the service; ran: %v", r.lines())
	}
}

func TestLaunchdStartAndStopRefuseWhenNotInstalled(t *testing.T) {
	l, r, _ := newLaunchd(t)

	if err := l.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Start returned %v, want ErrNotInstalled", err)
	}
	if err := l.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Stop returned %v, want ErrNotInstalled", err)
	}
	if len(r.lines()) != 0 {
		t.Errorf("launchctl was invoked despite nothing being installed: %v", r.lines())
	}
}

func TestLaunchdUninstallRemovesThePlist(t *testing.T) {
	l, r, plistPath := newLaunchd(t)

	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := l.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plist survived uninstall (%v)", err)
	}
	if !r.ran("launchctl bootout " + l.serviceTarget()) {
		t.Errorf("the agent was never unloaded; ran: %v", r.lines())
	}
}

func TestLaunchdUninstallSucceedsWhenNothingIsInstalled(t *testing.T) {
	l, _, _ := newLaunchd(t)

	if err := l.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall with nothing installed: %v", err)
	}
}

func TestLaunchdStatusReportsEachState(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		l, _, _ := newLaunchd(t)

		state, err := l.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if state.Installed || state.Running {
			t.Errorf("got %+v, want a zero State", state)
		}
	})

	t.Run("installed but not loaded", func(t *testing.T) {
		l, r, _ := newLaunchd(t)
		if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		// 113 is launchctl's "Could not find service" — the plist is on disk but nothing loaded it.
		r.respond("launchctl print", Result{ExitCode: 113, Stderr: "Could not find service"})

		state, err := l.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !state.Installed || state.Running {
			t.Errorf("got %+v, want installed and not running", state)
		}
	})

	t.Run("loaded but not running", func(t *testing.T) {
		l, r, _ := newLaunchd(t)
		if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		// A loaded agent that is not currently running prints its block with no pid line at all.
		r.respond("launchctl print", Result{ExitCode: 0, Stdout: strings.Join([]string{
			"com.norite.daemon = {",
			"\tstate = not running",
			"\tprogram = /opt/norite/norite-daemon",
			"}",
		}, "\n")})

		state, err := l.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !state.Installed || state.Running {
			t.Errorf("got %+v, want installed and not running", state)
		}
	})

	t.Run("running", func(t *testing.T) {
		l, r, _ := newLaunchd(t)
		if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		r.respond("launchctl print", Result{ExitCode: 0, Stdout: strings.Join([]string{
			"com.norite.daemon = {",
			"\tpid = 4213",
			"\tstate = running",
			"}",
		}, "\n")})

		state, err := l.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !state.Installed || !state.Running {
			t.Errorf("got %+v, want installed and running", state)
		}
	})
}

func TestLaunchdPrintPIDParsing(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"indented pid", "\tpid = 42", true},
		{"unindented pid", "pid = 42", true},
		{"extra spacing", "   pid   =   42   ", true},
		// A zero pid is launchctl reporting no process, not process 0.
		{"zero pid", "\tpid = 0", false},
		{"no pid line", "\tstate = not running", false},
		// The substring "pid" appears in other keys; matching loosely would report a stopped daemon as up.
		{"a key merely containing pid", "\tlast exit pid = 42", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchdPrintReportsAPID(tc.out); got != tc.want {
				t.Errorf("launchdPrintReportsAPID(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func mustDefinitionPath(t *testing.T, m Manager) string {
	t.Helper()
	path, err := m.DefinitionPath()
	if err != nil {
		t.Fatalf("DefinitionPath: %v", err)
	}
	return path
}
