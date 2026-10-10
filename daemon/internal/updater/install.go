// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
)

const (
	// The phases of an installation. "swapping" is written before the first program is replaced and
	// "applied" after the second, so a start that finds "swapping" knows the pair may be mixed.
	phaseSwapping = "swapping"
	phaseApplied  = "applied"

	// unconfirmedStarts is how many starts of a new version may end badly before the next one undoes it.
	// The Windows task restarts a failed daemon three times and then stops trying (M23), so the undoing
	// has to happen within those.
	unconfirmedStarts = 2
	// maxBad bounds the list of versions rolled back from.
	maxBad = 32
)

// Pending is an installation not yet confirmed: the new version has been put in place and has not yet
// stayed up.
type Pending struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Phase  string `json:"phase"`
	Starts int    `json:"starts"`
}

// Record is what the updater remembers between runs.
type Record struct {
	Pending *Pending `json:"pending,omitempty"`
	// Bad lists versions installed here and rolled back from. They are not offered again.
	Bad []string `json:"bad,omitempty"`
	// RolledBack is the last version undone, kept until another is installed, so it can be said.
	RolledBack string `json:"rolled_back,omitempty"`
	// LastCheck is when the listing was last read.
	LastCheck time.Time `json:"last_check,omitzero"`
	// Available is the version the last check chose, or "".
	Available string `json:"available,omitempty"`
	// LastError is why the last check or installation failed, or "".
	LastError string `json:"last_error,omitempty"`
	// Mode and Channel are what the account chose (ipc.UpdateAuto and the rest), or "" for the default.
	// They are here and not in config.toml because they are this machine's and the daemon's own, as the
	// port for scripts' switch is: the daemon interprets nobody's config file (M21).
	Mode    string `json:"mode,omitempty"`
	Channel string `json:"channel,omitempty"`
}

// Outcome is what a start found in the record.
type Outcome int

const (
	// Nothing was pending.
	Nothing Outcome = iota
	// Counted: an unconfirmed installation is running, and this start is counted against it.
	Counted
	// Recovered: an installation that was interrupted half-way has been undone. The pair on disk is the
	// one this daemon belongs to.
	Recovered
	// RolledBack: the new version did not stay up and the previous pair is back in place. The running
	// daemon is the version just undone, and must be replaced by the one now on disk.
	RolledBack
)

// Installer puts a staged release in place of the running pair, and puts the previous pair back.
type Installer struct {
	// Dir is the updater's directory in the state directory: the record, and the previous pair.
	Dir string
	// BinDir is where the two installed programs are.
	BinDir string
	GOOS   string

	mu sync.Mutex
}

func (in *Installer) recordPath() string  { return filepath.Join(in.Dir, "state.json") }
func (in *Installer) previousDir() string { return filepath.Join(in.Dir, "previous") }

func (in *Installer) programs() []string {
	return []string{cliName + exeSuffix(in.GOOS), daemonName + exeSuffix(in.GOOS)}
}

// Load reads the record. One that is missing or unreadable is an empty one: the worst that follows is a
// version offered again, or an update that is never confirmed and never undone.
func (in *Installer) Load() Record {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.load()
}

func (in *Installer) load() Record {
	var r Record
	if data, err := os.ReadFile(in.recordPath()); err == nil && len(data) < 1<<20 {
		_ = json.Unmarshal(data, &r)
	}
	if r.Pending != nil && (r.Pending.Phase != phaseSwapping && r.Pending.Phase != phaseApplied) {
		r.Pending = nil
	}
	return r
}

// Update changes the record under the installer's lock.
func (in *Installer) Update(change func(*Record)) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	r := in.load()
	change(&r)
	return in.save(r)
}

func (in *Installer) save(r Record) error {
	if len(r.Bad) > maxBad {
		r.Bad = r.Bad[len(r.Bad)-maxBad:]
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return err
	}
	err = atomicfile.Write(in.recordPath(), append(data, '\n'), atomicfile.Options{Mode: 0o600})
	if errors.Is(err, atomicfile.ErrNotDurable) {
		return nil
	}
	return err
}

// Install replaces the installed pair with the staged one. from is the version running.
//
// The caller has stopped everything that writes: this runs after the daemon's run has returned, with its
// lock still held. The current pair is copied aside first, and the record says "swapping" before the first
// program is replaced, so that a machine switched off in the middle is put right at the next start.
func (in *Installer) Install(staged Staged, from string) error {
	in.mu.Lock()
	defer in.mu.Unlock()

	previous := in.previousDir()
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	if err := os.MkdirAll(previous, 0o700); err != nil {
		return err
	}
	for _, name := range in.programs() {
		if err := copyFile(filepath.Join(in.BinDir, name), filepath.Join(previous, name)); err != nil {
			return fmt.Errorf("keeping the current %s: %w", name, err)
		}
	}

	r := in.load()
	r.Pending = &Pending{From: from, To: staged.Release.Version.String(), Phase: phaseSwapping}
	r.RolledBack, r.Available, r.LastError = "", "", ""
	if err := in.save(r); err != nil {
		return fmt.Errorf("recording the installation: %w", err)
	}

	// The command first and the daemon last: a start that finds the new daemon knows both are new.
	sources := map[string]string{cliName + exeSuffix(in.GOOS): staged.CLI, daemonName + exeSuffix(in.GOOS): staged.Daemon}
	for _, name := range in.programs() {
		if err := replaceProgram(filepath.Join(in.BinDir, name), sources[name]); err != nil {
			// Put back whatever was replaced. If that fails too, the record still says "swapping" and the
			// next start tries again.
			if in.restore() == nil {
				r.Pending = nil
				_ = in.save(r)
			}
			return fmt.Errorf("installing %s: %w", name, err)
		}
	}

	r.Pending.Phase = phaseApplied
	if err := in.save(r); err != nil {
		return fmt.Errorf("recording the installation: %w", err)
	}
	_ = os.RemoveAll(filepath.Dir(staged.Daemon))
	return nil
}

// restore puts the previous pair back over the installed one.
func (in *Installer) restore() error {
	for _, name := range in.programs() {
		if err := replaceProgram(filepath.Join(in.BinDir, name), filepath.Join(in.previousDir(), name)); err != nil {
			return fmt.Errorf("restoring %s: %w", name, err)
		}
	}
	return nil
}

// AtStart is asked once, when a daemon starts and holds the lock. running is its version, and crashed is
// whether the run before this one left a traceback (M23).
//
// A new version gets unconfirmedStarts starts that end badly, or one that crashes outright; the start
// after that puts the previous pair back, lists the version as bad, and reports RolledBack. This is not a
// downgrade and does not pass through Choose: it restores what this installation ran and kept.
func (in *Installer) AtStart(running string, crashed bool) (Outcome, error) {
	in.mu.Lock()
	defer in.mu.Unlock()

	r := in.load()
	p := r.Pending
	if p == nil {
		return Nothing, nil
	}

	if p.Phase == phaseSwapping {
		if running == p.To {
			// The daemon is replaced last, so both programs are the new ones and only the record was
			// left behind.
			p.Phase, p.Starts = phaseApplied, 1
			return Counted, in.save(r)
		}
		if err := in.restore(); err != nil {
			return Nothing, err
		}
		r.Pending = nil
		_ = os.RemoveAll(in.previousDir())
		return Recovered, in.save(r)
	}

	if running != p.To {
		// Something else put another version here: an installer run by hand. The record describes an
		// installation that is no longer the one on disk.
		r.Pending = nil
		_ = os.RemoveAll(in.previousDir())
		return Nothing, in.save(r)
	}

	if (crashed && p.Starts >= 1) || p.Starts >= unconfirmedStarts {
		if err := in.restore(); err != nil {
			return Counted, err
		}
		r.Bad = append(r.Bad, p.To)
		r.RolledBack, r.Pending = p.To, nil
		_ = os.RemoveAll(in.previousDir())
		return RolledBack, in.save(r)
	}
	p.Starts++
	return Counted, in.save(r)
}

// Confirm ends an installation's trial: the new version has stayed up. The previous pair is dropped.
func (in *Installer) Confirm() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	r := in.load()
	if r.Pending == nil || r.Pending.Phase != phaseApplied {
		return nil
	}
	r.Pending = nil
	_ = os.RemoveAll(in.previousDir())
	return in.save(r)
}

// CleanExit takes back the count of a start that ended because it was asked to. Only a start that ends
// badly counts against a new version: somebody who restarts their machine twice in the ten minutes after
// an update has not found a bad release.
func (in *Installer) CleanExit() {
	in.mu.Lock()
	defer in.mu.Unlock()
	r := in.load()
	if r.Pending != nil && r.Pending.Phase == phaseApplied && r.Pending.Starts > 0 {
		r.Pending.Starts--
		_ = in.save(r)
	}
}

// copyFile copies a regular file, flushed, with a program's mode.
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &fs.PathError{Op: "copy", Path: src, Err: errors.New("is not a regular file")}
	}
	in, err := os.Open(src) //nolint:gosec // the installed program, or the staged one
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, stagedMode) //nolint:gosec // a path this package built
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// replaceProgram puts a copy of src where dst is, so that anything opening dst finds either the old
// program whole or the new one whole.
func replaceProgram(dst, src string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := renameOver(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
