package daemonctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command.
//
// An interface rather than a direct exec call for two reasons. Tests are the obvious one: asserting the
// exact systemctl and launchctl invocations is the only way to check those backends from a Linux CI machine
// that has no launchd. The other is that it puts every subprocess the CLI spawns behind one type, so the
// rule that a command's output is never treated as trusted terminal text (CLAUDE.md rule 19) has a single
// place to be enforced rather than a dozen.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

// Result is what a command produced.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

// Run executes name with args and captures both streams.
//
// A non-zero exit is *not* an error here — it is returned in Result.ExitCode. Service-manager tools use
// exit status to answer questions ("is this unit active?" is `systemctl is-active`, exit 3 for no), so
// treating every non-zero as a failure would make the normal answer unreachable. Errors are reserved for
// the command not running at all.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	res := Result{
		Stdout: terminalSafe(strings.TrimRight(stdout.String(), "\r\n")),
		Stderr: terminalSafe(strings.TrimRight(stderr.String(), "\r\n")),
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	case errors.Is(err, exec.ErrNotFound):
		return res, fmt.Errorf("%s is not installed or not on PATH", name)
	default:
		return res, fmt.Errorf("running %s: %w", name, err)
	}
}

// mustSucceed runs a command and turns a non-zero exit into an error naming the command and what it said.
//
// The full command line goes into the message on purpose. Service management fails for reasons this CLI
// cannot diagnose — a masked unit, a disabled linger, a Group Policy blocking task creation — and the
// operator's fastest route to an answer is running the same command themselves and reading the manager's
// own output.
func mustSucceed(ctx context.Context, r Runner, name string, args ...string) (Result, error) {
	res, err := r.Run(ctx, name, args...)
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("`%s` failed (exit %d): %s", commandLine(name, args), res.ExitCode, firstNonEmpty(res.Stderr, res.Stdout, "no output"))
	}
	return res, nil
}

// terminalSafe strips ANSI escape sequences and stray control characters from subprocess output.
//
// Everything a Runner captures ends up on the user's terminal — inside a status line, or inside an error
// message quoting what the tool said — which puts it squarely under CLAUDE.md rule 19: untrusted text is
// never written to the terminal raw. "Untrusted" is a low bar to clear here, since reaching systemd's own
// output means already being inside the user's session, but the rule exists precisely so that nobody has to
// re-litigate the trust level of each new source. Applying it at the single point output enters the program
// is cheaper than remembering it at every place output leaves.
//
// This is a narrow local version. The CLI's blanket sanitizer (docs/architecture.md §4) arrives with the TUI
// milestone that needs it; when it does, this should call that instead of keeping a second implementation.
func terminalSafe(s string) string {
	if !strings.ContainsFunc(s, isUnsafeTerminalRune) {
		return s
	}

	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			// Kept: the output is multi-line and column-aligned, and mangling it would make the tool's own
			// message harder to read for no gain — neither can reposition a cursor or rewrite the screen.
			sb.WriteRune(r)
		case isUnsafeTerminalRune(r):
			// Dropped rather than escaped. This text is shown to a human, not parsed, so a visible "\x1b"
			// would be noise; what matters is that it cannot act on the terminal.
			continue
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func isUnsafeTerminalRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	// C0 controls (including ESC, which starts every ANSI sequence), DEL, and the C1 range — the last
	// because a single 0x9B byte is CSI on its own in some terminals, with no ESC to look for.
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func commandLine(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
