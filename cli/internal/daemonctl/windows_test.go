package daemonctl

import (
	"errors"
	"strings"
	"testing"
)

// queryLine is the exact schtasks query the backend issues; every test that needs the task to look present
// or absent scripts a response against it.
var queryLine = "schtasks /Query /TN " + windowsTaskName + " /FO LIST"

func taskExists() Result {
	return Result{ExitCode: 0, Stdout: strings.Join([]string{
		"Folder: \\",
		"HostName:      DESKTOP-1",
		"TaskName:      \\" + windowsTaskName,
		"Next Run Time: N/A",
		"Status:        Ready",
	}, "\r\n")}
}

func taskMissing() Result {
	return Result{ExitCode: 1, Stderr: "ERROR: The system cannot find the file specified."}
}

func TestWindowsInstallCreatesALogonTask(t *testing.T) {
	r := newFakeRunner()
	w := &windowsTask{run: r}

	if err := w.Install(t.Context(), `C:\Program Files\Norite\norite-daemon.exe`); err != nil {
		t.Fatalf("Install: %v", err)
	}

	line := r.lines()[0]
	for _, want := range []string{
		"/Create",
		"/TN " + windowsTaskName,
		"/SC ONLOGON",
		// LIMITED, not HIGHEST: the daemon needs no privilege beyond the user's own, and asking for
		// elevation would prompt at install time and widen the blast radius of any later bug.
		"/RL LIMITED",
		// /F replaces an existing task, which is what makes reinstalling idempotent rather than an error.
		"/F",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the schtasks invocation is missing %q:\n%s", want, line)
		}
	}

	// /TR takes a command line that schtasks re-parses, so an unquoted "C:\Program Files\..." becomes the
	// program "C:\Program" with an argument — a task that fails at every logon.
	if !strings.Contains(line, `"C:\Program Files\Norite\norite-daemon.exe"`) {
		t.Errorf("the binary path was not quoted for /TR:\n%s", line)
	}
}

func TestWindowsUninstallDeletesTheTask(t *testing.T) {
	r := newFakeRunner()
	w := &windowsTask{run: r}

	if err := w.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// /F suppresses the confirmation prompt. Without it this blocks forever on stdin that a scripted
	// install never provides.
	if !r.ran("schtasks /Delete /TN " + windowsTaskName + " /F") {
		t.Errorf("the task was not deleted; ran: %v", r.lines())
	}
}

func TestWindowsUninstallSucceedsWhenTheTaskIsAbsent(t *testing.T) {
	r := newFakeRunner()
	r.respond("schtasks /Delete", taskMissing())
	w := &windowsTask{run: r}

	// Uninstall's contract is that the task is gone afterwards, and it already is.
	if err := w.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall on an absent task: %v", err)
	}
}

func TestWindowsUninstallSurfacesARealFailure(t *testing.T) {
	r := newFakeRunner()
	r.respond("schtasks /Delete", Result{ExitCode: 1, Stderr: "ERROR: Access is denied."})
	w := &windowsTask{run: r}

	err := w.Uninstall(t.Context())
	if err == nil {
		t.Fatal("Uninstall succeeded despite schtasks reporting access denied")
	}
	// Only "cannot find" is treated as already-done. Anything else is a real failure and must not be
	// silently swallowed into a success, or an uninstall that did nothing would report that it worked.
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Errorf("the error drops the reason: %v", err)
	}
}

func TestWindowsStartAndStopRefuseWhenNotInstalled(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, taskMissing())
	w := &windowsTask{run: r}

	if err := w.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Start returned %v, want ErrNotInstalled", err)
	}
	if err := w.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Stop returned %v, want ErrNotInstalled", err)
	}
}

func TestWindowsStartRunsTheTask(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, taskExists())
	w := &windowsTask{run: r}

	if err := w.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !r.ran("schtasks /Run /TN " + windowsTaskName) {
		t.Errorf("the task was never run; ran: %v", r.lines())
	}
}

func TestWindowsStopToleratesAnAlreadyStoppedTask(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, taskExists())
	r.respond("schtasks /End", Result{ExitCode: 1, Stderr: "ERROR: The system cannot find the file specified."})
	w := &windowsTask{run: r}

	// Stopping something already stopped is a success by Manager's contract — scripts run stop before
	// uninstall without checking, and that must not be an error.
	if err := w.Stop(t.Context()); err != nil {
		t.Fatalf("Stop on an already-stopped task: %v", err)
	}
}

func TestWindowsStatusReportsEachState(t *testing.T) {
	cases := []struct {
		name          string
		query         Result
		wantInstalled bool
		wantRunning   bool
	}{
		{"not installed", taskMissing(), false, false},
		{"installed and waiting", taskExists(), true, false},
		{"running", Result{ExitCode: 0, Stdout: "TaskName:      \\" + windowsTaskName + "\r\nStatus:        Running"}, true, true},
		{"disabled", Result{ExitCode: 0, Stdout: "Status:        Disabled"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.respond(queryLine, tc.query)
			w := &windowsTask{run: r}

			state, err := w.Status(t.Context())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if state.Installed != tc.wantInstalled || state.Running != tc.wantRunning {
				t.Errorf("got %+v, want installed=%v running=%v", state, tc.wantInstalled, tc.wantRunning)
			}
		})
	}
}

func TestTaskStatusFieldParsing(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"CRLF output", "TaskName: \\X\r\nStatus:        Ready\r\n", "Ready"},
		{"LF output", "Status: Running\n", "Running"},
		{"case-insensitive key", "STATUS: Ready", "Ready"},
		// A localized Windows names the key in its own language. Degrading to "" — which the caller shows
		// as "unknown" — is the honest outcome; the Installed flag comes from the exit code and stays right.
		{"localized key", "Statut:        Prêt", ""},
		{"no status line", "TaskName: \\X", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskStatusField(tc.out); got != tc.want {
				t.Errorf("taskStatusField(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

func TestWindowsHasNoDefinitionPath(t *testing.T) {
	w := &windowsTask{run: newFakeRunner()}

	// Task Scheduler keeps definitions in a registry-backed store. Reporting an invented file path would
	// send someone looking for a file that does not exist.
	path, err := w.DefinitionPath()
	if err != nil {
		t.Fatalf("DefinitionPath: %v", err)
	}
	if path != "" {
		t.Errorf("DefinitionPath = %q, want empty", path)
	}
}
