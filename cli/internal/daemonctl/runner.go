package daemonctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Alexnex31/Norite/cli/internal/termsafe"
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

	// Block rather than Text: this output is multi-line and column-aligned, and a tool's own message is
	// harder to read mangled for no gain — neither a newline nor a tab can move a cursor or repaint a line.
	res := Result{
		Stdout: termsafe.Block(strings.TrimRight(stdout.String(), "\r\n")),
		Stderr: termsafe.Block(strings.TrimRight(stderr.String(), "\r\n")),
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
