package credentials

import "errors"

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
	return openIn(dir, fileStore{dir: dir})
}
