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
// leaves config.toml where it is. Unsplit folds the two back onto config.toml key by key (config.Merge),
// and sets aside rather than deletes everything it replaced: both split files, and config.toml as it was,
// each with ".before-unsplit" after its name.
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

// Handler answers the toggle's two requests.
type Handler struct {
	// ConfigDir is the directory config.toml is in.
	ConfigDir string
	// StateDir is the daemon's state directory, where state.json is.
	StateDir string
	// Changed is called after the toggle has moved, so that attached clients read their config again: the
	// file each one reads has just become another file.
	Changed func()
	Log     zerolog.Logger

	// rename is os.Rename, and something a test can stand inside.
	rename func(from, to string) error
}

// refusal is a request understood and not carried out. Its text is for the person who asked.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Do answers one local request.
func (h *Handler) Do(ctx context.Context, req ipc.Request) ipc.Response {
	var run func(context.Context) (ipc.ConfigToggle, error)
	switch req.Path {
	case ipc.PathConfigSplit:
		run = h.split
	case ipc.PathConfigUnsplit:
		run = h.unsplit
	default:
		return failure(ipc.RelayBadRequest, "the daemon answers no request of its own at that path")
	}
	if req.Method != http.MethodPost {
		return failure(ipc.RelayBadRequest, "the config toggle is changed with POST")
	}

	done, err := run(ctx)
	var no *refusal
	switch {
	case errors.As(err, &no):
		return failure(ipc.RelayConflict, no.msg)
	case errors.Is(err, internalstate.ErrLocked):
		return failure(ipc.RelayConflict, "another request is changing the config toggle; try again")
	case err != nil:
		// The error names paths, which are the environment's and the filesystem's text.
		h.Log.Error().Str("error", termsafe.Text(err.Error())).Str("path", req.Path).Msg("the config toggle failed")
		return failure(ipc.RelayFailed, termsafe.Text(err.Error()))
	}
	body, err := json.Marshal(done)
	if err != nil {
		return failure(ipc.RelayFailed, "the daemon could not encode its answer")
	}
	h.Log.Info().Bool("split", done.Split).Msg("the config toggle changed")
	if h.Changed != nil {
		h.Changed()
	}
	status := http.StatusOK
	return ipc.Response{Status: &status, Body: body}
}

func failure(code, msg string) ipc.Response {
	return ipc.Response{Error: &ipc.RelayError{Code: code, Message: msg}}
}

func (h *Handler) split(ctx context.Context) (ipc.ConfigToggle, error) {
	files := config.FilesIn(h.ConfigDir)
	out := ipc.ConfigToggle{Split: true, Files: []string{files.TUI, files.GUI},
		Merged: []string{}, Kept: []string{}, Skipped: []string{}, Backups: []string{}}

	err := internalstate.Update(ctx, h.StateDir, func(s *statefile.State) error {
		if s.ConfigSplit {
			return refuse("the config is already split: the terminal client reads %s and the GUI %s",
				termsafe.Text(files.TUI), termsafe.Text(files.GUI))
		}
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
			// Something already there that is not this copy is somebody's file, made by hand or left by a
			// split that never finished against a config that has since changed. It is not overwritten.
			_, err := os.Lstat(path)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return err
			default:
				// Read as a config is read: bounded, and only if it is a file. Anything else there, a
				// directory or a pipe or a file too large to be a config, is not this copy either.
				have, err := config.ReadFile(path)
				if err == nil && string(have) == string(shared) {
					continue
				}
				return refuse("%s already exists and is not a copy of config.toml; move it away, or delete "+
					"it, and split again", termsafe.Text(path))
			}
			if err := os.MkdirAll(h.ConfigDir, 0o700); err != nil {
				return err
			}
			if err := write(path, shared, mode); err != nil {
				return err
			}
		}
		s.ConfigSplit = true
		return nil
	}, nil)
	return out, err
}

func (h *Handler) unsplit(ctx context.Context) (ipc.ConfigToggle, error) {
	files := config.FilesIn(h.ConfigDir)
	out := ipc.ConfigToggle{Split: false, Files: []string{files.Shared},
		Merged: []string{}, Kept: []string{}, Skipped: []string{}, Backups: []string{}}
	err := internalstate.Update(ctx, h.StateDir, func(s *statefile.State) error {
		if !s.ConfigSplit {
			return refuse("the config is not split: both clients read %s", termsafe.Text(files.Shared))
		}
		tui, tuiAt, err := readWithTime(files.TUI)
		if err != nil {
			return err
		}
		gui, guiAt, err := readWithTime(files.GUI)
		if err != nil {
			return err
		}
		if tuiAt == 0 && guiAt == 0 {
			// Neither client's file is there: removed by hand, or never written. There is nothing to fold
			// back, and folding nothing onto config.toml would empty it. It is left exactly as it is.
			s.ConfigSplit = false
			return nil
		}
		// Both must parse before either is touched. A merge is by key, and a file that is not TOML has no
		// keys to carry over: going on would drop everything in it while reporting success.
		for path, data := range map[string][]byte{files.TUI: tui, files.GUI: gui} {
			if _, err := config.Inspect(data); err != nil {
				return refuse("%s is not valid TOML (%s); fix it and unsplit again. Nothing was changed",
					termsafe.Text(path), termsafe.Text(err.Error()))
			}
		}

		// The more recently written file is the base, and wins where both set a key. The terminal client's
		// on a tie, which is also what two untouched copies are.
		base, other, baseName := tui, gui, files.TUI
		if guiAt.After(tuiAt) {
			base, other, baseName = gui, tui, files.GUI
		}
		merged, plan, err := config.Merge(base, other)
		if err != nil {
			return err
		}
		if len(merged) > config.MaxFileSize {
			return refuse("the two files together are larger than a config may be (%d bytes); remove "+
				"something from one and unsplit again. Nothing was changed", config.MaxFileSize)
		}
		out.Base = baseName
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

		// config.toml has been read by nobody since the split, and may have been edited all the same. What
		// it holds is kept before it is replaced.
		was, err := config.ReadFile(files.Shared)
		if err != nil {
			return err
		}
		if len(was) > 0 && string(was) != string(merged) {
			backup := files.Shared + config.BackupSuffix
			if err := write(backup, was, newFileMode); err != nil {
				return err
			}
			out.Backups = append(out.Backups, backup)
		}
		// Through config's own writer: under the lock `norite config set` takes, following a link, keeping
		// the file's mode.
		if err := config.Update(files.Shared, func([]byte) ([]byte, error) { return merged, nil }); err != nil {
			return err
		}
		s.ConfigSplit = false
		return nil
	}, func() {
		// Only now, with the state saying nobody reads them, and still under the state's lock: moved aside,
		// not deleted. Outside the lock, a split asked for at this moment could find the two files still in
		// place as copies of config.toml, take them as its own, and have them moved out from under it,
		// leaving the toggle on with no file behind either client (M21 /security-sweep).
		//
		// A failure here leaves a split file in place, which the next split refuses by name unless it is
		// still a copy, so it is said rather than swallowed.
		for _, path := range []string{files.TUI, files.GUI} {
			backup := path + config.BackupSuffix
			rename := h.rename
			if rename == nil {
				rename = os.Rename
			}
			err := rename(path, backup)
			switch {
			case err == nil:
				out.Backups = append(out.Backups, backup)
			case errors.Is(err, fs.ErrNotExist):
			default:
				h.Log.Warn().Str("error", termsafe.Text(err.Error())).Msg("could not set a split config file aside")
				out.Skipped = append(out.Skipped, termsafe.Text(filepath.Base(path))+" could not be moved aside, and is still there")
			}
		}
	})
	return out, err
}

// readWithTime reads a config file and when it was last written. A missing file is empty and older than
// anything.
func readWithTime(path string) ([]byte, int64Time, error) {
	data, err := config.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return data, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return data, int64Time(info.ModTime().UnixNano()), nil
}

// int64Time is a modification time in nanoseconds, zero for a file that is not there.
type int64Time int64

func (t int64Time) After(u int64Time) bool { return t > u }

func write(path string, data []byte, mode fs.FileMode) error {
	err := atomicfile.Write(path, data, atomicfile.Options{Mode: mode})
	if errors.Is(err, atomicfile.ErrNotDurable) {
		return nil
	}
	return err
}
