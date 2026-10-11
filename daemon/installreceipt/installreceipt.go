// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package installreceipt is the note an installer leaves saying where it put the two programs (M24).
//
// The daemon installs a release itself only where it finds one naming the directory it is running from.
// An install without one, a package manager's or a checkout's build, is somebody else's to update: the
// daemon checks, says so, and never writes a program there.
//
// Outside internal/ because the CLI writes it, for the installer scripts, and the daemon reads it.
package installreceipt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

// FileName is the receipt's name in the state directory.
const FileName = "install.json"

// maxSize is more than any receipt this writes.
const maxSize = 16 << 10

// Receipt says where an installer put `norite` and `norite-daemon`.
type Receipt struct {
	// BinDir is the directory holding both, absolute.
	BinDir string `json:"bin_dir"`
	// By names what wrote it: "install.sh", "install.ps1".
	By string `json:"installed_by"`
}

// ErrNone is no receipt in the state directory.
var ErrNone = errors.New("no install receipt")

// Read returns the receipt in a state directory, or ErrNone.
func Read(stateDir string) (Receipt, error) {
	path := filepath.Join(stateDir, FileName)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Receipt{}, ErrNone
	}
	if err != nil {
		return Receipt{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return Receipt{}, fmt.Errorf("%s is not an install receipt", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // a fixed name in the user's own state directory
	if err != nil {
		return Receipt{}, err
	}
	var r Receipt
	if err := json.Unmarshal(data, &r); err != nil || !filepath.IsAbs(r.BinDir) {
		return Receipt{}, fmt.Errorf("%s is not an install receipt", path)
	}
	r.BinDir = filepath.Clean(r.BinDir)
	return r, nil
}

// Write records a receipt in a state directory.
func Write(stateDir string, r Receipt) error {
	if !filepath.IsAbs(r.BinDir) {
		return fmt.Errorf("an install receipt names an absolute directory, and %q is not one", r.BinDir)
	}
	r.BinDir = filepath.Clean(r.BinDir)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	err = atomicfile.Write(filepath.Join(stateDir, FileName), append(data, '\n'), atomicfile.Options{Mode: 0o600})
	if errors.Is(err, atomicfile.ErrNotDurable) {
		return nil
	}
	return err
}

// Remove deletes the receipt, for an uninstall. None there is not an error.
func Remove(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Record writes a receipt in the current user's state directory, and Forget removes it: what the CLI does
// for an installer and for an uninstall.
func Record(r Receipt) error {
	dir, err := paths.StateDir()
	if err != nil {
		return err
	}
	return Write(dir, r)
}

func Forget() error {
	dir, err := paths.StateDir()
	if err != nil {
		return err
	}
	return Remove(dir)
}

// ForgetIfNames removes the current user's receipt only when it names binDir. An installer that put
// programs somewhere else, or removed them from somewhere else, has nothing to say about the install the
// receipt is for.
func ForgetIfNames(binDir string) error {
	dir, err := paths.StateDir()
	if err != nil {
		return err
	}
	r, err := Read(dir)
	if err != nil {
		// None, or one that cannot be read: nothing names this directory.
		return nil
	}
	if r.BinDir != filepath.Clean(binDir) {
		return nil
	}
	return Remove(dir)
}
