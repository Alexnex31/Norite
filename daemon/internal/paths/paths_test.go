package paths

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStateDirFollowsEachPlatformsConvention(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // what os.UserHomeDir reads on Windows
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", "")

	cases := []struct {
		goos string
		want string
	}{
		{"linux", filepath.Join(home, ".local", "state", "norite")},
		{"freebsd", filepath.Join(home, ".local", "state", "norite")},
		{"darwin", filepath.Join(home, "Library", "Application Support", "Norite")},
		{"windows", filepath.Join(home, "AppData", "Local", "Norite")},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			got, err := stateDirFor(tc.goos)
			if err != nil {
				t.Fatalf("stateDirFor(%q): %v", tc.goos, err)
			}
			if got != tc.want {
				t.Errorf("stateDirFor(%q) = %q, want %q", tc.goos, got, tc.want)
			}
		})
	}
}

func TestStateDirHonorsXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	custom := t.TempDir()
	t.Setenv("XDG_STATE_HOME", custom)

	got, err := stateDirFor("linux")
	if err != nil {
		t.Fatalf("stateDirFor: %v", err)
	}
	if want := filepath.Join(custom, "norite"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStateDirIgnoresARelativeXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// The XDG spec says a relative value must be treated as unset. Honoring it would be actively harmful
	// here: the daemon is normally started by systemd, whose working directory is the user's home but is
	// not guaranteed to be, so a relative path would put the lock file somewhere that moves.
	t.Setenv("XDG_STATE_HOME", "relative/path")

	got, err := stateDirFor("linux")
	if err != nil {
		t.Fatalf("stateDirFor: %v", err)
	}
	if want := filepath.Join(home, ".local", "state", "norite"); got != want {
		t.Errorf("a relative XDG_STATE_HOME was honored: got %q, want the default %q", got, want)
	}
}

func TestStateDirPrefersLocalAppDataOnWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)

	got, err := stateDirFor("windows")
	if err != nil {
		t.Fatalf("stateDirFor: %v", err)
	}
	if want := filepath.Join(local, "Norite"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStateDirIsCreatedPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	dir, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Later milestones put plugin capability grants and pinned .wasm hashes in here (docs/architecture.md
	// §8). A directory another local user can write to would let them rewrite what a plugin is allowed to
	// do, so the mode is established now rather than migrated later.
	if got := info.Mode().Perm(); got != fs.FileMode(0o700) {
		t.Errorf("state directory mode = %#o, want 0700", got)
	}
}

func TestStateDirIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	first, err := StateDir()
	if err != nil {
		t.Fatalf("first StateDir: %v", err)
	}
	// Called on every daemon start, so an existing directory must not be an error.
	second, err := StateDir()
	if err != nil {
		t.Fatalf("second StateDir on an existing directory: %v", err)
	}
	if first != second {
		t.Errorf("StateDir is not stable: %q then %q", first, second)
	}
}

func TestLockAndLogSitInsideTheStateDir(t *testing.T) {
	dir := t.TempDir()
	if got, want := LockFile(dir), filepath.Join(dir, "daemon.lock"); got != want {
		t.Errorf("LockFile = %q, want %q", got, want)
	}
	if got, want := LogFile(dir), filepath.Join(dir, "daemon.log"); got != want {
		t.Errorf("LogFile = %q, want %q", got, want)
	}
}
