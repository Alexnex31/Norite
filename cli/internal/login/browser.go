package login

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Opening the person's browser.
//
// Done by exec rather than by taking a dependency. github.com/pkg/browser is the usual answer and it is
// roughly two hundred lines wrapping the three commands below; this repository names and justifies every
// dependency it has, and a runtime.GOOS switch does not earn an entry in that list.
//
// Never fatal. A machine with no desktop session, a locked-down container, an unknown platform — all of
// them end with the sign-in URL printed instead, which is a working flow rather than a failure, and is the
// same thing `--no-browser` asks for deliberately.

// openBrowser asks the desktop to open target.
func openBrowser(ctx context.Context, target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", target)
	case "windows":
		// Not `cmd /c start`, which is the more familiar incantation and the wrong one. `start` treats `&`
		// as a command separator and its first quoted argument as a window title, so it is a
		// command-injection shape around a string this program builds — safe today, and one refactor away
		// from not being. rundll32 takes the URL as a single argv entry with no shell anywhere in the path.
		cmd = exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", target)
	default:
		return fmt.Errorf("no way to open a browser on %s", runtime.GOOS)
	}

	// Start, not Run: xdg-open blocks for the life of the browser on some desktops, and this must not wait
	// for someone to close their window. The child is reaped by a goroutine so it does not linger as a
	// zombie for the rest of the flow's fifteen minutes.
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
