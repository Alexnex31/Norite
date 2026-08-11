package daemonctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSystemd builds a systemd backend rooted in a temporary XDG_CONFIG_HOME, so tests write unit files into
// a scratch directory rather than the developer's real ~/.config/systemd/user.
func newSystemd(t *testing.T) (*systemdUser, *fakeRunner, string) {
	t.Helper()

	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)

	r := newFakeRunner()
	s := &systemdUser{run: r}

	unit := filepath.Join(cfg, "systemd", "user", unitFileName)
	return s, r, unit
}

func TestSystemdInstallWritesTheUnitAndEnablesIt(t *testing.T) {
	s, r, unitPath := newSystemd(t)

	if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the unit file: %v", err)
	}
	unit := string(body)

	if !strings.Contains(unit, "ExecStart=/opt/norite/norite-daemon") {
		t.Errorf("the unit does not start the binary it was given:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("the unit is not installable into the user's default target:\n%s", unit)
	}

	// A clean stop must not be answered with a restart, or `systemctl --user stop` could never take effect.
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Errorf("the unit restarts on a clean exit:\n%s", unit)
	}
	// Exit 3 is "another daemon already holds the lock" — retrying it forever achieves nothing.
	if !strings.Contains(unit, "RestartPreventExitStatus=3") {
		t.Errorf("the unit would restart-loop on the already-running exit code:\n%s", unit)
	}
	if !strings.Contains(unit, "NoNewPrivileges=true") {
		t.Errorf("the unit drops no privileges:\n%s", unit)
	}

	// daemon-reload has to come before enable, or systemd enables a definition it has not re-read.
	reload := indexOfLine(r.lines(), "systemctl --user daemon-reload")
	enable := indexOfLine(r.lines(), "systemctl --user enable "+unitFileName)
	switch {
	case reload < 0:
		t.Errorf("no daemon-reload was issued; ran: %v", r.lines())
	case enable < 0:
		t.Errorf("the unit was never enabled; ran: %v", r.lines())
	case reload > enable:
		t.Errorf("enable ran before daemon-reload; ran: %v", r.lines())
	}
}

func TestSystemdInstallDoesNotStartTheDaemon(t *testing.T) {
	s, r, _ := newSystemd(t)

	if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Install and start are separate verbs in this CLI. `enable --now` would blur them and make the
	// documented two-step sequence report a confusing already-running state on its second command.
	for _, line := range r.lines() {
		if strings.Contains(line, "start") || strings.Contains(line, "--now") {
			t.Errorf("install started the daemon: %q", line)
		}
	}
}

func TestSystemdInstallIsRepeatable(t *testing.T) {
	s, _, unitPath := newSystemd(t)

	if err := s.Install(t.Context(), "/opt/a/norite-daemon"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// Reinstalling after moving the binary is an ordinary upgrade, not an error, and the unit must end up
	// pointing at the new location rather than the old one.
	if err := s.Install(t.Context(), "/opt/b/norite-daemon"); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the unit file: %v", err)
	}
	if !strings.Contains(string(body), "ExecStart=/opt/b/norite-daemon") {
		t.Errorf("the reinstalled unit still points at the old binary:\n%s", body)
	}
	if strings.Contains(string(body), "/opt/a/") {
		t.Errorf("the old path survived the reinstall:\n%s", body)
	}
}

func TestSystemdEscapesAPathWithSpaces(t *testing.T) {
	s, _, unitPath := newSystemd(t)

	if err := s.Install(t.Context(), "/home/a b/bin/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("reading the unit file: %v", err)
	}

	// systemd splits ExecStart on whitespace. Unquoted, this becomes the program "/home/a" with an argument
	// — a service that fails at every boot with a message about a file that was never named.
	if !strings.Contains(string(body), `ExecStart="/home/a b/bin/norite-daemon"`) {
		t.Errorf("the path was not quoted:\n%s", body)
	}
}

func TestSystemdUninstallRemovesTheUnit(t *testing.T) {
	s, r, unitPath := newSystemd(t)

	if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unit file survived uninstall (%v)", err)
	}
	if !r.ran("systemctl --user disable " + unitFileName) {
		t.Errorf("the unit was never disabled, so it would still be wanted at login; ran: %v", r.lines())
	}
}

func TestSystemdUninstallSucceedsWhenNothingIsInstalled(t *testing.T) {
	s, _, _ := newSystemd(t)

	// Uninstall's contract is that the service is gone afterwards. Running it on a machine that never had
	// one — a provisioning script's cleanup step, a second run after a failure — has already achieved that.
	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall with nothing installed: %v", err)
	}
}

func TestSystemdUninstallProceedsWhenStopFails(t *testing.T) {
	s, r, unitPath := newSystemd(t)

	if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A dead or already-stopped unit makes `systemctl stop` unhappy on some versions. Refusing to remove
	// the definition over that would leave the user with no way to complete an uninstall.
	r.respond("systemctl --user stop", Result{ExitCode: 5, Stderr: "Unit not loaded."})

	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unit file survived uninstall (%v)", err)
	}
}

func TestSystemdStartAndStopRefuseWhenNotInstalled(t *testing.T) {
	s, r, _ := newSystemd(t)

	if err := s.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Start on a machine with no unit returned %v, want ErrNotInstalled", err)
	}
	if err := s.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Stop on a machine with no unit returned %v, want ErrNotInstalled", err)
	}
	// Better to say "not installed" than to let systemctl report its own less specific error, which sends
	// the reader looking for a broken unit rather than a missing one.
	if len(r.lines()) != 0 {
		t.Errorf("systemctl was invoked despite nothing being installed: %v", r.lines())
	}
}

func TestSystemdStatusReportsEachState(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		s, _, _ := newSystemd(t)

		state, err := s.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if state.Installed || state.Running {
			t.Errorf("got %+v, want a zero State", state)
		}
	})

	t.Run("installed but stopped", func(t *testing.T) {
		s, r, _ := newSystemd(t)
		if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}

		// `systemctl is-active` answers "inactive" with exit 3. That is the answer, not a failure — a
		// backend that treated non-zero as an error could never report a stopped daemon at all.
		r.respond("systemctl --user is-active", Result{ExitCode: 3, Stdout: "inactive"})

		state, err := s.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !state.Installed || state.Running {
			t.Errorf("got %+v, want installed and not running", state)
		}
		if state.Detail != "inactive" {
			t.Errorf("Detail = %q, want systemd's own word %q", state.Detail, "inactive")
		}
	})

	t.Run("running", func(t *testing.T) {
		s, r, _ := newSystemd(t)
		if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		r.respond("systemctl --user is-active", Result{ExitCode: 0, Stdout: "active"})

		state, err := s.Status(t.Context())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !state.Installed || !state.Running {
			t.Errorf("got %+v, want installed and running", state)
		}
	})
}

func TestSystemdSurfacesTheFailedCommand(t *testing.T) {
	s, r, _ := newSystemd(t)
	r.respond("systemctl --user enable", Result{ExitCode: 1, Stderr: "Unit norite-daemon.service is masked."})

	err := s.Install(t.Context(), "/opt/norite/norite-daemon")
	if err == nil {
		t.Fatal("Install succeeded despite systemctl failing")
	}

	// The operator's fastest route to a fix is running the same command and reading systemd's own words, so
	// both the command and its output have to survive into the message.
	if !strings.Contains(err.Error(), "systemctl --user enable") {
		t.Errorf("the error does not name the command that failed: %v", err)
	}
	if !strings.Contains(err.Error(), "masked") {
		t.Errorf("the error drops systemd's own explanation: %v", err)
	}
}

func TestSystemdDefinitionPathFollowsXDG(t *testing.T) {
	s, _, unitPath := newSystemd(t)

	got, err := s.DefinitionPath()
	if err != nil {
		t.Fatalf("DefinitionPath: %v", err)
	}
	if got != unitPath {
		t.Errorf("DefinitionPath = %q, want %q", got, unitPath)
	}
}

func TestSystemdIgnoresARelativeXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The spec says a relative value is invalid and must be ignored. Honoring it would put the unit file
	// somewhere relative to whatever directory the CLI happened to be run from.
	t.Setenv("XDG_CONFIG_HOME", "relative/config")

	s := &systemdUser{run: newFakeRunner()}
	got, err := s.DefinitionPath()
	if err != nil {
		t.Fatalf("DefinitionPath: %v", err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user", unitFileName); got != want {
		t.Errorf("DefinitionPath = %q, want the default %q", got, want)
	}
}

func indexOfLine(lines []string, want string) int {
	for i, line := range lines {
		if line == want {
			return i
		}
	}
	return -1
}
