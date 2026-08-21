package login

import (
	"os"
	"os/exec"
	"runtime"
)

// Deciding whether a browser on *this* machine can be reached.
//
// # Why this exists
//
// M8's loopback listener binds 127.0.0.1, so the browser that finishes the sign-in has to be on the same
// machine as the CLI. Over SSH it is not: the link can be pasted into a browser on a phone, but that
// browser cannot reach the server's loopback port, so the flow hangs until it times out. The device-code
// flow (M9) is what that case falls back to.
//
// # Detected rather than asked for
//
// The roadmap settles this, and the reason is that the person on the far end of an SSH session should not
// have to know which of two sign-in flows their situation calls for. `--device-code` forces it for anyone
// who does want to choose; `--no-browser` keeps M8's meaning — print the URL and keep listening — which is
// right for a forwarded port and is a different request entirely.
//
// # Which way to be wrong
//
// Conservative toward the flow that needs nothing local. A wrong "reachable" strands somebody at a URL that
// nothing will open and a listener nothing can reach; a wrong "not reachable" costs them one extra step,
// on their own screen, which they can see and act on.

// browserEnv is the environment lookup, indirected so the platform rules are testable without setting real
// variables in a process other tests share.
type browserEnv struct {
	lookup   func(string) (string, bool)
	lookPath func(string) (string, error)
	goos     string
}

func realBrowserEnv() browserEnv {
	return browserEnv{lookup: os.LookupEnv, lookPath: exec.LookPath, goos: runtime.GOOS}
}

// browserReachable reports whether opening a URL here would put it in front of the person running this.
func browserReachable() bool { return realBrowserEnv().browserReachable() }

func (e browserEnv) browserReachable() bool {
	// The signal that is right on every platform, and the case this milestone exists for. A desktop
	// machine being administered over SSH has all the local trappings of one that could open a browser —
	// DISPLAY may even be set, by X forwarding — and the browser it opens is on the wrong screen.
	//
	// Checked first for that reason: on macOS `open` always succeeds, and would silently launch Safari on
	// a machine nobody is sitting at.
	for _, name := range []string{"SSH_CONNECTION", "SSH_TTY", "SSH_CLIENT"} {
		if value, ok := e.lookup(name); ok && value != "" {
			return false
		}
	}

	switch e.goos {
	case "darwin", "windows":
		// `open` and `rundll32` are part of the OS, and a session that is not a desktop one is the SSH
		// case above. There is no third state worth guessing at here.
		return true

	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		// Two conditions, and both are needed. A display server means somebody is looking at something;
		// an opener means there is a way to put a URL in front of them. A container with xdg-open
		// installed and no display has the second and not the first, and a bare tty has neither.
		if !e.hasDisplay() {
			return false
		}
		_, err := e.lookPath("xdg-open")
		return err == nil

	default:
		// An unknown platform has no opener this program knows how to call — openBrowser refuses it
		// outright — so the honest answer is the flow that needs nothing local.
		return false
	}
}

// hasDisplay reports whether a display server is addressable.
func (e browserEnv) hasDisplay() bool {
	for _, name := range []string{"WAYLAND_DISPLAY", "DISPLAY"} {
		if value, ok := e.lookup(name); ok && value != "" {
			return true
		}
	}
	return false
}
