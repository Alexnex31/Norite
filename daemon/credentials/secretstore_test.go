package credentials

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// The file fallback is what makes `norite login` work on a headless Linux server, where there is no Secret
// Service for go-keyring to talk to. That is not an edge case here — the CLI's audience is exactly the
// SSH-and-scripting one (ADR 0009) — so these are as load-bearing as the keyring path.

func TestTheFileFallbackRoundTrips(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, fileStore{dir: dir})
	require.NoError(t, err)

	require.NoError(t, store.Save(sampleRecord(), "nrt_headless"))

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "ada", record.Username)
	assert.Equal(t, "nrt_headless", token)
}

// The state directory is 0700 and per-user, and the token inside it is 0600. That pair is the whole
// security boundary for this path — the same one the daemon already relies on for plugin capability grants
// and pinned .wasm hashes (rule 12) — so it is worth asserting rather than assuming.
func TestTheFallbackTokenFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes do not describe Windows ACLs")
	}
	dir := t.TempDir()
	fs := fileStore{dir: dir}
	require.NoError(t, fs.set(keyringService, "https://chat.example.com", "nrt_headless"))

	info, err := os.Stat(fs.path("https://chat.example.com"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestTheFallbackReportsAnAbsentTokenAsNoCredential(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	_, err := fs.get(keyringService, "https://chat.example.com")
	assert.ErrorIs(t, err, ErrNoCredential)
	assert.NoError(t, fs.delete(keyringService, "https://chat.example.com"),
		"deleting a token that is not there is not a failure")
}

// A person on a desktop who expected their keyring needs to be able to tell that it was not used. Silent
// degradation is how someone finds out months later that their token was in a file all along.
func TestTheFallbackSaysWhereTheTokenWent(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, fileStore{dir: dir})
	require.NoError(t, err)

	where := store.SecretLocation()
	assert.Contains(t, where, dir)
	assert.Contains(t, where, "no usable OS keyring")
}

// The instance URL reaches a filename, and it is operator-supplied. Anything that could climb out of the
// state directory or name a reserved device has to be gone before it gets there.
func TestTheTokenFilenameCannotEscapeTheStateDirectory(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	for _, hostile := range []string{
		"https://../../etc/passwd",
		"https://example.com/../../../root/.ssh/authorized_keys",
		"..",
		"../..",
		`https://example.com\..\..\windows`,
	} {
		path := fs.path(hostile)
		base := filepath.Base(path)

		// The property that matters is that the result is one path component inside the state directory.
		// A literal ".." *within* a longer name — "token-.." — is an ordinary filename and traverses
		// nothing; only a separator, or a name that is exactly "." or "..", could climb out.
		assert.Equal(t, dir, filepath.Dir(path),
			"%q must stay inside the state directory, got %q", hostile, path)
		assert.NotContains(t, base, "/")
		assert.NotContains(t, base, `\`)
		assert.NotEqual(t, ".", base)
		assert.NotEqual(t, "..", base)
		assert.Equal(t, filepath.Join(dir, base), filepath.Clean(path),
			"%q must not resolve anywhere else", hostile)
	}
}

// Two instances must not collide onto one file, or logging into the second would silently overwrite the
// first machine-wide.
func TestEachInstanceGetsItsOwnTokenFile(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	require.NoError(t, fs.set(keyringService, "https://one.example.com", "nrt_one"))
	require.NoError(t, fs.set(keyringService, "https://two.example.com", "nrt_two"))

	one, err := fs.get(keyringService, "https://one.example.com")
	require.NoError(t, err)
	two, err := fs.get(keyringService, "https://two.example.com")
	require.NoError(t, err)
	assert.Equal(t, "nrt_one", one)
	assert.Equal(t, "nrt_two", two)
}

// ---------- the keyring path ----------

// go-keyring's own mock, so the keyring branch is exercised on a CI machine that has no keyring at all.
// It is the same code path a desktop takes; only the backend behind it differs.
func TestTheKeyringPathRoundTrips(t *testing.T) {
	keyring.MockInit()

	dir := t.TempDir()
	store, err := openIn(dir, keyringStore{})
	require.NoError(t, err)

	require.NoError(t, store.Save(sampleRecord(), "nrt_desktop"))

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "ada", record.Username)
	assert.Equal(t, "nrt_desktop", token)

	require.NoError(t, store.Clear())
	_, _, err = store.Load()
	assert.ErrorIs(t, err, ErrNoCredential)

	// ...and no token file was written beside it, which is what "one entry" means.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "token-"),
			"the keyring path must not also leave a file")
	}
}

// A missing keyring entry is "no credential", not an error to escalate — the daemon has to tell "nobody has
// logged in" apart from "the keyring is broken", and only the first is ordinary.
func TestTheKeyringReportsAnAbsentEntryAsNoCredential(t *testing.T) {
	keyring.MockInit()

	_, err := keyringStore{}.get(keyringService, "https://absent.example.com")
	assert.ErrorIs(t, err, ErrNoCredential)
}

// The probe decides which backend a machine gets, and it writes rather than reads: a read of a missing
// entry looks the same on a working keyring and a broken one, so a read-based probe would pick the keyring
// for a machine that cannot store anything — and the failure would land at the one moment there is a real
// token to lose.
func TestTheProbeReportsAWorkingKeyring(t *testing.T) {
	keyring.MockInit()
	assert.True(t, keyringUsable())

	// ...and leaves nothing behind under a name that could be mistaken for a session.
	_, err := keyring.Get(keyringService, keyringProbeAccount)
	assert.ErrorIs(t, err, keyring.ErrNotFound)
}

// Sanitizing an instance URL into a filename is lossy, so it cannot be the whole name. `:8443` and `/8443`
// are both valid and distinct — a port and a path — and both collapse to the same underscores. With that
// alone, logging in to the second silently overwrote the first's refresh token, and a later load handed the
// daemon the wrong instance's credential.
func TestDistinctInstancesNeverShareATokenFile(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	for _, pair := range [][2]string{
		{"https://a.example.com:8443", "https://a.example.com/8443"},
		{"https://a.example.com/x", "https://a.example.com:x"},
		{"http://a.example.com", "https://a.example.com"},
		{"https://a.example.com", "https://a-example-com"},
	} {
		assert.NotEqual(t, fs.path(pair[0]), fs.path(pair[1]),
			"%q and %q are different instances and must not share a file", pair[0], pair[1])
	}

	// ...and the mapping is stable, or a daemon restart would not find what a login wrote.
	assert.Equal(t, fs.path("https://a.example.com:8443"), fs.path("https://a.example.com:8443"))
}

// Written out end to end, because the collision was invisible at the unit level: both writes succeed, and
// only the second read shows the loss.
func TestLoggingInToATwinInstanceDoesNotOverwriteTheOther(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	require.NoError(t, fs.set(keyringService, "https://a.example.com:8443", "nrt_port"))
	require.NoError(t, fs.set(keyringService, "https://a.example.com/8443", "nrt_path"))

	byPort, err := fs.get(keyringService, "https://a.example.com:8443")
	require.NoError(t, err)
	assert.Equal(t, "nrt_port", byPort, "the second login must not have overwritten the first")
}

// The readable prefix is there so a person can tell the files apart; the hash is what makes them distinct.
func TestATokenFilenameStaysReadableAndBounded(t *testing.T) {
	dir := t.TempDir()
	fs := fileStore{dir: dir}

	base := filepath.Base(fs.path("https://chat.example.com"))
	assert.Contains(t, base, "chat.example.com")
	assert.Less(t, len(base), 128, "must stay inside every filesystem's component limit")

	long := filepath.Base(fs.path("https://" + strings.Repeat("verylongsubdomain.", 20) + "example.com"))
	assert.Less(t, len(long), 128)
}
