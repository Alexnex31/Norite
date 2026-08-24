package login

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The platform rules, exercised without touching the real environment — which matters more than usual
// here, because the answer depends on variables the test process itself inherits, so a test that set them
// for real would pass or fail according to where it was run.

func TestBrowserReachabilityByPlatform(t *testing.T) {
	for _, tc := range []struct {
		why   string
		env   map[string]string
		goos  string
		found bool
		want  bool
	}{
		{
			why: "a Linux desktop under X", env: map[string]string{"DISPLAY": ":0"},
			goos: "linux", found: true, want: true,
		},
		{
			why: "a Linux desktop under Wayland", env: map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			goos: "linux", found: true, want: true,
		},
		{
			why: "a Linux server with no display at all", env: map[string]string{},
			goos: "linux", found: true, want: false,
		},
		{
			why:  "a container with a display and no opener installed",
			env:  map[string]string{"DISPLAY": ":0"},
			goos: "linux", found: false, want: false,
		},
		{
			why:  "an empty DISPLAY, which is set and useless",
			env:  map[string]string{"DISPLAY": ""},
			goos: "linux", found: true, want: false,
		},

		// The case this milestone exists for, and the reason SSH is checked before anything else. A
		// desktop administered over SSH has every local sign of being able to open a browser — DISPLAY may
		// even be set, by X forwarding — and on macOS `open` would cheerfully launch Safari on a screen
		// nobody is sitting at.
		{
			why:  "an SSH session on a desktop machine",
			env:  map[string]string{"DISPLAY": ":0", "SSH_CONNECTION": "10.0.0.2 51763 10.0.0.9 22"},
			goos: "linux", found: true, want: false,
		},
		{
			why:  "an SSH session on macOS, where the opener always exists",
			env:  map[string]string{"SSH_TTY": "/dev/ttys001"},
			goos: "darwin", found: true, want: false,
		},

		{why: "a Mac at a desk", env: map[string]string{}, goos: "darwin", found: true, want: true},
		{why: "a Windows desktop", env: map[string]string{}, goos: "windows", found: true, want: true},

		// openBrowser refuses an unknown platform outright, so the honest answer is the flow that needs
		// nothing local rather than one that will fail after somebody has waited for it.
		{
			why: "a platform this program cannot open a browser on", env: map[string]string{},
			goos: "plan9", found: true, want: false,
		},
	} {
		env := browserEnv{
			lookup: func(name string) (string, bool) {
				value, ok := tc.env[name]
				return value, ok
			},
			lookPath: func(string) (string, error) {
				if tc.found {
					return "/usr/bin/xdg-open", nil
				}
				return "", errors.New("not found")
			},
			goos: tc.goos,
		}
		assert.Equal(t, tc.want, env.browserReachable(), tc.why)
	}
}

// The real one is wired to the real environment. Asserting its *answer* would be asserting where the test
// runs, so this asserts only that it is wired at all and does not panic.
func TestTheRealDetectionIsWiredUp(t *testing.T) {
	env := realBrowserEnv()
	assert.NotNil(t, env.lookup)
	assert.NotNil(t, env.lookPath)
	assert.NotEmpty(t, env.goos)
	_ = browserReachable()
}
