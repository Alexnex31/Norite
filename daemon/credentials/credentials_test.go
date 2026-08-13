package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeIn builds a store over a temp directory with an explicit backend, so a test names the backend it
// means rather than depending on whatever the machine running it happens to have.
func storeIn(t *testing.T, secrets secretStore) *Store {
	t.Helper()
	s, err := openIn(t.TempDir(), secrets)
	require.NoError(t, err)
	return s
}

// memoryStore is a secret backend with no machine behind it, for the cases that are about Store's own
// behaviour rather than about where a token ends up.
type memoryStore struct {
	entries  map[string]string
	setErr   error
	failNext bool
}

func newMemoryStore() *memoryStore { return &memoryStore{entries: map[string]string{}} }

func (m *memoryStore) set(_, account, secret string) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.entries[account] = secret
	return nil
}

func (m *memoryStore) get(_, account string) (string, error) {
	if m.failNext {
		return "", errors.New("backend unavailable")
	}
	secret, ok := m.entries[account]
	if !ok {
		return "", ErrNoCredential
	}
	return secret, nil
}

func (m *memoryStore) delete(_, account string) error {
	delete(m.entries, account)
	return nil
}

func (m *memoryStore) describe() string { return "a test store" }

func sampleRecord() Record {
	return Record{
		InstanceURL: "https://chat.example.com",
		UserID:      "123456789",
		Username:    "ada",
		DeviceID:    "dev_abcdefgh",
		DeviceName:  "laptop",
	}
}

// ---------- round trip ----------

func TestASavedSessionComesBack(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, sampleRecord(), record)
	assert.Equal(t, "nrt_secret", token)
}

// The daemon reads this at every start, so absent has to be an answer rather than a fault.
func TestLoadingWithNoSessionSaysSoPlainly(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	_, _, err := store.Load()
	require.ErrorIs(t, err, ErrNoCredential)
	assert.Contains(t, err.Error(), "norite login",
		"the error is what a person sees when the daemon has nothing; it must say what to do")
}

func TestSavingReplacesTheEarlierSession(t *testing.T) {
	store := storeIn(t, newMemoryStore())
	require.NoError(t, store.Save(sampleRecord(), "nrt_first"))

	second := sampleRecord()
	second.Username = "grace"
	require.NoError(t, store.Save(second, "nrt_second"))

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "grace", record.Username)
	assert.Equal(t, "nrt_second", token)
}

// Logging out twice is something people do. The second must not be an error.
func TestClearIsIdempotent(t *testing.T) {
	store := storeIn(t, newMemoryStore())
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	require.NoError(t, store.Clear())
	require.NoError(t, store.Clear(), "clearing an already-cleared session is not a failure")

	_, _, err := store.Load()
	assert.ErrorIs(t, err, ErrNoCredential)
}

// The record is written after the secret, so a failure in between leaves an unreferenced token rather than
// a record pointing at one that is not there — a daemon that starts, finds a credential, and cannot use it.
func TestAFailedSecretWriteLeavesNoRecord(t *testing.T) {
	secrets := newMemoryStore()
	secrets.setErr = errors.New("keyring is locked")
	store := storeIn(t, secrets)

	require.Error(t, store.Save(sampleRecord(), "nrt_secret"))

	_, err := store.LoadRecord()
	assert.ErrorIs(t, err, ErrNoCredential, "a half-written session must not look like a session")
}

// Showing which account is logged in must never touch the keyring: on a locked one that read pops a system
// dialog, and a status command that does that is a status command nobody runs twice.
func TestLoadRecordDoesNotReachForTheSecret(t *testing.T) {
	secrets := newMemoryStore()
	store := storeIn(t, secrets)
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	secrets.failNext = true
	record, err := store.LoadRecord()
	require.NoError(t, err)
	assert.Equal(t, "ada", record.Username)
}

// ---------- what will not be stored ----------

func TestSaveRefusesAnUnusableSession(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	noInstance := sampleRecord()
	noInstance.InstanceURL = ""
	assert.Error(t, store.Save(noInstance, "nrt_secret"))

	noDevice := sampleRecord()
	noDevice.DeviceID = ""
	assert.Error(t, store.Save(noDevice, "nrt_secret"))

	// An empty token would store cleanly and fail at the daemon's first refresh, a long way from the cause.
	assert.Error(t, store.Save(sampleRecord(), ""))
}

// ---------- the record file ----------

func TestTheRecordFileIsOwnerOnlyAndHoldsNoSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes do not describe Windows ACLs")
	}
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_super_secret"))

	info, err := os.Stat(filepath.Join(dir, recordFileName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	data, err := os.ReadFile(filepath.Join(dir, recordFileName))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "nrt_super_secret",
		"the record is the non-secret half and is read by people debugging; a token must never be in it")
}

// A record left unreadable — a truncated write from an older build, an editor that mangled it — must say
// what and where, not fail with a bare syntax error.
func TestACorruptRecordNamesItsFile(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, recordFileName), []byte("{not json"), 0o600))

	_, _, loadErr := store.Load()
	require.Error(t, loadErr)
	assert.Contains(t, loadErr.Error(), recordFileName)
}

// No partial file is ever visible under the real name: the write goes to a temp file and is renamed.
func TestWritingTheRecordLeavesNoTemporaryFilesBehind(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret_again"))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.Equal(t, recordFileName, e.Name(), "only the record itself may remain")
	}
}

// ---------- instance URLs ----------

func TestParseInstanceURL(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		for input, want := range map[string]string{
			"https://chat.example.com":       "https://chat.example.com",
			"https://chat.example.com/":      "https://chat.example.com",
			"  https://chat.example.com  ":   "https://chat.example.com",
			"chat.example.com":               "https://chat.example.com",
			"http://localhost:8080":          "http://localhost:8080",
			"https://example.com/norite":     "https://example.com/norite",
			"https://example.com:8443/chat/": "https://example.com:8443/chat",
		} {
			got, err := ParseInstanceURL(input)
			require.NoError(t, err, "input %q", input)
			assert.Equal(t, want, got, "input %q", input)
		}
	})

	// A bare host becomes https, never http: this string decides where a password is sent, and a slip must
	// not be able to quietly downgrade the connection carrying it.
	t.Run("a bare host defaults to https", func(t *testing.T) {
		got, err := ParseInstanceURL("chat.example.com")
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(got, "https://"))
	})

	t.Run("refused", func(t *testing.T) {
		for _, input := range []string{
			"",
			"   ",
			"ftp://chat.example.com",
			"file:///etc/passwd",
			"https://",
			// Credentials in the URL would land in the record file, and from there into any bug report
			// that included it.
			"https://ada:hunter2@chat.example.com",
		} {
			_, err := ParseInstanceURL(input)
			assert.Error(t, err, "input %q must be refused", input)
		}
	})
}

// ---------- device IDs ----------

func TestDeviceIDsAreRandomAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id, err := NewDeviceID()
		require.NoError(t, err)
		require.False(t, seen[id], "device IDs must not repeat")
		seen[id] = true

		assert.True(t, strings.HasPrefix(id, "dev_"))
		// It travels in a JSON body and is stored on a session list; anything needing escaping is a
		// nuisance everywhere it is displayed.
		assert.Equal(t, id, strings.TrimSpace(id))
		for _, r := range strings.TrimPrefix(id, "dev_") {
			assert.True(t,
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_',
				"unexpected character %q in %q", r, id)
		}
	}
}
