// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf16"
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

// taskTemplate is the task's definition, in Task Scheduler's own XML.
//
// Until M23 the task was made with `schtasks /Create /SC ONLOGON`, which takes Task Scheduler's defaults,
// and they are a laptop's defaults for a job that runs for a minute: not started on battery and stopped
// when the machine goes onto it, ended after three days, never restarted. None of them can be changed
// from that command's flags. A definition says each one:
//
//   - The trigger is this user's logon, not anybody's. The task is theirs and runs as them.
//   - LeastPrivilege is what /RL LIMITED was: the user's own integrity level, never elevated.
//   - IgnoreNew: a second start while one runs does nothing, which is what makes start idempotent.
//   - PT0S is no time limit.
//   - Three restarts a minute apart after a failure. An exit of 0, which a requested stop is, is not one.
//
// The element order is the one Task Scheduler exports. Written from its documented schema and not yet
// registered on a Windows machine from this code; docs/roadmap.md's M23 entry says so.
var taskTemplate = template.Must(template.New("task").Funcs(template.FuncMap{
	"xml": xmlEscape,
}).Parse(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Norite background daemon</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>{{ .User | xml }}</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>{{ .User | xml }}</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>{{ .Program | xml }}</Command>
    </Exec>
  </Actions>
</Task>
`))

// taskXML renders the definition for one user and one executable.
func taskXML(user, program string) (string, error) {
	var b strings.Builder
	if err := taskTemplate.Execute(&b, struct{ User, Program string }{user, program}); err != nil {
		return "", fmt.Errorf("rendering the task definition: %w", err)
	}
	return b.String(), nil
}

// utf16File encodes text as UTF-16 little-endian with a byte-order mark, which is what the definition's
// own first line declares. schtasks refuses a file whose bytes and declaration disagree.
func utf16File(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func (w *windowsTask) Install(ctx context.Context, daemonBinary string) error {
	if strings.ContainsFunc(daemonBinary, isControl) {
		return fmt.Errorf("refusing to register a task: the executable path contains a control character (%s)",
			strconv.Quote(daemonBinary))
	}
	// Who the task is for, in the form Task Scheduler names accounts: DOMAIN\user. Asked of Windows
	// rather than assembled from the environment, which a shell can set to anything.
	res, err := mustSucceed(ctx, w.run, "whoami")
	if err != nil {
		return err
	}
	user := strings.TrimSpace(res.Stdout)
	if user == "" || strings.ContainsFunc(user, isControl) {
		return fmt.Errorf("`whoami` did not name this account (%s), so the task cannot be made its own",
			strconv.Quote(user))
	}

	definition, err := taskXML(user, daemonBinary)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "norite-task-*.xml")
	if err != nil {
		return fmt.Errorf("writing the task definition: %w", err)
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = file.Write(utf16File(definition))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("writing the task definition: %w", err)
	}

	// /F replaces an existing task, which is what makes reinstalling idempotent rather than an error.
	_, err = mustSucceed(ctx, w.run, "schtasks", "/Create", "/TN", windowsTaskName, "/XML", path, "/F")
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
	if res.ExitCode != 0 {
		absent, err := w.absentNotFailing(ctx)
		if err != nil {
			return err
		}
		// Deleting a task that is already gone is the outcome Uninstall wanted, so it is a success.
		if !absent {
			return errFromResult("schtasks /Delete", res)
		}
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

	// /End on a task that is not running exits non-zero. Stopping something already stopped is a success by
	// this interface's contract, and Manager's idempotence promise is load-bearing: restart propagates a
	// stop failure, so a Stop that errors on an already-stopped daemon breaks the command people reach for
	// to recover a crashed one.
	//
	// The task's existence was just confirmed above, so any /End failure from here is either "not running"
	// or a transient fault — and neither is worth failing a stop over. Reading the message to tell them
	// apart is what broke on localized Windows in the first place.
	res, err := w.run.Run(ctx, "schtasks", "/End", "/TN", windowsTaskName)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		absent, err := w.absentNotFailing(ctx)
		if err != nil {
			return err
		}
		// Raced with an uninstall between the check above and here.
		if absent {
			return ErrNotInstalled
		}
	}
	return nil
}

func (w *windowsTask) Status(ctx context.Context) (State, error) {
	res, err := w.run.Run(ctx, "schtasks", "/Query", "/TN", windowsTaskName, "/FO", "LIST")
	if err != nil {
		return State{}, err
	}
	if res.ExitCode != 0 {
		// Only an absent task means "not installed". Group Policy or a permissions problem can make /Query
		// fail on a machine where it exists, and reporting that as "not installed" would send the user to
		// `norite daemon install` — which fails the same way, with the real cause still hidden.
		absent, err := w.absentNotFailing(ctx)
		if err != nil {
			return State{}, err
		}
		if !absent {
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
// The output is a block of "Key: value" lines, and both the key and the value are localized on a
// non-English Windows. Matching the English key is a deliberate best effort: a mismatch degrades Status to
// "unknown" rather than misreporting it. The Installed flag — which is what the CLI actually turns into an
// exit code — does not depend on this at all; it comes from taskExists, which reads no translated text.
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

// taskExists answers "is this task registered at all", without reading a single word of English.
//
// The obvious implementation — matching schtasks' "cannot find" error text — is wrong on every non-English
// Windows, and it gated the Installed flag that `norite daemon status` turns into an exit code and that
// Start/Stop turn into ErrNotInstalled. A localized machine therefore got an error where it should have got
// "not installed", and lost the idempotence Manager promises for Stop and Uninstall.
//
// Listing tasks instead is locale-independent for the one reason that matters: schtasks translates its
// messages and its column headers, but never a task name, because the name is ours. /NH drops the header
// row so nothing localized is parsed at all.
//
// A failure of the listing itself is a real error and stays one — that is what keeps a Group Policy or
// permissions problem from being reported as "not installed", which would send someone to
// `norite daemon install` with the actual cause still hidden.
func (w *windowsTask) taskExists(ctx context.Context) (bool, error) {
	res, err := w.run.Run(ctx, "schtasks", "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, errFromResult("schtasks /Query", res)
	}
	return strings.Contains(res.Stdout, windowsTaskName), nil
}

// absentNotFailing distinguishes "that command failed because the task is not there" from "that command
// failed for a reason worth reporting", for a per-task schtasks call that already returned non-zero.
func (w *windowsTask) absentNotFailing(ctx context.Context) (bool, error) {
	exists, err := w.taskExists(ctx)
	if err != nil {
		return false, err
	}
	return !exists, nil
}

func errFromResult(what string, res Result) error {
	return fmt.Errorf("`%s` failed (exit %d): %s", what, res.ExitCode, firstNonEmpty(res.Stderr, res.Stdout, "no output"))
}
