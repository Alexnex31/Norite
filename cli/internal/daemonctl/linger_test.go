// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Alexnex31/Norite/cli/internal/clierr"
)

// The unit carried After= and Wants=network-online.target until M23. That target is the system manager's:
// in a user manager it is not found, so the lines ordered the daemon after nothing.
func TestTheUnitDoesNotOrderItselfAfterATargetItsManagerLacks(t *testing.T) {
	s, _, unitPath := newSystemd(t)
	if err := s.Install(t.Context(), "/opt/norite/norite-daemon"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "After=") || strings.HasPrefix(trimmed, "Wants=") {
			t.Errorf("the unit orders itself: %q", trimmed)
		}
	}
}

func TestSystemdReadsAndAsksForLingeringByNumericID(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())
	show := "loginctl show-user " + uid + " --property=Linger --value"

	for answer, want := range map[string]bool{"yes\n": true, "no\n": false, "": false, "maybe\n": false} {
		s, r, _ := newSystemd(t)
		r.respond(show, Result{Stdout: answer})
		got, err := s.Lingering(t.Context())
		if err != nil || got != want {
			t.Errorf("logind answered %q: lingering=%v, %v; want %v", answer, got, err, want)
		}
		if lines := r.lines(); len(lines) != 1 || lines[0] != show {
			t.Errorf("asked %v, want %q", lines, show)
		}
	}

	// A machine with no logind record for the account, or no logind: not known, and said as an error.
	s, r, _ := newSystemd(t)
	r.respond(show, Result{ExitCode: 1, Stderr: "Failed to look up user"})
	if _, err := s.Lingering(t.Context()); err == nil {
		t.Error("a failed lookup was read as an answer")
	}

	s, r, _ = newSystemd(t)
	if err := s.EnableLinger(t.Context()); err != nil {
		t.Fatalf("EnableLinger: %v", err)
	}
	if lines := r.lines(); len(lines) != 1 || lines[0] != "loginctl enable-linger "+uid {
		t.Errorf("ran %v", lines)
	}

	s, r, _ = newSystemd(t)
	r.respond("loginctl enable-linger", Result{ExitCode: 1, Stderr: "Access denied"})
	if err := s.EnableLinger(t.Context()); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("a refusal: %v, want the tool's own words", err)
	}
}

// lingerManager is a stubManager on a platform where services stop at logout.
type lingerManager struct {
	*stubManager
	lingering bool
	readErr   error
	enableErr error
	enables   int
}

func (l *lingerManager) Lingering(context.Context) (bool, error) { return l.lingering, l.readErr }
func (l *lingerManager) EnableLinger(context.Context) error {
	l.enables++
	if l.enableErr == nil {
		l.lingering = true
	}
	return l.enableErr
}

func installWith(t *testing.T, mgr Manager, argv ...string) (string, error) {
	t.Helper()
	t.Setenv(DaemonBinaryEnvVar, writeExecutable(t, t.TempDir(), daemonBinaryName))
	return runCommand(t, mgr, append([]string{"daemon", "install"}, argv...)...)
}

func TestInstallSaysWhatHappensAtLogout(t *testing.T) {
	t.Run("an account that does not linger is told, and told the flag", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}}
		out, err := installWith(t, mgr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "stops when you log out") || !strings.Contains(out, "norite daemon install --linger") {
			t.Errorf("output:\n%s", out)
		}
		if mgr.enables != 0 {
			t.Error("lingering was turned on without being asked for")
		}
	})
	t.Run("an account that lingers is told that instead", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}, lingering: true}
		out, err := installWith(t, mgr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Your account lingers") || strings.Contains(out, "--linger") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("asked for, it is turned on and said", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}}
		out, err := installWith(t, mgr, "--linger")
		if err != nil {
			t.Fatal(err)
		}
		if mgr.enables != 1 || !strings.Contains(out, "now lingers") || !strings.Contains(out, "disable-linger") {
			t.Errorf("enables=%d, output:\n%s", mgr.enables, out)
		}
	})
	t.Run("asked for when it is already on, nothing is run", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}, lingering: true}
		if _, err := installWith(t, mgr, "--linger"); err != nil {
			t.Fatal(err)
		}
		if mgr.enables != 0 {
			t.Errorf("enable ran %d times on an account that lingers", mgr.enables)
		}
	})
	t.Run("asked for and refused, the install stands and the command that needs a privilege is named", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}, enableErr: errors.New("Access denied")}
		out, err := installWith(t, mgr, "--linger")
		if err == nil {
			t.Fatal("a refused --linger exited 0")
		}
		if !strings.Contains(err.Error(), "the service is installed") || !strings.Contains(err.Error(), LingerHint) ||
			!strings.Contains(err.Error(), "Access denied") {
			t.Errorf("error: %v", err)
		}
		if len(mgr.installed) != 1 || !strings.Contains(out, "Installed "+ServiceName) || strings.Contains(out, "now lingers") {
			t.Errorf("installed=%v, output:\n%s", mgr.installed, out)
		}
	})
	t.Run("where logind cannot be asked, nothing is claimed", func(t *testing.T) {
		mgr := &lingerManager{stubManager: &stubManager{}, readErr: errors.New("loginctl: not found")}
		out, err := installWith(t, mgr)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "linger") || strings.Contains(out, "log out") {
			t.Errorf("output claims something about logout it could not know:\n%s", out)
		}
		// Asked for, not being able to tell is the answer.
		if _, err := installWith(t, mgr, "--linger"); err == nil || mgr.enables != 0 {
			t.Errorf("--linger with no logind: %v, enables=%d", err, mgr.enables)
		}
	})
}

// On macOS and Windows there is no such setting, so the flag is refused before anything is written.
func TestLingerIsRefusedWhereItMeansNothing(t *testing.T) {
	mgr := &stubManager{}
	out, err := installWith(t, mgr, "--linger")
	var usage *clierr.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("--linger on a platform without it: %v, want a usage error", err)
	}
	if len(mgr.installed) != 0 || out != "" {
		t.Errorf("something was installed or printed before the refusal: %v, %q", mgr.installed, out)
	}
	// And without the flag, such a platform is told nothing about logout.
	out, err = installWith(t, mgr)
	if err != nil || strings.Contains(out, "log out") {
		t.Errorf("install: %v\n%s", err, out)
	}
}

func TestOnlyTheSystemdBackendLingers(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": false, "windows": false} {
		mgr, err := newFor(goos, newFakeRunner())
		if err != nil {
			t.Fatal(err)
		}
		if _, got := mgr.(Lingerer); got != want {
			t.Errorf("%s: lingers=%v, want %v", goos, got, want)
		}
	}
}
