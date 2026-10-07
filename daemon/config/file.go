// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/Alexnex31/Norite/daemon/atomicfile"
	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

const (
	// newFileMode is what a config.toml created here gets. A file that already exists keeps its own:
	// it is a person's, and often one they publish.
	newFileMode = 0o600
	dirMode     = 0o700

	lockWait    = 5 * time.Second
	lockPoll    = 25 * time.Millisecond
	maxAttempts = 4
)

// ErrLocked reports that another program held the config's lock for the whole wait.
var ErrLocked = errors.New("another Norite program is writing the config file; try again in a moment")

// ErrKeepsChanging reports a file that was modified under every attempt to write it.
var ErrKeepsChanging = errors.New("the config file kept changing while Norite was writing it, " +
	"so nothing was written; save it in your editor and try again")

// ErrUnknownKey reports a name that is not a key Norite defines. Set refuses it: writing a key nothing
// reads is how a typo becomes a setting that silently does nothing.
var ErrUnknownKey = errors.New("not a key Norite knows")

// ValueError reports a value a key does not accept. It is the caller's mistake rather than a fault in the
// file, which is the difference a command line turns into its exit code.
type ValueError struct {
	// Key is the full key, "tui.colors.accent".
	Key string
	// Problem says what the key accepts instead.
	Problem string
}

func (e *ValueError) Error() string { return e.Key + " " + e.Problem }

var errChanged = errors.New("changed underneath")

// Update rewrites the file at path with what fn returns for its current contents.
//
// Three things can write this file at once: this program, another Norite program, and a person in an
// editor. The first two take a lock. It is never on the config itself, whose inode a rename replaces, and
// it is not beside the config either: that directory roams on Windows and is often a link into a dotfiles
// repository, and a lock file belongs in neither. It lives in the state directory, named for the config's
// path. The editor takes no lock at all, so immediately before the rename the
// file is read again: if it is not what fn was given, somebody saved in between, and fn runs again on
// their version rather than overwriting it. Of the two edits, the person's is the one that must survive.
//
// fn may run more than once and must not keep state between runs. A file that does not exist is given to
// fn as no bytes, and is created if fn returns any.
func Update(path string, fn func(current []byte) ([]byte, error)) error {
	lockPath, err := lockFor(path)
	if err != nil {
		return err
	}
	lock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, lockPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("locking the config file: %w", err)
	}
	if !locked {
		return ErrLocked
	}
	// The lock file stays. Removing it would let a second program lock a new file of the same name while
	// a third still holds the old one.
	defer func() { _ = lock.Unlock() }()

	for range maxAttempts {
		current, err := snapshot(path)
		if err != nil {
			return err
		}
		next, err := fn(current)
		if err != nil {
			return err
		}
		if bytes.Equal(next, current) {
			return nil
		}
		// Bounded on the way out as on the way in. Otherwise one write can produce a file that every
		// later read refuses, this package's own Unset included, and the key that did it can no longer
		// be removed through Norite at all.
		if len(next) > MaxFileSize {
			return fmt.Errorf("%s: the change would make it: %w", path, ErrTooLarge)
		}
		// Only now, with something to write: an Unset on a machine with no config creates nothing.
		if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
			return fmt.Errorf("creating the config directory: %w", err)
		}
		err = atomicfile.Write(path, next, atomicfile.Options{
			Mode:          newFileMode,
			KeepMode:      true,
			FollowSymlink: true,
			Before: func() error {
				now, err := snapshot(path)
				if err != nil {
					return err
				}
				if !bytes.Equal(now, current) {
					return errChanged
				}
				return nil
			},
		})
		if errors.Is(err, errChanged) {
			continue
		}
		// The file holds the change. A setting is not worth failing a command over a directory flush.
		if errors.Is(err, atomicfile.ErrNotDurable) {
			return nil
		}
		return err
	}
	return ErrKeepsChanging
}

// lockFor returns the lock file for the config at path: in the state directory, named by a digest of
// the path as written, so every program that names the same config takes the same lock.
func lockFor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("locating %s: %w", path, err)
	}
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(dir, "config-"+hex.EncodeToString(sum[:8])+".lock"), nil
}

// ReadFile returns the config at path, or no bytes when there is none. It is bounded like every read here.
func ReadFile(path string) ([]byte, error) { return snapshot(path) }

// snapshot reads the file, with a missing one read as empty.
func snapshot(path string) ([]byte, error) {
	data, err := read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	return data, nil
}

// Set writes what somebody typed for a key into the file at path.
//
// name is the key as the contract lists it, "colors.accent", and section is where to write it. A shared
// key may be written under a client's section, which is how one client is given its own value.
func Set(path string, section Section, name, input string) error {
	segments, literal, err := resolve(section, name, input, true)
	if err != nil {
		return err
	}
	return named(path, Update(path, func(current []byte) ([]byte, error) {
		return setRaw(current, segments, literal)
	}))
}

// named puts the file's name on an error about the file's contents, which otherwise carries a line number
// and nothing to say which file it is a line of.
func named(path string, err error) error {
	var pe *ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return err
}

// Unset removes a key from the file at path, so its default or its [shared] value applies again.
func Unset(path string, section Section, name string) error {
	segments, _, err := resolve(section, name, "", false)
	if err != nil {
		return err
	}
	return named(path, Update(path, func(current []byte) ([]byte, error) {
		return unsetRaw(current, segments)
	}))
}

// resolve turns a section and a key name into the path the editor addresses, and the literal to write.
func resolve(section Section, name, input string, withValue bool) (segments []string, literal string, err error) {
	// The section first. lookup accepts a shared key under any section it is asked about, so without
	// this a typo of the section writes a table nothing reads: the silent setting ErrUnknownKey exists
	// to prevent.
	if !validSection(string(section)) {
		return nil, "", fmt.Errorf("[%s]: %w; the sections are [shared], [tui] and [gui]", section, ErrUnknownKey)
	}
	key, ok := lookup(section, name)
	if !ok {
		return nil, "", fmt.Errorf("%s.%s: %w", section, name, ErrUnknownKey)
	}
	segments = append([]string{string(section)}, strings.Split(key.Name, ".")...)
	if key.Kind == KindTable {
		// Everything after the table's name is one key, dots and all: a chord may contain one.
		inner := strings.TrimPrefix(name, key.Name)
		inner = strings.TrimPrefix(inner, ".")
		if inner == "" {
			return nil, "", &ValueError{Key: string(section) + "." + name,
				Problem: "is a table; name a key inside it, as in " + key.Name + ".C-x b"}
		}
		segments = append(segments, inner)
		if withValue {
			literal = BasicString(input)
		}
		return segments, literal, nil
	}
	if withValue {
		literal, err = Literal(key, input)
		if err != nil {
			return nil, "", &ValueError{Key: string(section) + "." + name, Problem: err.Error()}
		}
	}
	return segments, literal, nil
}

// errMoved reports that the file a client reads changed while a write to it was waiting: the toggle was
// flipped.
var errMoved = errors.New("the config toggle changed")

// UpdateFor is Update on the file client reads right now, which the same-machine toggle decides, and
// returns which file that was. fn is also told whether that is the client's own file.
//
// Which file is asked again once the file's lock is held. The daemon holds that lock while it flips the
// toggle, so a write that was waiting behind a flip finds it has the wrong file and starts again on the
// right one, where otherwise it would land in a file just set aside, or bring back one just moved away,
// and report success (M21 /code-review).
func UpdateFor(client Section, fn func(path string, split bool, current []byte) ([]byte, error)) (string, error) {
	var path string
	for range maxAttempts {
		var split bool
		var err error
		path, split, err = PathFor(client)
		if err != nil {
			return "", err
		}
		err = Update(path, func(current []byte) ([]byte, error) {
			now, _, err := PathFor(client)
			if err != nil {
				return nil, err
			}
			if now != path {
				return nil, errMoved
			}
			return fn(path, split, current)
		})
		if errors.Is(err, errMoved) {
			continue
		}
		return path, err
	}
	return path, ErrKeepsChanging
}

// SetFor is Set on the file client reads right now.
func SetFor(client, section Section, name, input string) (string, error) {
	segments, literal, err := resolve(section, name, input, true)
	if err != nil {
		return "", err
	}
	path, err := UpdateFor(client, func(_ string, _ bool, current []byte) ([]byte, error) {
		return setRaw(current, segments, literal)
	})
	return path, named(path, err)
}

// UnsetFor is Unset on the file client reads right now.
func UnsetFor(client, section Section, name string) (string, error) {
	segments, _, err := resolve(section, name, "", false)
	if err != nil {
		return "", err
	}
	path, err := UpdateFor(client, func(_ string, _ bool, current []byte) ([]byte, error) {
		return unsetRaw(current, segments)
	})
	return path, named(path, err)
}

// Lock takes the lock a write to the config at path takes, and returns what releases it. It is for the
// daemon's toggle, which must keep `norite config set` out of a file from the moment it reads it until the
// toggle says who reads it next. It waits as a write does and reports ErrLocked the same way.
func Lock(path string) (unlock func(), err error) {
	lockPath, err := lockFor(path)
	if err != nil {
		return nil, err
	}
	lock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, lockPoll)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("locking the config file: %w", err)
	}
	if !locked {
		return nil, ErrLocked
	}
	return func() { _ = lock.Unlock() }, nil
}
