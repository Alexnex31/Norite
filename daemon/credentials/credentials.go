// Package credentials stores the one session a Norite client account has on this machine.
//
// # Why this lives in the daemon module, and is exported
//
// The daemon is the sole holder of its account's tokens (ADR 0011): one entry, one process, and no attach
// client keeps a copy. So the daemon module owns what a stored credential *is*. The CLI imports this
// package rather than defining a second format, because two implementations of one on-disk shape drift the
// first time either changes and the failure mode is a login that appears to work and a daemon that cannot
// find it.
//
// The CLI writes exactly once, at `norite login`, because it is the only process that ever sees the
// password. From Milestone M20 even that goes over the local IPC socket and the CLI stops touching this
// package at all; until then it is the only path there is.
//
// # What is stored, and what is not
//
// The refresh token, and nothing else. An access token lives fifteen minutes and would be expired before
// any daemon restart it might have survived, so persisting one buys a shorter window of nothing (M4). The
// daemon trades the refresh token for a fresh pair at startup.
//
// Beside it, a small non-secret record: which instance, which account, and this installation's device ID.
// That is deliberately *not* in the keyring — a keyring entry is an awkward place for something a person
// may need to read while debugging, and none of it is a secret.
package credentials

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alexnex31/Norite/daemon/internal/paths"
)

// ErrNoCredential reports that this machine has no stored session — nobody has run `norite login`, or a
// logout cleared it.
var ErrNoCredential = errors.New("no stored credential: run `norite login` first")

// keyringService is the service name the OS keyring files this under. One constant, used by both writer and
// reader; a mismatch here is a login that stores a token nothing can find.
const keyringService = "norite"

// recordFileName holds the non-secret half. It sits in the daemon's state directory, which is 0700 and
// per-user by construction (see paths.StateDir).
const recordFileName = "account.json"

// filePerm is owner-only. It applies to the record and, where there is no keyring, to the token beside it.
const filePerm = 0o600

// Record is everything about a session that is not a secret.
type Record struct {
	// InstanceURL is the origin of the Norite instance this session belongs to, without a trailing slash.
	InstanceURL string `json:"instance_url"`
	// UserID and Username identify the account, for display. Neither is authoritative — the instance
	// decides who a token belongs to — and neither is used to make a decision here.
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// DeviceID scopes the refresh-token family (ADR 0011). Generated once per installation and kept for the
	// life of it: a new one on every login would strand the previous family until it expired, and rotating
	// it is what reuse detection reads as a stolen token.
	DeviceID string `json:"device_id"`
	// DeviceName is what the account's session list shows a person. Free text, theirs to recognise.
	DeviceName string `json:"device_name"`
}

// Validate reports whether a record is usable.
func (r Record) Validate() error {
	if r.InstanceURL == "" {
		return errors.New("credential record has no instance URL")
	}
	if _, err := ParseInstanceURL(r.InstanceURL); err != nil {
		return err
	}
	if r.DeviceID == "" {
		return errors.New("credential record has no device ID")
	}
	return nil
}

// ParseInstanceURL normalizes an instance origin and rejects anything that cannot be one.
//
// Strict on purpose: this string decides where a password is sent. A typo that resolves to a scheme this
// does not expect, or that quietly carries a path, is worth failing at the prompt rather than at the first
// request — and worth failing before the password is asked for at all.
func ParseInstanceURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("an instance URL is required")
	}
	// A bare host is what people type. Defaulting it to https rather than http means a slip cannot silently
	// downgrade the connection that carries the password.
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable instance URL", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Permitted, because a self-hosted instance behind a reverse proxy on a private network is a real
		// deployment and refusing it outright would make this unusable there. The CLI says so out loud at
		// the point a password is about to cross it.
	default:
		return "", fmt.Errorf("instance URL scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", raw)
	}
	if u.User != nil {
		// Credentials in the URL would land in the record file, and from there in any bug report that
		// includes it.
		return "", errors.New("an instance URL must not carry a username or password")
	}

	return strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// NewDeviceID returns an identifier for this installation.
//
// Random rather than derived from a hostname or a MAC address: it is sent to the instance and stored on the
// account's session list, so deriving it from anything about the machine would put that detail on a server
// the person may not control, to solve a problem randomness solves for free.
func NewDeviceID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a device ID: %w", err)
	}
	return "dev_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Store writes a session to this machine.
type Store struct {
	// dir is the daemon state directory. Resolved once at construction so every operation agrees on it.
	dir string
	// secrets is the backing store for the refresh token — the OS keyring, or a file beside the record when
	// the machine has no keyring. See newSecretStore.
	secrets secretStore
}

// Open resolves the store for the current user.
func Open() (*Store, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	return openIn(dir, newSecretStore(dir))
}

func openIn(dir string, secrets secretStore) (*Store, error) {
	return &Store{dir: dir, secrets: secrets}, nil
}

// Save records a session, replacing any previous one.
//
// The secret is written first. If the record write then fails there is an unreferenced token in the keyring
// rather than a record pointing at a token that is not there — the first is inert, the second is a daemon
// that starts, finds a credential, and cannot use it.
func (s *Store) Save(record Record, refreshToken string) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if refreshToken == "" {
		return errors.New("refusing to store an empty refresh token")
	}

	if err := s.secrets.set(keyringService, record.InstanceURL, refreshToken); err != nil {
		return err
	}
	return s.writeRecord(record)
}

// Load returns the stored session.
func (s *Store) Load() (Record, string, error) {
	record, err := s.readRecord()
	if err != nil {
		return Record{}, "", err
	}
	if err := record.Validate(); err != nil {
		return Record{}, "", fmt.Errorf("stored credential is unusable: %w", err)
	}

	token, err := s.secrets.get(keyringService, record.InstanceURL)
	if err != nil {
		return Record{}, "", err
	}
	return record, token, nil
}

// LoadRecord returns the non-secret half alone.
//
// Separate from Load so that showing which account is logged in — and, later, deciding whether to prompt —
// never has to touch the keyring. On a locked keyring that read prompts the user, and a status command that
// pops a system dialog is a status command nobody runs twice.
func (s *Store) LoadRecord() (Record, error) {
	return s.readRecord()
}

// Clear removes the stored session. Absent is success: logging out twice is not an error.
func (s *Store) Clear() error {
	record, err := s.readRecord()
	switch {
	case errors.Is(err, ErrNoCredential):
		return nil
	case err != nil:
		return err
	}

	// The secret goes first, for the same reason it is written first: whichever half survives a partial
	// failure, it should be the inert one.
	if err := s.secrets.delete(keyringService, record.InstanceURL); err != nil {
		return err
	}
	if err := os.Remove(s.recordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the credential record: %w", err)
	}
	return nil
}

// SecretLocation describes where the token is actually kept, for a client that has to tell someone.
func (s *Store) SecretLocation() string { return s.secrets.describe() }

func (s *Store) recordPath() string { return filepath.Join(s.dir, recordFileName) }

func (s *Store) readRecord() (Record, error) {
	data, err := os.ReadFile(s.recordPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, ErrNoCredential
		}
		return Record{}, fmt.Errorf("reading the credential record: %w", err)
	}

	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("the credential record at %s is not readable JSON: %w", s.recordPath(), err)
	}
	return record, nil
}

// writeRecord replaces the record atomically.
//
// Temp file plus rename, as every writer of shared client state does (architecture.md §3): a half-written
// record is a daemon that cannot start, and a crash mid-write is exactly when someone is least able to
// diagnose one. The temp file is created 0600 rather than tightened afterwards, so its contents are never
// briefly world-readable.
func (s *Store) writeRecord(record Record) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the credential record: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, recordFileName+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary credential record: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename has succeeded

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restricting the credential record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing the credential record: %w", err)
	}
	// Flushed before the rename, so a crash between the two cannot leave the new name pointing at an empty
	// file — which is the one outcome a rename is supposed to rule out.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flushing the credential record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing the credential record: %w", err)
	}

	if err := os.Rename(tmpName, s.recordPath()); err != nil {
		return fmt.Errorf("replacing the credential record: %w", err)
	}
	return nil
}
