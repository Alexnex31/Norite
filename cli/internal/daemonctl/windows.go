package daemonctl

import (
	"context"
	"fmt"
	"strings"
)

// windowsTask drives a Task Scheduler task that runs at logon.
//
// A scheduled task rather than a Windows *service*, which is what docs/roadmap.md M3 calls for. The
// difference matters: a real service is machine-wide, needs Administrator to install, and runs in session 0
// with no access to the interactive user's credential store — all three wrong for a per-user daemon holding
// that user's tokens. A logon task installs unelevated, runs as the user, and starts when they log in.
type windowsTask struct{ run Runner }

// DefinitionPath reports no path: Task Scheduler keeps its definitions in a registry-backed store rather
// than a file the user can usefully be pointed at.
func (w *windowsTask) DefinitionPath() (string, error) { return "", nil }

// StartsOnInstall is false: a logon task is registered and waits for its trigger.
func (w *windowsTask) StartsOnInstall() bool { return false }

func (w *windowsTask) LogHint() string {
	return `Task Scheduler (taskschd.msc), Task Scheduler Library > "` + windowsTaskName + `"`
}

func (w *windowsTask) Install(ctx context.Context, daemonBinary string) error {
	// /RL LIMITED runs at the user's normal integrity level rather than elevated. The daemon needs no
	// privilege beyond the user's own, and asking for elevation would both prompt at install time and make
	// every later escalation bug more expensive.
	//
	// /F replaces an existing task, which is what makes reinstalling idempotent rather than an error.
	_, err := mustSucceed(ctx, w.run, "schtasks",
		"/Create",
		"/TN", windowsTaskName,
		"/TR", quoteTaskCommand(daemonBinary),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	)
	return err
}

func (w *windowsTask) Uninstall(ctx context.Context) error {
	// Best-effort stop first, then delete. /F suppresses the confirmation prompt, without which this would
	// block forever waiting on stdin that a scripted install never provides.
	_, _ = w.run.Run(ctx, "schtasks", "/End", "/TN", windowsTaskName)

	res, err := w.run.Run(ctx, "schtasks", "/Delete", "/TN", windowsTaskName, "/F")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !taskNotFound(res) {
		return errFromResult("schtasks /Delete", res)
	}
	return nil
}

func (w *windowsTask) Start(ctx context.Context) error {
	installed, err := w.installed(ctx)
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}
	// /Run on an already-running task returns success and starts no second instance — the task's default
	// multiple-instances policy is to refuse a parallel run, which is exactly the idempotence wanted here.
	_, err = mustSucceed(ctx, w.run, "schtasks", "/Run", "/TN", windowsTaskName)
	return err
}

func (w *windowsTask) Stop(ctx context.Context) error {
	installed, err := w.installed(ctx)
	if err != nil {
		return err
	}
	if !installed {
		return ErrNotInstalled
	}

	// /End on a task that is not running exits non-zero with "The system cannot find the file specified" or
	// similar. Stopping something already stopped is a success by this interface's contract, so that case
	// is accepted rather than surfaced.
	res, err := w.run.Run(ctx, "schtasks", "/End", "/TN", windowsTaskName)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !taskNotRunning(res) {
		return errFromResult("schtasks /End", res)
	}
	return nil
}

func (w *windowsTask) Status(ctx context.Context) (State, error) {
	res, err := w.run.Run(ctx, "schtasks", "/Query", "/TN", windowsTaskName, "/FO", "LIST")
	if err != nil {
		return State{}, err
	}
	if res.ExitCode != 0 {
		// Only "cannot find" means the task is absent. Group Policy or a permissions problem can make
		// /Query fail on a machine where it exists, and reporting that as "not installed" would send the
		// user to `norite daemon install` — which fails the same way, with the real cause still hidden.
		if !taskNotFound(res) {
			return State{}, errFromResult("schtasks /Query", res)
		}
		return State{}, nil
	}

	status := taskStatusField(res.Stdout)
	return State{
		Installed: true,
		// "Running" is the only status meaning the process is up; "Ready" means installed and waiting for
		// its trigger, and "Disabled" means it will not fire at all.
		Running: strings.EqualFold(status, "Running"),
		Detail:  firstNonEmpty(status, "unknown"),
	}, nil
}

func (w *windowsTask) installed(ctx context.Context) (bool, error) {
	state, err := w.Status(ctx)
	if err != nil {
		return false, err
	}
	return state.Installed, nil
}

// taskStatusField pulls the Status value out of `schtasks /FO LIST` output.
//
// The output is a block of "Key: value" lines, and the key is localized on a non-English Windows. Matching
// the English key is a deliberate best effort: a mismatch degrades Status to "unknown" rather than
// misreporting, and the Installed flag — which is what the CLI actually gates on — comes from the exit code
// and stays correct in every locale.
func taskStatusField(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "Status") {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

func taskNotFound(res Result) bool {
	return strings.Contains(strings.ToUpper(res.Stderr+res.Stdout), "CANNOT FIND")
}

func taskNotRunning(res Result) bool { return taskNotFound(res) }

// quoteTaskCommand wraps the binary path for schtasks' /TR argument.
//
// /TR takes a command line, not a program path, and re-parses it: an unquoted `C:\Program Files\...` would
// become the program `C:\Program` with an argument. Quoting unconditionally rather than only when a space
// is present keeps the two paths through this code identical, so the one exercised in testing is the one
// that ships.
func quoteTaskCommand(path string) string { return `"` + path + `"` }

func errFromResult(what string, res Result) error {
	return fmt.Errorf("`%s` failed (exit %d): %s", what, res.ExitCode, firstNonEmpty(res.Stderr, res.Stdout, "no output"))
}
