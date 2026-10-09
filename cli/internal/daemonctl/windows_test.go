// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"encoding/xml"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
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

// The locale-independent existence check. schtasks translates its messages and its column headers but
// never a task name, so listing is what "is it registered" is actually decided by.
var listLine = "schtasks /Query /FO CSV /NH"

func taskListed() Result {
	return Result{ExitCode: 0, Stdout: `"\` + windowsTaskName + `","N/A","Ready"` + "\r\n"}
}

func taskNotListed() Result {
	return Result{ExitCode: 0, Stdout: `"\Microsoft\Windows\Defrag\ScheduledDefrag","N/A","Ready"` + "\r\n"}
}

// installed runs Install against a runner that answers whoami with user, and returns the definition
// schtasks was handed, decoded, with the command lines that were run.
func installed(t *testing.T, user, binary string) (definition string, raw []byte, lines []string, err error) {
	t.Helper()
	r := newFakeRunner()
	r.respond("whoami", Result{Stdout: user})
	var path string
	r.during = func(name string, args []string) {
		for i, arg := range args {
			if name == "schtasks" && arg == "/XML" && i+1 < len(args) {
				path = args[i+1]
				// Read while the command "runs": the file is removed once it returns.
				raw, _ = os.ReadFile(path)
			}
		}
	}
	err = (&windowsTask{run: r}).Install(t.Context(), binary)
	if path != "" {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the definition was left behind at %s: %v", path, statErr)
		}
	}
	if len(raw) >= 2 {
		units := make([]uint16, 0, len(raw)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
		}
		definition = string(utf16.Decode(units))
	}
	return definition, raw, r.lines(), err
}

func TestWindowsInstallRegistersADefinitionForThisUsersLogon(t *testing.T) {
	definition, raw, lines, err := installed(t, "DESKTOP-1\\ada\r\n", `C:\Program Files\Norite\norite-daemon.exe`)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(lines) != 2 || lines[0] != "whoami" {
		t.Fatalf("ran %v", lines)
	}
	for _, want := range []string{"schtasks /Create", "/TN " + windowsTaskName, "/XML ", "/F"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("the schtasks invocation is missing %q:\n%s", want, lines[1])
		}
	}
	// The flags took Task Scheduler's defaults, which none of them can change.
	for _, gone := range []string{"/SC", "/TR", "/RL"} {
		if strings.Contains(lines[1], gone) {
			t.Errorf("the invocation still passes %s beside a definition:\n%s", gone, lines[1])
		}
	}

	// The bytes are what the first line says they are, or schtasks calls the file malformed.
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatalf("the definition does not begin with a UTF-16 little-endian byte-order mark: % x", raw[:min(len(raw), 4)])
	}
	if !strings.HasPrefix(definition, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("the definition does not declare the encoding it is written in:\n%s", definition)
	}
	var parsed struct {
		Trigger   string `xml:"Triggers>LogonTrigger>UserId"`
		Principal struct {
			User      string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principals>Principal"`
		Settings struct {
			Instances   string `xml:"MultipleInstancesPolicy"`
			NoBattery   string `xml:"DisallowStartIfOnBatteries"`
			StopBattery string `xml:"StopIfGoingOnBatteries"`
			Limit       string `xml:"ExecutionTimeLimit"`
			Restarts    string `xml:"RestartOnFailure>Count"`
		} `xml:"Settings"`
		Command string `xml:"Actions>Exec>Command"`
	}
	// encoding/xml reads UTF-8, which the decoded string is; the declaration is for schtasks.
	body := strings.Replace(definition, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("the definition is not well-formed: %v\n%s", err, definition)
	}
	// This user's logon, and run as them: not anybody's, which a standard account may not register.
	if parsed.Trigger != `DESKTOP-1\ada` || parsed.Principal.User != `DESKTOP-1\ada` {
		t.Errorf("the task is for %q and runs as %q", parsed.Trigger, parsed.Principal.User)
	}
	// LeastPrivilege, not HighestAvailable: the daemon needs no privilege beyond the user's own.
	if parsed.Principal.RunLevel != "LeastPrivilege" || parsed.Principal.LogonType != "InteractiveToken" {
		t.Errorf("principal: %+v", parsed.Principal)
	}
	if parsed.Settings.NoBattery != "false" || parsed.Settings.StopBattery != "false" {
		t.Errorf("the daemon stops for a battery: %+v", parsed.Settings)
	}
	if parsed.Settings.Limit != "PT0S" {
		t.Errorf("the daemon is ended after %s", parsed.Settings.Limit)
	}
	if parsed.Settings.Instances != "IgnoreNew" || parsed.Settings.Restarts != "3" {
		t.Errorf("settings: %+v", parsed.Settings)
	}
	// A path, not a command line: nothing re-parses it, so the space needs no quotes.
	if parsed.Command != `C:\Program Files\Norite\norite-daemon.exe` {
		t.Errorf("the task runs %q", parsed.Command)
	}
}

// A path or an account name is text in a document, and a document has characters that end its elements.
func TestWindowsDefinitionEscapesWhatItIsGiven(t *testing.T) {
	definition, _, _, err := installed(t, `CORP&CO\a<b>`, `C:\R&D\"x"\norite-daemon.exe`)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	var parsed struct {
		User    string `xml:"Principals>Principal>UserId"`
		Command string `xml:"Actions>Exec>Command"`
	}
	body := strings.Replace(definition, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("the definition is not well-formed: %v\n%s", err, definition)
	}
	if parsed.User != `CORP&CO\a<b>` || parsed.Command != `C:\R&D\"x"\norite-daemon.exe` {
		t.Errorf("round trip: %q, %q", parsed.User, parsed.Command)
	}
}

func TestWindowsInstallRefusesWhatItCannotName(t *testing.T) {
	// No account name, or one that is not a name: the task would be nobody's.
	for _, user := range []string{"", "  \r\n", "a\x00b"} {
		if _, _, lines, err := installed(t, user, `C:\norite-daemon.exe`); err == nil || len(lines) != 1 {
			t.Errorf("whoami answered %q: err=%v, ran %v", user, err, lines)
		}
	}
	// A control character in the path is refused before anything is asked.
	if _, _, lines, err := installed(t, `PC\ada`, "C:\\a\nb.exe"); err == nil || len(lines) != 0 {
		t.Errorf("a path with a newline: err=%v, ran %v", err, lines)
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
	r.respond(listLine, taskNotListed())
	w := &windowsTask{run: r}

	// Uninstall's contract is that the task is gone afterwards, and it already is.
	if err := w.Uninstall(t.Context()); err != nil {
		t.Fatalf("Uninstall on an absent task: %v", err)
	}
}

func TestWindowsUninstallSurfacesARealFailure(t *testing.T) {
	r := newFakeRunner()
	r.respond("schtasks /Delete", Result{ExitCode: 1, Stderr: "ERROR: Access is denied."})
	r.respond(listLine, taskListed())
	w := &windowsTask{run: r}

	err := w.Uninstall(t.Context())
	if err == nil {
		t.Fatal("Uninstall succeeded despite schtasks reporting access denied")
	}
	// Only a task that is genuinely absent counts as already-done, and the listing is what decides that.
	// Anything else is a real failure and must not be swallowed into a success, or an uninstall that did
	// nothing would report that it worked.
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Errorf("the error drops the reason: %v", err)
	}
}

func TestWindowsStartAndStopRefuseWhenNotInstalled(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, taskMissing())
	r.respond(listLine, taskNotListed())
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
	r.respond(listLine, taskListed())
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
		// as "unknown" — is the honest outcome, and Installed stays correct regardless because it comes
		// from the task listing rather than from any translated text.
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

// The bug this replaced: "task is absent" was decided by matching the English string "CANNOT FIND", so on
// a localized Windows every absent-task path broke at once — `norite daemon status` exited 1 instead of the
// documented 2, Start and Stop reported a spurious error instead of ErrNotInstalled, and Uninstall and Stop
// lost the idempotence Manager promises.
//
// Each case below is a real schtasks "not found" message in another language. None of them contains
// "cannot find", and none of them needs to: absence is decided by the task listing.
func TestWindowsAbsentTaskIsDetectedInAnyLocale(t *testing.T) {
	localized := map[string]string{
		"french":   "ERREUR: Le système ne trouve pas le fichier spécifié.",
		"german":   "FEHLER: Das System kann die angegebene Datei nicht finden.",
		"spanish":  "ERROR: El sistema no puede encontrar el archivo especificado.",
		"japanese": "エラー: 指定されたファイルが見つかりません。",
		"russian":  "ОШИБКА: Не удается найти указанный файл.",
	}

	for name, message := range localized {
		t.Run(name, func(t *testing.T) {
			r := newFakeRunner()
			r.respond(queryLine, Result{ExitCode: 1, Stderr: message})
			r.respond(listLine, taskNotListed())
			w := &windowsTask{run: r}

			state, err := w.Status(t.Context())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if state.Installed {
				t.Error("an absent task was reported as installed")
			}

			// The exit codes the CLI documents hang off this, and so does Stop's idempotence.
			if err := w.Start(t.Context()); !errors.Is(err, ErrNotInstalled) {
				t.Errorf("Start returned %v, want ErrNotInstalled", err)
			}
			if err := w.Stop(t.Context()); !errors.Is(err, ErrNotInstalled) {
				t.Errorf("Stop returned %v, want ErrNotInstalled", err)
			}
		})
	}
}

// The other half of the same decision: a real failure must stay a failure in every locale too, rather than
// being downgraded to "not installed" because the message could not be parsed.
func TestWindowsLocalizedFailureOnAnExistingTaskIsStillAnError(t *testing.T) {
	r := newFakeRunner()
	r.respond(queryLine, Result{ExitCode: 1, Stderr: "FEHLER: Zugriff verweigert."})
	r.respond(listLine, taskListed())
	w := &windowsTask{run: r}

	if _, err := w.Status(t.Context()); err == nil {
		t.Fatal("a permissions failure on an existing task was reported as not installed")
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
