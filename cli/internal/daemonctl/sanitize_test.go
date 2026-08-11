package daemonctl

import (
	"os/exec"
	"strings"
	"testing"
)

func TestTerminalSafeStripsEscapeSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "active", "active"},
		{"newlines and tabs survive", "TaskName:\t\\X\nStatus:\tReady", "TaskName:\t\\X\nStatus:\tReady"},

		// The attack this exists to stop: a sequence that repaints the line, so what the user reads is not
		// what the command actually reported.
		{"ANSI color", "\x1b[31mactive\x1b[0m", "[31mactive[0m"},
		{"cursor movement", "stopped\x1b[2K\x1b[1Grunning", "stopped[2K[1Grunning"},
		{"carriage return overwrite", "inactive\ractive", "inactiveactive"},

		// 0x9b is CSI on its own in some terminals, with no ESC in front of it to look for — which is why
		// the filter covers the whole C1 range rather than just hunting for 0x1b.
		{"bare CSI with no ESC", "a\u009b31mb", "a31mb"},

		{"DEL", "a\u007fb", "ab"},
		{"NUL and bell", "a\x00b\ac", "abc"},

		// Sanitizing must not mangle legitimate text. A localized Windows reports its task status in the
		// system language, and those words have to survive intact.
		{"non-ASCII text is preserved", "Prêt — 日本語", "Prêt — 日本語"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalSafe(tc.in); got != tc.want {
				t.Errorf("terminalSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

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
