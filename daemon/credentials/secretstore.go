package credentials

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// Where the refresh token actually goes.
//
// # The gap this exists to close
//
// ADR 0011 says "one keychain entry", and on a desktop that is the whole story: macOS Keychain, Windows
// Credential Manager, and a running GNOME/KDE keyring all work. On a headless Linux server there is no
// Secret Service at all — no session bus, nothing implementing the D-Bus interface go-keyring talks to —
// and every call fails.
//
// That is not an edge case for this project. The CLI's stated audience is exactly the SSH-and-scripting one
// (ADR 0009), so "keyring only" would mean `norite login` cannot work on the machines the CLI exists for.
// Nothing in the docs had noticed: the headless story that *is* written down covers the OAuth browser leg
// (the device-code flow at M9), which is a different problem with a similar name.
//
// # The answer, and what it is not
//
// The keyring when there is one; a file in the daemon's state directory when there is not. The file is
// 0600 inside a directory that is 0700 and per-user, which is the same boundary the daemon already relies
// on for plugin capability grants and pinned .wasm hashes (CLAUDE.md rule 12) — a boundary this codebase
// has already decided to trust for things at least as dangerous as a refresh token.
//
// The token is written in the clear, deliberately. Encrypting it would need a key, the key would have to
// live beside it, and a decryption key stored next to its ciphertext is obfuscation with a security
// vocabulary — worse than plaintext, because it invites the belief that the file is safe to treat
// carelessly. Anyone who can read the file can read the key.
//
// The degradation is never silent. `norite login` says which of the two it used, so a person on a desktop
// who expected their keyring can tell that something is wrong with it rather than discovering months later
// that their token was in a file all along.

// secretStore is the small surface Store needs, so the keyring can be swapped for a file — and, in tests,
// for something that does neither.
type secretStore interface {
	set(service, account, secret string) error
	get(service, account string) (string, error)
	delete(service, account string) error
	// describe names the backing store in one phrase, for a client that has to tell someone where their
	// token went.
	describe() string
	// name is the stable identifier written into the record, so that a later process reads and deletes from
	// the backend actually holding the secret rather than the one it would have picked for itself. Never
	// change these strings: a record naming a backend nothing recognizes falls back to the probe, which is
	// the behavior that made a live token unreachable in the first place.
	name() string
}

// The values Record.SecretBackend may hold.
const (
	backendKeyring = "keyring"
	backendFile    = "file"
)

// backendNamed returns the backend a record names, or nil when it names nothing recognizable — an empty
// field on a record written before this existed, or a value from some later version.
func backendNamed(dir, name string) secretStore {
	switch name {
	case backendKeyring:
		return keyringStore{}
	case backendFile:
		return fileStore{dir: dir}
	default:
		return nil
	}
}

// newSecretStore picks a backend for this machine.
//
// The choice is made once and cached, because it costs a probe: a keyring call that fails on a headless box
// can take a D-Bus timeout, and paying that on every read would make daemon startup mysteriously slow.
func newSecretStore(dir string) secretStore {
	return &autoStore{dir: dir}
}

type autoStore struct {
	dir string

	once   sync.Once
	chosen secretStore
}

func (a *autoStore) backend() secretStore {
	a.once.Do(func() {
		if keyringUsable() {
			a.chosen = keyringStore{}
			return
		}
		a.chosen = fileStore{dir: a.dir}
	})
	return a.chosen
}

func (a *autoStore) set(service, account, secret string) error {
	return a.backend().set(service, account, secret)
}
func (a *autoStore) get(service, account string) (string, error) {
	return a.backend().get(service, account)
}
func (a *autoStore) delete(service, account string) error {
	return a.backend().delete(service, account)
}
func (a *autoStore) describe() string { return a.backend().describe() }
func (a *autoStore) name() string     { return a.backend().name() }

// keyringProbeAccount is the entry the probe writes and removes. Named so that one left behind by a crash
// is obviously not a real credential.
const keyringProbeAccount = "__norite_probe__"

// keyringUsable reports whether this machine has a working keyring.
//
// By writing and deleting, not by reading. A read of a missing entry returns "not found" on a working
// keyring and an error on a broken one, and go-keyring does not distinguish those uniformly across
// platforms — so a read-based probe would answer "usable" for a keyring that cannot actually store
// anything, and the failure would surface at the one moment there is a real token to lose.
func keyringUsable() bool {
	if err := keyring.Set(keyringService, keyringProbeAccount, "probe"); err != nil {
		return false
	}
	// A delete failure is not disqualifying: the write worked, which is the property being tested, and a
	// stray probe entry is harmless.
	_ = keyring.Delete(keyringService, keyringProbeAccount)
	return true
}

// ---------- the OS keyring ----------

type keyringStore struct{}

func (keyringStore) set(service, account, secret string) error {
	if err := keyring.Set(service, account, secret); err != nil {
		return fmt.Errorf("storing the credential in the OS keyring: %w", err)
	}
	return nil
}

func (keyringStore) get(service, account string) (string, error) {
	secret, err := keyring.Get(service, account)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNoCredential
	case err != nil:
		return "", fmt.Errorf("reading the credential from the OS keyring: %w", err)
	}
	return secret, nil
}

func (keyringStore) delete(service, account string) error {
	err := keyring.Delete(service, account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("removing the credential from the OS keyring: %w", err)
	}
	return nil
}

func (keyringStore) describe() string { return "the OS keyring" }
func (keyringStore) name() string     { return backendKeyring }

// ---------- the file fallback ----------

type fileStore struct{ dir string }

// path names the token file for one instance.
//
// A readable prefix plus a hash of the whole URL. The prefix alone was the first version and was wrong:
// substituting every unsafe character for '_' is not injective, so `https://a.example.com:8443` and
// `https://a.example.com/8443` — both valid, both distinct instances — produced one filename, and logging
// in to the second silently overwrote the first's refresh token. The hash is what makes the mapping
// one-to-one; the prefix is what lets a person looking at the directory tell which file is which.
//
// Hex rather than base64url, because a filesystem that is case-insensitive (Windows, and macOS by default)
// would fold `A` and `a` together and put the collision straight back.
func (f fileStore) path(account string) string {
	sum := sha256.Sum256([]byte(account))
	prefix := sanitizeForFilename(account)
	if len(prefix) > maxTokenFilePrefix {
		prefix = prefix[:maxTokenFilePrefix]
	}
	return filepath.Join(f.dir, "token-"+prefix+"-"+hex.EncodeToString(sum[:])[:16])
}

// maxTokenFilePrefix keeps the readable part well inside every filesystem's component limit, leaving room
// for the hash and the "token-" prefix.
const maxTokenFilePrefix = 48

func (f fileStore) set(_, account, secret string) error {
	// Written with the same care as the record, and by the same function: 0600 from creation, flushed,
	// renamed into place.
	if err := writeFileAtomically(f.path(account), []byte(secret)); err != nil {
		return fmt.Errorf("storing the credential in a file: %w", err)
	}
	return nil
}

func (f fileStore) get(_, account string) (string, error) {
	data, err := os.ReadFile(f.path(account))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNoCredential
		}
		return "", fmt.Errorf("reading the credential file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (f fileStore) delete(_, account string) error {
	if err := os.Remove(f.path(account)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the credential file: %w", err)
	}
	return nil
}

func (f fileStore) describe() string {
	return "a file in " + f.dir + " (this machine has no usable OS keyring)"
}

func (fileStore) name() string { return backendFile }

// sanitizeForFilename reduces an instance URL to something safe as one path component.
//
// Everything outside a conservative allow-list becomes '_', which cannot produce a separator, a parent
// reference, or a Windows-reserved character — the input is operator-supplied, and a filename built from
// untrusted text is how a write lands somewhere it was never meant to.
//
// Lossy by design, and never used alone: see path, where a hash of the untouched URL carries the identity
// this discards.
func sanitizeForFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
