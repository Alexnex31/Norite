// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package toggle performs the same-machine config toggle (M21): `norite config split` and `unsplit`, as
// requests on the attach socket.
//
// Off, which is the default, the terminal client and the GUI read one config.toml. On, each reads its
// own, config.tui.toml and config.gui.toml, so that somebody who uses both on one machine can let them
// diverge. Which it is lives in state.json, and the daemon is that file's only writer, which is why this
// is the daemon's to do and not the command's.
//
// # What it promises
//
// Turning it on and off again loses nothing. Split copies config.toml to both files, byte for byte, and
// leaves config.toml where it is. Unsplit folds the two back onto config.toml (config.MergeClients): each
// client's section from its own file, and [shared] key by key. It sets aside rather than deletes
// everything it replaced: both split files, and config.toml as it was, each with ".before-unsplit" after
// its name, and a number after that when an earlier unsplit's copy is already there.
//
// # Other writers
//
// `norite config set` takes a lock on the file it writes, and the toggle holds that lock on every file it
// reads from the read until the state says who reads what; the command asks which file is its own again
// once it has the lock (config.UpdateFor), so it never lands in a file just set aside. An editor takes no
// lock, so each file is read again after the work is done and the work redone if somebody saved.
//
// # Order, and what a crash leaves
//
// The files are written before the state, in both directions, and the state is written before anything is
// moved away. A daemon killed partway leaves the toggle where it was with every file a client reads still
// in place; the request can simply be made again, and a split finding its files already there as copies
// of config.toml carries on rather than refusing.
package toggle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/config"
	internalstate "github.com/Alexnex31/Norite/daemon/internal/statefile"
	"github.com/Alexnex31/Norite/daemon/ipc"
	"github.com/Alexnex31/Norite/daemon/statefile"
	"github.com/Alexnex31/Norite/daemon/termsafe"
)

// newFileMode is what a config file this package creates gets, as daemon/config creates one.
const newFileMode = 0o600

// Handler answers the toggle's requests.
type Handler struct {
	// ConfigDir is the directory config.toml is in.
	ConfigDir string
	// StateDir is the daemon's state directory, where state.json is.
	StateDir string
	// Changed is called after the toggle has moved, so that attached clients read their config again: the
	// file each one reads has just become another file.
	Changed func()
	Log     zerolog.Logger

	// rename is os.Rename, and something a test can stand inside. saved, when set, runs after a pass over
	// the files and before they are read again, where a test saves one as an editor would.
	rename func(from, to string) error
	saved  func()
}

// attempts bounds how often the work is redone because somebody saved a file under it.
const attempts = 4

// maxBackups bounds how many set-aside copies of one file are kept before an unsplit asks for some to be
// moved away, rather than overwrite one.
const maxBackups = 100

// refusal is a request understood and not carried out. Its text is for the person who asked.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Do answers one local request.
func (h *Handler) Do(ctx context.Context, req ipc.Request) ipc.Response {
	var run func(context.Context) (any, error)
	method := http.MethodPost
	switch req.Path {
	case ipc.PathConfig:
		run, method = h.where, http.MethodGet
	case ipc.PathConfigSplit:
		run = func(ctx context.Context) (any, error) { return h.split(ctx) }
	case ipc.PathConfigUnsplit:
		run = func(ctx context.Context) (any, error) { return h.unsplit(ctx) }
	default:
		return ipc.Failure(ipc.RelayBadRequest, "the daemon answers no request of its own at that path")
	}
	if req.Method != method {
		return ipc.Failure(ipc.RelayBadRequest, "the config toggle is read with GET and changed with POST")
	}

	done, err := run(ctx)
	var no *refusal
	var parse *config.ParseError
	switch {
	case errors.As(err, &no):
		return ipc.Failure(ipc.RelayConflict, no.msg)
	case errors.Is(err, internalstate.ErrLocked):
		return ipc.Failure(ipc.RelayConflict, "another request is changing the config toggle; try again")
	case errors.Is(err, config.ErrLocked):
		return ipc.Failure(ipc.RelayConflict, "something else is writing a config file; try again")
	case errors.Is(err, config.ErrKeepsChanging):
		return ipc.Failure(ipc.RelayConflict, "a config file kept being saved while the toggle worked on it; "+
			"try again once it is left alone. Nothing a client reads was moved")
	case errors.Is(err, atomicfile.ErrReadOnly), errors.Is(err, config.ErrInlineTable),
		errors.Is(err, config.ErrNotAFile), errors.Is(err, config.ErrTooLarge), errors.As(err, &parse):
		// Understood, and not done because of a file as it stands: the person can fix that, and the
		// daemon did not fail. The error names paths, which are the filesystem's text.
		return ipc.Failure(ipc.RelayConflict, termsafe.Text(err.Error())+"; the toggle was not changed")
	case err != nil:
		h.Log.Error().Str("error", termsafe.Text(err.Error())).Str("path", req.Path).Msg("the config toggle failed")
		return ipc.Failure(ipc.RelayFailed, termsafe.Text(err.Error()))
	}
	body, err := json.Marshal(done)
	if err != nil {
		return ipc.Failure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	if toggled, moved := done.(ipc.ConfigToggle); moved {
		h.Log.Info().Bool("split", toggled.Split).Msg("the config toggle changed")
		if h.Changed != nil {
			h.Changed()
		}
	}
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}

// where answers which directory this daemon keeps configs in and how the toggle stands. A command asks
// before it asks for a toggle: a daemon started with another XDG_CONFIG_HOME than the shell's would split
// a directory no client reads, and every client would then find its own file missing.
func (h *Handler) where(context.Context) (any, error) {
	state, err := statefile.ReadIn(h.StateDir)
	if err != nil {
		return nil, err
	}
	return ipc.ConfigLocation{Dir: h.ConfigDir, Split: state.ConfigSplit}, nil
}

func (h *Handler) split(ctx context.Context) (ipc.ConfigToggle, error) {
	files := config.FilesIn(h.ConfigDir)
	out := ipc.ConfigToggle{Split: true, Files: []string{files.TUI, files.GUI},
		Merged: []string{}, Kept: []string{}, Skipped: []string{}, Backups: []string{}}
	var unlock func()
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()

	err := internalstate.Update(ctx, h.StateDir, func(s *statefile.State) error {
		if s.ConfigSplit {
			return refuse("the config is already split: the terminal client reads %s and the GUI %s",
				termsafe.Text(files.TUI), termsafe.Text(files.GUI))
		}
		// Held until the state is written: a `norite config set` that was about to write config.toml waits,
		// and then finds its file is now another.
		var err error
		if unlock, err = config.Lock(files.Shared); err != nil {
			return err
		}
		ours := map[string]bool{}
		for range attempts {
			// As it is, whatever it is: a copy is a copy, and a config that does not parse today is still
			// somebody's file with their comments in it. Read through a link, so a dotfile's contents are
			// what is copied; the copies themselves are plain files beside it.
			shared, err := config.ReadFile(files.Shared)
			if err != nil {
				return err
			}
			mode := fs.FileMode(newFileMode)
			if info, err := os.Stat(files.Shared); err == nil {
				mode = info.Mode().Perm() | 0o200
			}
			for _, path := range []string{files.TUI, files.GUI} {
				// Something already there that is not this copy is somebody's file, made by hand or left by
				// a split that never finished against a config that has since changed. It is not
				// overwritten. What this request wrote a moment ago is its own to write again.
				if _, err := os.Lstat(path); err == nil && !ours[path] {
					// Read as a config is read: bounded, and only if it is a file. Anything else there, a
					// directory or a pipe or a file too large to be a config, is not this copy either.
					have, err := config.ReadFile(path)
					if err == nil && string(have) == string(shared) {
						continue
					}
					return refuse("%s already exists and is not a copy of config.toml; move it away, or "+
						"delete it, and split again", termsafe.Text(path))
				} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if err := os.MkdirAll(h.ConfigDir, 0o700); err != nil {
					return err
				}
				if err := write(path, shared, mode); err != nil {
					return err
				}
				ours[path] = true
			}
			if h.saved != nil {
				h.saved()
			}
			// An editor takes no lock. If config.toml was saved while it was being copied, the copies are
			// of what it used to say.
			again, err := config.ReadFile(files.Shared)
			if err != nil {
				return err
			}
			if string(again) == string(shared) {
				s.ConfigSplit = true
				return nil
			}
		}
		return config.ErrKeepsChanging
	}, nil)
	return out, err
}

func (h *Handler) unsplit(ctx context.Context) (ipc.ConfigToggle, error) {
	files := config.FilesIn(h.ConfigDir)
	out := ipc.ConfigToggle{Split: false, Files: []string{files.Shared},
		Merged: []string{}, Kept: []string{}, Skipped: []string{}, Backups: []string{}}
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	// Where each file goes once nobody reads it, chosen before anything is changed.
	aside := map[string]string{}
	// config.toml as this request found it, what the request last wrote to it, and whether it has set the
	// person's contents aside: a second pass must not mistake the first pass's work for theirs.
	var original, written []byte
	seen, keptShared := false, false

	err := internalstate.Update(ctx, h.StateDir, func(s *statefile.State) error {
		if !s.ConfigSplit {
			return refuse("the config is not split: both clients read %s", termsafe.Text(files.Shared))
		}
		// Both clients' files, held until they have been moved aside: a `norite config set` waiting on
		// one finds, when it gets the lock, that its file is config.toml again.
		for _, path := range []string{files.TUI, files.GUI} {
			unlock, err := config.Lock(path)
			if err != nil {
				return err
			}
			unlocks = append(unlocks, unlock)
		}
		for _, path := range []string{files.Shared, files.TUI, files.GUI} {
			name, err := freeBackup(path)
			if err != nil {
				return err
			}
			aside[path] = name
		}

		for range attempts {
			tui, tuiAt, tuiThere, err := readWithTime(files.TUI)
			if err != nil {
				return err
			}
			gui, guiAt, guiThere, err := readWithTime(files.GUI)
			if err != nil {
				return err
			}
			if !tuiThere && !guiThere {
				// Neither client's file is there: removed by hand, or never written. There is nothing to
				// fold back, and folding nothing onto config.toml would empty it. It is left as it is.
				s.ConfigSplit = false
				return nil
			}
			// The more recently written file is the base, and wins where both set a shared key. The
			// terminal client's on a tie, which is also what two untouched copies are.
			// A file that is not there has no time at all, which is before any time a file can carry.
			newer, baseName := config.TUI, files.TUI
			if guiAt.After(tuiAt) {
				newer, baseName = config.GUI, files.GUI
			}
			// Both must parse before anything is touched. A merge is by key, and a file that is not TOML has
			// no keys to carry over: going on would drop everything in it while reporting success.
			merged, plan, err := config.MergeClients(tui, gui, newer)
			var unread *config.ClientFileError
			if errors.As(err, &unread) {
				path := files.TUI
				if unread.Client == config.GUI {
					path = files.GUI
				}
				return refuse("%s is not valid TOML (%s); fix it and unsplit again. Nothing was changed",
					termsafe.Text(path), termsafe.Text(unread.Error()))
			}
			if err != nil {
				return err
			}
			if len(merged) > config.MaxFileSize {
				return refuse("the two files together are larger than a config may be (%d bytes); remove "+
					"something from one and unsplit again. Nothing was changed", config.MaxFileSize)
			}
			out.Base = baseName
			out.Merged, out.Kept, out.Skipped = []string{}, []string{}, []string{}
			for _, c := range plan.Apply {
				out.Merged = append(out.Merged, c.Key())
			}
			for _, c := range plan.Kept {
				out.Kept = append(out.Kept, c.Key())
			}
			for _, w := range plan.Skipped {
				out.Skipped = append(out.Skipped, w.String())
			}
			if plan.MoreSkipped > 0 {
				out.Skipped = append(out.Skipped, fmt.Sprintf("and %d more", plan.MoreSkipped))
			}

			// Through config's own writer: under the lock `norite config set` takes, following a link,
			// keeping the file's mode. config.toml has been read by nobody since the split and may have
			// been edited all the same, so what it holds at the moment it is replaced is kept, and that is
			// asked inside the write: a save landing while this waited is the one kept, not lost.
			keptNow := false
			err = config.Update(files.Shared, func(current []byte) ([]byte, error) {
				// What is the person's: the file as first found, or, if it no longer says what the last pass
				// wrote, whatever they have saved over that since.
				keep := original
				switch {
				case !seen:
					original, keep, seen = current, current, true
				case string(current) != string(written):
					keep = current
				}
				if len(keep) > 0 && string(keep) != string(merged) {
					if err := write(aside[files.Shared], keep, newFileMode); err != nil {
						return nil, err
					}
					keptNow = true
				}
				return merged, nil
			})
			if err != nil {
				// The copy is made inside the write, so that it is of what the file held at that moment, and
				// the write then did not happen: a read-only config.toml, a lock held too long. A copy of a
				// file that was not replaced is clutter that says otherwise, and each refused attempt left
				// another, using up the names (M21 /code-review). Unless an earlier pass did replace it.
				if keptNow && !keptShared {
					_ = os.Remove(aside[files.Shared])
				}
				return err
			}
			keptShared = keptShared || keptNow
			written = merged
			if keptShared {
				out.Backups = []string{aside[files.Shared]}
			}
			if h.saved != nil {
				h.saved()
			}
			// An editor takes no lock. If either file was saved while it was being merged, config.toml
			// holds what it used to say.
			tuiNow, _, _, err := readWithTime(files.TUI)
			if err != nil {
				return err
			}
			guiNow, _, _, err := readWithTime(files.GUI)
			if err != nil {
				return err
			}
			if string(tuiNow) == string(tui) && string(guiNow) == string(gui) {
				s.ConfigSplit = false
				return nil
			}
		}
		return config.ErrKeepsChanging
	}, func() {
		// Only now, with the state saying nobody reads them, and still under the state's lock: moved aside,
		// not deleted. Outside the lock, a split asked for at this moment could find the two files still in
		// place as copies of config.toml, take them as its own, and have them moved out from under it,
		// leaving the toggle on with no file behind either client (M21 /security-sweep).
		//
		// A failure here leaves a split file in place, which the next split refuses by name unless it is
		// still a copy, so it is said rather than swallowed.
		rename := h.rename
		if rename == nil {
			rename = os.Rename
		}
		for _, path := range []string{files.TUI, files.GUI} {
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			err := rename(path, aside[path])
			switch {
			case err == nil:
				out.Backups = append(out.Backups, aside[path])
			case errors.Is(err, fs.ErrNotExist):
			default:
				h.Log.Warn().Str("error", termsafe.Text(err.Error())).Msg("could not set a split config file aside")
				out.Skipped = append(out.Skipped, termsafe.Text(filepath.Base(path))+" could not be moved aside, and is still there")
			}
		}
	})
	return out, err
}

// freeBackup returns a name beside path that nothing has: path with the backup suffix, then with a number
// after it. A copy an earlier unsplit set aside is the only copy of what that unsplit replaced, and the
// answer says nothing was deleted, so it is never the name chosen (M21 /code-review).
func freeBackup(path string) (string, error) {
	base := path + config.BackupSuffix
	for n := 1; n <= maxBackups; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s.%d", base, n)
		}
		_, err := os.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", refuse("there are already %d copies of %s set aside by earlier unsplits; move some away and "+
		"unsplit again. Nothing was changed", maxBackups, termsafe.Text(filepath.Base(path)))
}

// readWithTime reads a config file, when it was last written, and whether it is there at all. A missing
// file is empty. Whether it is there is said outright and not read off the time: a file restored from an
// archive can carry the epoch, and two such files were taken for two missing ones.
func readWithTime(path string) (data []byte, at time.Time, there bool, err error) {
	data, err = config.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return data, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return data, info.ModTime(), true, nil
}

func write(path string, data []byte, mode fs.FileMode) error {
	err := atomicfile.Write(path, data, atomicfile.Options{Mode: mode})
	if errors.Is(err, atomicfile.ErrNotDurable) {
		return nil
	}
	return err
}
