package daemonctl

import (
	"os/exec"
	"strings"
	"testing"
)

// The sanitizer itself lives in cli/internal/termsafe and is tested there. What belongs here is that the
// Runner actually applies it, which is the property every backend depends on without knowing it does.

func TestExecRunnerSanitizesWhatItCaptures(t *testing.T) {
	if _, err := exec.LookPath("printf"); err != nil {
		t.Skip("no printf available to produce escape sequences")
	}

	res, err := ExecRunner{}.Run(t.Context(), "printf", `\033[31mred\033[0m`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Sanitizing inside the Runner rather than at each print site is the point: every backend, and every
	// error message quoting a tool's output, inherits it without having to remember.
	if strings.ContainsRune(res.Stdout, 0x1b) {
		t.Errorf("captured output still contains ESC: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "red") {
		t.Errorf("sanitizing destroyed the actual text: %q", res.Stdout)
	}
}

func TestExecRunnerReportsANonZeroExitAsData(t *testing.T) {
	// `systemctl is-active` answers "inactive" with exit 3. A Runner treating every non-zero exit as an
	// error could never report a stopped daemon at all.
	res, err := ExecRunner{}.Run(t.Context(), "false")
	if err != nil {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0, want non-zero")
	}
}

func TestExecRunnerErrorsWhenTheToolIsMissing(t *testing.T) {
	_, err := ExecRunner{}.Run(t.Context(), "definitely-not-a-real-command-9f3a")
	if err == nil {
		t.Fatal("Run succeeded for a command that does not exist")
	}
	// A machine without systemctl is a real situation — a container, a non-systemd distro — and this points
	// at it far better than exec's raw error does.
	if !strings.Contains(err.Error(), "not installed or not on PATH") {
		t.Errorf("the error does not explain the situation: %v", err)
	}
}
