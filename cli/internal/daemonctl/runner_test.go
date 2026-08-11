package daemonctl

import (
	"context"
	"strings"
	"sync"
)

// call is one recorded command invocation.
type call struct {
	Name string
	Args []string
}

func (c call) String() string { return commandLine(c.Name, c.Args) }

// fakeRunner records every command and answers from a scripted table.
//
// The whole point of the Runner interface: systemd, launchd, and Task Scheduler backends are all
// constructible and assertable from a single Linux test machine, so a mistake in the launchd command line
// is caught here rather than by the first macOS user.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call

	// responses maps a command line prefix to what running it produces. The longest matching prefix wins,
	// so a table can set a default for `systemctl --user` and override one specific subcommand.
	responses map[string]Result

	// err, when non-nil, is returned instead of running anything.
	err error
}

func newFakeRunner() *fakeRunner { return &fakeRunner{responses: map[string]Result{}} }

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call{Name: name, Args: args})
	if f.err != nil {
		return Result{}, f.err
	}

	line := commandLine(name, args)
	best := ""
	for prefix := range f.responses {
		if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return Result{}, nil
	}
	return f.responses[best], nil
}

func (f *fakeRunner) respond(prefix string, res Result) { f.responses[prefix] = res }

// lines returns every recorded invocation as a command line.
func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.String())
	}
	return out
}

// ran reports whether any recorded invocation matches the given command line exactly.
func (f *fakeRunner) ran(line string) bool {
	for _, got := range f.lines() {
		if got == line {
			return true
		}
	}
	return false
}
