// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemonctl

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// stubManager is a Manager whose every answer a test dictates.
type stubManager struct {
	state           State
	statusErr       error
	installErr      error
	startErr        error
	stopErr         error
	startsOnInstall bool

	installed  []string
	uninstalls int
	starts     int
	stops      int
}

func (s *stubManager) Install(_ context.Context, binary string) error {
	s.installed = append(s.installed, binary)
	return s.installErr
}
func (s *stubManager) Uninstall(context.Context) error       { s.uninstalls++; return nil }
func (s *stubManager) Start(context.Context) error           { s.starts++; return s.startErr }
func (s *stubManager) Stop(context.Context) error            { s.stops++; return s.stopErr }
func (s *stubManager) Status(context.Context) (State, error) { return s.state, s.statusErr }
func (s *stubManager) StartsOnInstall() bool                 { return s.startsOnInstall }
func (s *stubManager) DefinitionPath() (string, error)       { return "/tmp/norite-daemon.service", nil }

// testRoot builds a root command that handles exit codes the way cliapp.New does.
//
// The no-op ExitErrHandler is the load-bearing part. urfave/cli's default prints the error and calls
// os.Exit from inside Run, which would take the test binary with it — and, in production, would bypass
// cmd/app/main.go entirely. Mirroring cliapp here means these tests exercise the real arrangement rather
// than a more convenient one. cliapp cannot simply be imported: it depends on this package.
func testRoot(out *bytes.Buffer) *cli.Command {
	return &cli.Command{
		Name:           "norite",
		Writer:         out,
		ErrWriter:      out,
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Flags:          []cli.Flag{&cli.BoolFlag{Name: "json"}},
		Commands:       []*cli.Command{GroupCommand("test")},
	}
}

// runCommand builds a root command around the daemon group, substitutes mgr, and runs argv.
func runCommand(t *testing.T, mgr Manager, argv ...string) (stdout string, err error) {
	t.Helper()

	previous := managerFor
	managerFor = func() (Manager, error) { return mgr, nil }
	t.Cleanup(func() { managerFor = previous })
	// No socket unless the test set one with answerStop: the real one would be the daemon of whoever runs
	// the suite, and these commands stop it.
	if !stopStubbed {
		answerStop(t, stopNotAsked)
	}
	if !statusStubbed {
		answerStatus(t, socketAnswer{})
	}

	var out bytes.Buffer
	err = testRoot(&out).Run(t.Context(), append([]string{"norite"}, argv...))
	return out.String(), err
}

// statusStubbed is whether a test has said what the socket answers when asked who is there.
var statusStubbed bool

// answerStatus makes the socket answer every status question with answer for the rest of the test.
func answerStatus(t *testing.T, answer socketAnswer) {
	t.Helper()
	previous, was := socketStatus, statusStubbed
	socketStatus = func(context.Context, string) socketAnswer { return answer }
	statusStubbed = true
	t.Cleanup(func() { socketStatus, statusStubbed = previous, was })
}

// stopStubbed is whether a test has said what the socket answers a stop request.
var stopStubbed bool

// answerStop makes the socket answer every stop request with outcome for the rest of the test, and
// returns how many times it was asked.
func answerStop(t *testing.T, outcome stopOutcome) *int {
	t.Helper()
	asked := new(int)
	previous, was := socketStop, stopStubbed
	socketStop = func(context.Context, string) stopOutcome { *asked++; return outcome }
	stopStubbed = true
	t.Cleanup(func() { socketStop, stopStubbed = previous, was })
	return asked
}

func TestStatusTellsTheUserWhatToDoNext(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		out, _ := runCommand(t, &stubManager{}, "daemon", "status")
		if !strings.Contains(out, "norite daemon install") {
			t.Errorf("the output does not name the command that fixes this:\n%s", out)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		out, _ := runCommand(t, &stubManager{state: State{Installed: true, Detail: "inactive"}}, "daemon", "status")
		if !strings.Contains(out, "norite daemon start") {
			t.Errorf("the output does not name the command that fixes this:\n%s", out)
		}
	})
}

func TestInstallRegistersTheLocatedBinary(t *testing.T) {
	isolate(t)

	dir := t.TempDir()
	binary := writeExecutable(t, dir, daemonBinaryName+exeSuffix())

	mgr := &stubManager{}
	out, err := runCommand(t, mgr, "daemon", "install", "--daemon-binary", binary)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	if len(mgr.installed) != 1 || mgr.installed[0] != binary {
		t.Fatalf("installed %v, want exactly [%s]", mgr.installed, binary)
	}
	// The path that got baked into the service definition is the single most useful thing to print: it is
	// what the user must fix if they later move or reinstall the binary.
	if !strings.Contains(out, binary) {
		t.Errorf("the output does not report which executable was registered:\n%s", out)
	}
	// Install and start are separate verbs, so the output has to say so or the user will assume it is up.
	if !strings.Contains(out, "norite daemon start") {
		t.Errorf("the output does not say how to start it:\n%s", out)
	}
}

func TestInstallReportsAMissingDaemonBinary(t *testing.T) {
	isolate(t)

	mgr := &stubManager{}
	_, err := runCommand(t, mgr, "daemon", "install")
	if err == nil {
		t.Fatal("install succeeded with no daemon binary anywhere")
	}
	if len(mgr.installed) != 0 {
		t.Errorf("a service was registered despite the binary not being found: %v", mgr.installed)
	}
}

func TestStartAndStopReportWhatHappened(t *testing.T) {
	mgr := &stubManager{}

	out, err := runCommand(t, mgr, "daemon", "start")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if mgr.starts != 1 {
		t.Errorf("Start called %d times, want 1", mgr.starts)
	}
	if !strings.Contains(out, "Started") {
		t.Errorf("start printed nothing useful:\n%s", out)
	}

	mgr = &stubManager{}
	out, err = runCommand(t, mgr, "daemon", "stop")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if mgr.stops != 1 {
		t.Errorf("Stop called %d times, want 1", mgr.stops)
	}
	if !strings.Contains(out, "Stopped") {
		t.Errorf("stop printed nothing useful:\n%s", out)
	}
}

func TestStartSurfacesNotInstalledUnchanged(t *testing.T) {
	_, err := runCommand(t, &stubManager{startErr: ErrNotInstalled}, "daemon", "start")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("start returned %v, want ErrNotInstalled to reach the user intact", err)
	}
	// The sentinel already tells the user to run install first. Wrapping it in another layer would bury
	// the one actionable sentence.
	if !strings.Contains(err.Error(), "norite daemon install") {
		t.Errorf("the error lost its advice: %v", err)
	}
}

func TestRestartDoesNotStartAfterAFailedStop(t *testing.T) {
	mgr := &stubManager{state: State{Installed: true, Running: true},
		stopErr: errors.New("systemctl --user stop failed")}

	_, err := runCommand(t, mgr, "daemon", "restart")
	if err == nil {
		t.Fatal("restart succeeded despite the stop failing")
	}
	// Reached, and failed: a restart refused before it tried would also start nothing.
	if mgr.stops != 1 {
		t.Fatalf("the stop was tried %d times, want once", mgr.stops)
	}
	// Starting anyway would either do nothing or produce a second daemon, and the second is exactly what
	// the single-instance lock exists to prevent — better to stop and say so.
	if mgr.starts != 0 {
		t.Errorf("restart started the daemon after failing to stop it")
	}
}

func TestRestartStopsThenStarts(t *testing.T) {
	mgr := &stubManager{state: State{Installed: true, Running: true}}

	if _, err := runCommand(t, mgr, "daemon", "restart"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if mgr.stops != 1 || mgr.starts != 1 {
		t.Errorf("stops=%d starts=%d, want one of each", mgr.stops, mgr.starts)
	}
}

func TestUninstallRemovesTheService(t *testing.T) {
	mgr := &stubManager{}

	out, err := runCommand(t, mgr, "daemon", "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if mgr.uninstalls != 1 {
		t.Errorf("Uninstall called %d times, want 1", mgr.uninstalls)
	}
	if !strings.Contains(out, "Removed") {
		t.Errorf("uninstall printed nothing useful:\n%s", out)
	}
}

func TestEveryDaemonSubcommandIsDocumented(t *testing.T) {
	group := GroupCommand("test")

	want := []string{"install", "uninstall", "start", "stop", "restart", "status"}
	got := map[string]*cli.Command{}
	for _, sub := range group.Commands {
		got[sub.Name] = sub
	}

	for _, name := range want {
		sub, ok := got[name]
		if !ok {
			t.Errorf("`norite daemon %s` is missing", name)
			continue
		}
		// A scriptable CLI is partly judged on whether `--help` answers the question. An undescribed
		// subcommand shows up as a blank line in the parent's help output.
		if strings.TrimSpace(sub.Usage) == "" {
			t.Errorf("`norite daemon %s` has no usage line", name)
		}
	}
}

func TestUnsupportedPlatformIsExplainedRatherThanCrashing(t *testing.T) {
	_, err := newFor("plan9", newFakeRunner())
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("newFor(plan9) returned %v, want ErrUnsupportedPlatform", err)
	}
	// The daemon binary itself is portable Go and runs fine there; only the service definition is missing.
	// Saying so leaves the user able to wire it into whatever supervisor they already run.
	if !strings.Contains(err.Error(), "plan9") {
		t.Errorf("the error does not name the platform: %v", err)
	}
}

func TestNewForReturnsTheRightBackendPerPlatform(t *testing.T) {
	cases := map[string]any{
		"linux":   &systemdUser{},
		"darwin":  &launchdAgent{},
		"windows": &windowsTask{},
	}
	for goos, want := range cases {
		t.Run(goos, func(t *testing.T) {
			got, err := newFor(goos, newFakeRunner())
			if err != nil {
				t.Fatalf("newFor(%q): %v", goos, err)
			}
			if gotType, wantType := typeName(got), typeName(want); gotType != wantType {
				t.Errorf("newFor(%q) = %s, want %s", goos, gotType, wantType)
			}
		})
	}
}

func typeName(v any) string { return strings.TrimPrefix(sprintType(v), "*") }

func sprintType(v any) string {
	switch v.(type) {
	case *systemdUser:
		return "*systemdUser"
	case *launchdAgent:
		return "*launchdAgent"
	case *windowsTask:
		return "*windowsTask"
	default:
		return "unknown"
	}
}
