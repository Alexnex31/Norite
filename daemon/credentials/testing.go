package credentials

import (
	"errors"
	"time"
)

// OpenLocalForTest returns a store that keeps everything in dir, including the secret, and never touches
// the machine's OS keyring.
//
// Exported because it is needed from other modules — the CLI's login tests and the daemon's session tests
// both drive a whole flow against a temporary directory — and named so that production use is obviously
// wrong.
//
// It exists because Open and OpenIn choose their backend by probing the real keyring, which is right for
// those and wrong for a test: on a developer's desktop the probe succeeds and the "temporary" store deposits
// live refresh tokens into the machine-global keyring, under the real service name, permanently, since no
// test removes them. On a headless CI box the same probe pays a D-Bus timeout per store instead. Neither is
// something a test should be doing, and the temp directory the caller passed makes it look as though
// neither is happening.
func OpenLocalForTest(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("a credential store needs a directory")
	}

	store, err := openIn(dir, fileStore{dir: dir})
	if err != nil {
		return nil, err
	}

	// A test that contends on the lock should find out in milliseconds. Production waits the full five
	// seconds because the holder is a real command doing real work; a test's holder is the test itself.
	store.lockWait = 100 * time.Millisecond

	// Both names resolve to the file, which is what makes the promise above true rather than merely likely.
	// openIn wires "keyring" to the real keyring, so a record carrying `"secret_backend": "keyring"` — a
	// literal in a test, or one left in a reused directory — would otherwise send Load, Clear and
	// ReplaceToken to the developer's own keyring under the real service name.
	store.backends = map[string]secretStore{
		backendKeyring: fileStore{dir: dir},
		backendFile:    fileStore{dir: dir},
	}
	return store, nil
}
