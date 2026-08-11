package daemonctl

import (
	"os"
	"strings"
	"testing"
)

// Regression tests for the M3 code-review findings. Each one failed before its fix; grouped here so the
// scenario that was missed stays legible rather than being scattered into the per-backend files.

// launchctl kill exits non-zero with "No such process" when the agent is loaded but has no running process
// — after a crash, or after an earlier stop. Manager promises stop-on-a-stopped-one succeeds, and systemd
// and Task Scheduler both honor that; launchd was the only backend that turned it into an error.
func TestLaunchdStopSucceedsWhenTheDaemonIsAlreadyStopped(t *testing.T) {
	cases := []struct {
		name string
		res  Result
	}{
		{"errno as exit code", Result{ExitCode: 3}},
		{"errno in the message", Result{ExitCode: 1, Stderr: "Could not kill service: 3: No such process"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, r, _ := newLaunchd(t)
			if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
				t.Fatalf("Install: %v", err)
			}
			r.respond("launchctl kill", tc.res)

			if err := l.Stop(t.Context()); err != nil {
				t.Fatalf("Stop on an already-stopped agent: %v", err)
			}
		})
	}
}

// The consequence of the bug above, and the reason it was worth a fix rather than a note: restart
// propagates a stop failure and never reaches Start (deliberately — see restartCommand). So the command a
// user reaches for to recover a crashed daemon would report failure and leave it down.
func TestRestartRecoversADaemonThatIsAlreadyStopped(t *testing.T) {
	l, r, _ := newLaunchd(t)
	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	r.respond("launchctl kill", Result{ExitCode: 3, Stderr: "Could not kill service: 3: No such process"})

	if err := l.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := l.Start(t.Context()); err != nil {
		t.Fatalf("Start after stopping an already-stopped agent: %v", err)
	}
	if !r.ran("launchctl kickstart " + l.serviceTarget()) {
		t.Errorf("the daemon was never started; ran: %v", r.lines())
	}
}

// A real launchctl failure must still surface — the tolerance above is for one specific condition, not a
// blanket "ignore errors from stop".
func TestLaunchdStopSurfacesARealFailure(t *testing.T) {
	l, r, _ := newLaunchd(t)
	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	r.respond("launchctl kill", Result{ExitCode: 1, Stderr: "Operation not permitted"})

	err := l.Stop(t.Context())
	if err == nil {
		t.Fatal("Stop succeeded despite launchctl reporting a real failure")
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("the error drops the reason: %v", err)
	}
}

// LogHint pointed at norite-daemon.out.log, but the daemon writes everything to stderr and nothing to
// stdout — so `norite daemon status` sent operators to tail a file that is always empty.
func TestLaunchdLogHintNamesTheFileTheDaemonActuallyWrites(t *testing.T) {
	l, _, plistPath := newLaunchd(t)
	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading the plist: %v", err)
	}
	plist := string(body)

	hint := l.LogHint()
	if !strings.Contains(hint, ServiceName+".log") || strings.Contains(hint, ".out.log") {
		t.Errorf("LogHint = %q, want the daemon's own rotating log", hint)
	}
	// And that file has to be the one the plist actually tells the daemon to write.
	if !strings.Contains(plist, "<string>-log-file</string>") {
		t.Errorf("the plist does not point the daemon's log anywhere:\n%s", plist)
	}
	if !strings.Contains(plist, ServiceName+".log</string>") {
		t.Errorf("the plist and LogHint name different files:\n%s", plist)
	}
}

// launchd writes StandardErrorPath to a plain file it never rotates. With the daemon mirroring every line
// to stderr, the rotated log's 3x10MB cap was enforced on one copy while the other grew forever.
func TestLaunchdDoesNotDuplicateTheRotatedLogIntoAnUnboundedOne(t *testing.T) {
	l, _, plistPath := newLaunchd(t)
	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading the plist: %v", err)
	}
	plist := string(body)

	if !strings.Contains(plist, "<string>-stderr-log=false</string>") {
		t.Errorf("the daemon still mirrors into launchd's unrotated capture:\n%s", plist)
	}
	// Nothing is written to stdout, so redirecting it only creates an empty file to wonder about.
	if strings.Contains(plist, "StandardOutPath") {
		t.Errorf("the plist redirects stdout, which the daemon never writes to:\n%s", plist)
	}
}

// launchd cannot exempt an exit code the way systemd's RestartPreventExitStatus does, so a daemon exiting 3
// against a lock held by a hand-started instance is respawned. The loop is self-correcting but real, so it
// is at least throttled rather than running at launchd's 10-second default.
func TestLaunchdThrottlesItsRestartLoop(t *testing.T) {
	l, _, plistPath := newLaunchd(t)
	if err := l.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("reading the plist: %v", err)
	}
	if !strings.Contains(string(body), "<key>ThrottleInterval</key>") {
		t.Errorf("no ThrottleInterval, so an exit-3 loop runs at launchd's default rate:\n%s", body)
	}
}

// `launchctl bootstrap` loads the agent and RunAtLoad runs it, so install *does* start the daemon on macOS
// — while the command printed systemd's "to start it now" advice on every platform.
func TestStartsOnInstallMatchesEachPlatformsBehavior(t *testing.T) {
	cases := map[string]bool{"linux": false, "darwin": true, "windows": false}
	for goos, want := range cases {
		t.Run(goos, func(t *testing.T) {
			mgr, err := newFor(goos, newFakeRunner())
			if err != nil {
				t.Fatalf("newFor(%q): %v", goos, err)
			}
			if got := mgr.StartsOnInstall(); got != want {
				t.Errorf("StartsOnInstall() = %v, want %v", got, want)
			}
		})
	}
}

func TestInstallOutputMatchesWhetherItStartedTheDaemon(t *testing.T) {
	isolate(t)
	binary := writeExecutable(t, t.TempDir(), daemonBinaryName+exeSuffix())

	t.Run("platform that does not start it", func(t *testing.T) {
		out, err := runCommand(t, &stubManager{}, "daemon", "install", "--daemon-binary", binary)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !strings.Contains(out, "norite daemon start") {
			t.Errorf("the output does not say how to start it:\n%s", out)
		}
	})

	t.Run("platform that starts it", func(t *testing.T) {
		out, err := runCommand(t, &stubManager{startsOnInstall: true}, "daemon", "install", "--daemon-binary", binary)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		// Telling a macOS user to start a daemon that is already running is the specific wrongness here.
		if strings.Contains(out, "norite daemon start") {
			t.Errorf("the output tells the user to start an already-running daemon:\n%s", out)
		}
		if !strings.Contains(out, "running now") {
			t.Errorf("the output does not say the daemon is already up:\n%s", out)
		}
	})
}

// systemd expands variables inside double quotes too, so quoting alone left `$` live: an unknown variable
// expands to nothing and the unit fails at boot naming a path the operator never wrote.
func TestSystemdEscapesDollarAndPercent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dollar", "/opt/build$rev/norite-daemon", `"/opt/build$$rev/norite-daemon"`},
		{"percent specifier", "/opt/%h/norite-daemon", `"/opt/%%h/norite-daemon"`},
		{"space", "/home/a b/norite-daemon", `"/home/a b/norite-daemon"`},
		{"nothing to escape", "/opt/norite/norite-daemon", "/opt/norite/norite-daemon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := systemdEscape(tc.in); got != tc.want {
				t.Errorf("systemdEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The systemd user manager does not read the shell profile, so a user who exports XDG_STATE_HOME gets a
// different state directory — and therefore a different single-instance lock — from the service than from a
// hand-started daemon. Both would start, silently breaking the one-daemon-per-user invariant.
func TestSystemdCarriesXDGStateHomeIntoTheUnit(t *testing.T) {
	t.Run("captured when set", func(t *testing.T) {
		s, _, unitPath := newSystemd(t)
		stateHome := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateHome)

		if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		body, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("reading the unit file: %v", err)
		}
		if !strings.Contains(string(body), "Environment=XDG_STATE_HOME="+stateHome) {
			t.Errorf("the unit does not pin the state directory:\n%s", body)
		}
	})

	t.Run("omitted when unset", func(t *testing.T) {
		s, _, unitPath := newSystemd(t)
		t.Setenv("XDG_STATE_HOME", "")

		if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		body, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("reading the unit file: %v", err)
		}
		// Pinning an empty value would be worse than not pinning: it would override the daemon's own
		// default resolution with nothing.
		if strings.Contains(string(body), "XDG_STATE_HOME") {
			t.Errorf("the unit pins a variable that was never set:\n%s", body)
		}
	})

	t.Run("a relative value is not carried", func(t *testing.T) {
		s, _, unitPath := newSystemd(t)
		t.Setenv("XDG_STATE_HOME", "relative/state")

		if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		body, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("reading the unit file: %v", err)
		}
		// The daemon ignores a relative value, so baking one in would pin the unit to a setting that does
		// nothing while looking like it does something.
		if strings.Contains(string(body), "XDG_STATE_HOME") {
			t.Errorf("the unit pins an invalid relative value:\n%s", body)
		}
	})
}

// Every non-zero schtasks /Query exit was read as "not installed", so a Group Policy or permissions failure
// on a machine where the task exists told the user to install it — which fails the same way.
func TestWindowsStatusDistinguishesAFailedQueryFromAnAbsentTask(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, Result{ExitCode: 1, Stderr: "ERROR: Access is denied."})
	w := &windowsTask{run: r}

	_, err := w.Status(t.Context())
	if err == nil {
		t.Fatal("Status reported a permissions failure as \"not installed\"")
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Errorf("the error hides the real cause: %v", err)
	}

	// Start and Stop gate on the same query, so they must not swallow it either.
	if err := w.Start(t.Context()); err == nil || strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start misreported a failed query: %v", err)
	}
}

// The install flag used to declare Sources: cli.EnvVars(NORITE_DAEMON_BINARY), so the environment value
// arrived at LocateDaemon as if it had been typed — and a bad path produced an error blaming a command line
// the user never wrote.
func TestEnvironmentSuppliedBinaryIsNotBlamedOnTheCommandLine(t *testing.T) {
	isolate(t)

	missing := "/no/such/norite-daemon"
	t.Setenv(DaemonBinaryEnvVar, missing)

	_, err := runCommand(t, &stubManager{}, "daemon", "install")
	if err == nil {
		t.Fatal("install succeeded with a bad path in the environment")
	}
	if strings.Contains(err.Error(), "command line") {
		t.Errorf("the error blames the command line for an environment value: %v", err)
	}
	if !strings.Contains(err.Error(), DaemonBinaryEnvVar) {
		t.Errorf("the error does not name the variable that supplied the path: %v", err)
	}
}
