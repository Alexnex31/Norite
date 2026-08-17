package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
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
// behavior rather than about where a token ends up.
type memoryStore struct {
	entries   map[string]string
	setErr    error
	deleteErr error
	failNext  bool
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
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.entries, account)
	return nil
}

func (m *memoryStore) describe() string { return "a test store" }

// Deliberately not "keyring" or "file": Store.backends must not have an entry for it, so a record saved
// through this double resolves back to the double rather than to a real backend pointed at the temp dir.
func (m *memoryStore) name() string { return "memory" }

// namedStore lets a double answer to one of the real backend names, so the cases where the record and the
// probe disagree can be built without touching the machine's own keyring.
type namedStore struct {
	secretStore
	backend string
}

func (n namedStore) name() string { return n.backend }

// twoBackends wires a store so that a recorded backend name resolves to one of these doubles rather than to
// the real keyring or a file in the test's directory.
func twoBackends(s *Store, keyring, file secretStore) {
	s.backends = map[string]secretStore{backendKeyring: keyring, backendFile: file}
}

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
	assert.Equal(t, "nrt_secret", token)

	// Save stamps where the secret actually went, so a later process reads it back from there rather than
	// from wherever its own probe happens to point.
	want := sampleRecord()
	want.SecretBackend = "memory"
	assert.Equal(t, want, record)
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
		// The record and the lock guarding it are expected; a leftover temp file is what this rules out.
		assert.Contains(t, []string{recordFileName, lockFileName}, e.Name(),
			"no partial write may be left behind, only the record and its lock")
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

			// This string is printed, and refusing it is the only honest answer: sanitizing would leave a
			// URL naming a different instance than the one that was typed. This is stricter than the CLI's
			// sanitizer on purpose, because rejecting is the recoverable direction — nothing unprintable
			// belongs in a URL. url.Parse catches only the first of these: U+009B is CSI with no ESC in
			// front of it, an invalid byte reaches a byte-oriented terminal as whatever it was, U+00A0 is a
			// hostname that reads as another one, an override prints what follows it backwards, and a
			// zero-width space is a host nobody can see.
			"https://chat.example.com/\x1b[2K",
			"https://chat.example.com/\u009b2K",
			"https://chat.example.com/\xff",
			"https://chat\u00a0example.com",
			"https://chat.example.com/\u202enimda",
			"https://chat\u200b.example.com",
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
		id, err := newDeviceID()
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

// The identity belongs to the installation, so asking twice must answer the same thing — including across
// separate Store values, since the CLI and the daemon are different processes.
func TestTheDeviceIDIsStableForTheInstallation(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)

	first, err := store.DeviceID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(first, "dev_"))

	again, err := store.DeviceID()
	require.NoError(t, err)
	assert.Equal(t, first, again)

	other, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	second, err := other.DeviceID()
	require.NoError(t, err)
	assert.Equal(t, first, second, "a second process on the same installation must see the same identity")
}

// A logout must not take the device identity with it. It did when the ID lived in the record: the next
// login minted a fresh one, which adds a second entry to the account's session list while the family the
// old ID named stays live for its full TTL, because logging out locally revokes nothing server-side.
func TestLoggingOutKeepsTheDeviceIdentity(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	before, err := store.DeviceID()
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	require.NoError(t, store.Clear())

	_, _, err = store.Load()
	require.ErrorIs(t, err, ErrNoCredential, "the session itself must be gone")

	after, err := store.DeviceID()
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// An installation from before the identity had its own file keeps the ID its record already carries —
// otherwise the first login after an upgrade looks exactly like the logout case above.
func TestAnExistingRecordsDeviceIDIsAdopted(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)

	// Written directly, as an older build would have left it: a record, and no device file beside it.
	require.NoError(t, store.writeRecord(sampleRecord()))

	id, err := store.DeviceID()
	require.NoError(t, err)
	assert.Equal(t, sampleRecord().DeviceID, id)

	// ...and it is now in the file, so a later logout cannot lose it.
	require.NoError(t, store.Clear())
	after, err := store.DeviceID()
	require.NoError(t, err)
	assert.Equal(t, sampleRecord().DeviceID, after)
}

// A truncated write leaves a file that exists and says nothing. That is not an identity.
func TestAnEmptyDeviceFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, deviceFileName), []byte("  \n"), 0o600))

	id, err := store.DeviceID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(id, "dev_"))
}

// ---------- which backend holds the secret ----------

// The record is believed over the probe. A process whose probe points somewhere else must still find the
// secret where it was actually put — otherwise the daemon reads an empty file store, gets ErrNoCredential,
// and reports "no stored credential; run `norite login`" for a credential sitting beside it in account.json.
func TestTheSecretIsReadFromTheBackendTheRecordNames(t *testing.T) {
	dir := t.TempDir()
	keyring, file := newMemoryStore(), newMemoryStore()

	// A process that reaches the keyring stores there.
	stored, err := openIn(dir, namedStore{keyring, backendKeyring})
	require.NoError(t, err)
	twoBackends(stored, keyring, file)
	require.NoError(t, stored.Save(sampleRecord(), "nrt_secret"))

	record, err := stored.LoadRecord()
	require.NoError(t, err)
	require.Equal(t, backendKeyring, record.SecretBackend, "Save must record where it put the secret")
	require.NotEmpty(t, keyring.entries)

	// A second process whose probe picks the other backend, which holds nothing.
	other, err := openIn(dir, namedStore{file, backendFile})
	require.NoError(t, err)
	twoBackends(other, keyring, file)

	_, token, err := other.Load()
	require.NoError(t, err, "the record names a backend; the probe's answer is not evidence about it")
	assert.Equal(t, "nrt_secret", token)
}

// The same mismatch on the delete path, which was worse: absent is success in either backend, so a logout
// deleted nothing, removed the record, and reported "Removed the credential" for a token still valid for its
// full TTL — and unreachable afterwards, since Clear derives the key from the record it just deleted.
func TestLogoutRemovesTheSecretFromTheBackendThatHoldsIt(t *testing.T) {
	dir := t.TempDir()
	keyring, file := newMemoryStore(), newMemoryStore()

	stored, err := openIn(dir, namedStore{keyring, backendKeyring})
	require.NoError(t, err)
	twoBackends(stored, keyring, file)
	require.NoError(t, stored.Save(sampleRecord(), "nrt_secret"))
	require.NotEmpty(t, keyring.entries)

	other, err := openIn(dir, namedStore{file, backendFile})
	require.NoError(t, err)
	twoBackends(other, keyring, file)
	require.NoError(t, other.Clear())

	assert.Empty(t, keyring.entries, "the secret must be removed from where it actually is")
}

// And when it genuinely cannot be reached, that is reported instead of being reported as success.
func TestLogoutSaysSoWhenTheSecretCannotBeRemoved(t *testing.T) {
	dir := t.TempDir()
	keyring, file := newMemoryStore(), newMemoryStore()

	store, err := openIn(dir, namedStore{keyring, backendKeyring})
	require.NoError(t, err)
	twoBackends(store, keyring, file)
	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))

	keyring.deleteErr = errors.New("the keyring is locked")
	err = store.Clear()
	require.Error(t, err, "a delete that failed must never be reported as a credential removed")
	assert.Contains(t, err.Error(), "may remain")

	// The record stays, so a later logout from a session that can reach the backend still knows what to
	// remove. Removing it anyway is what put the token beyond reach of every future logout.
	_, loadErr := store.LoadRecord()
	assert.NoError(t, loadErr)
}

// Signing in from a session that cannot reach the previous backend must still work — this CLI exists for
// SSH — but it must say what it left behind.
func TestALoginThatCannotReachTheOldBackendProceedsAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	keyring, file := newMemoryStore(), newMemoryStore()

	first, err := openIn(dir, namedStore{keyring, backendKeyring})
	require.NoError(t, err)
	twoBackends(first, keyring, file)
	require.NoError(t, first.Save(sampleRecord(), "nrt_first"))

	keyring.deleteErr = errors.New("no session bus")

	var told []string
	second, err := openIn(dir, namedStore{file, backendFile})
	require.NoError(t, err)
	twoBackends(second, keyring, file)
	second.Notify = func(msg string) { told = append(told, msg) }

	elsewhere := sampleRecord()
	elsewhere.InstanceURL = "https://other.example.com"
	require.NoError(t, second.Save(elsewhere, "nrt_second"), "an unreachable old backend must not block a login")

	require.Len(t, told, 1)
	assert.Contains(t, told[0], "norite logout")

	_, token, err := second.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_second", token)
}

// A record too corrupt to say what it named cannot be cleaned up from, and logging in must not be refused
// because of it — but the token it named is still out there, and only a person can chase it.
func TestAnUnreadableRecordIsReportedRatherThanBlockingALogin(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, recordFileName), []byte("{not json"), 0o600))

	var told []string
	store.Notify = func(msg string) { told = append(told, msg) }

	require.NoError(t, store.Save(sampleRecord(), "nrt_secret"))
	require.Len(t, told, 1)
	assert.Contains(t, told[0], "may still")
}

// ---------- renewing a token that may have been replaced underneath ----------

// The daemon reads the record, spends up to thirty seconds on the network, then writes back. A login landing
// in that window has already stored a session; Save would take the stale record as the truth, delete the
// token the login just stored, and rewrite the record back to the old instance.
func TestRenewingRefusesWhenTheSessionWasReplaced(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	read := sampleRecord()
	require.NoError(t, store.Save(read, "nrt_old"))

	// The login that happened while the refresh was in flight.
	fresh := sampleRecord()
	fresh.InstanceURL = "https://other.example.com"
	require.NoError(t, store.Save(fresh, "nrt_fresh"))

	err := store.ReplaceToken(read, "nrt_old", "nrt_renewed")
	require.ErrorIs(t, err, ErrCredentialChanged)

	record, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.com", record.InstanceURL, "the login's record must survive")
	assert.Equal(t, "nrt_fresh", token, "the login's token must survive")
}

// A logout in the same window is the other half of it.
func TestRenewingRefusesWhenTheSessionWasCleared(t *testing.T) {
	store := storeIn(t, newMemoryStore())
	require.NoError(t, store.Save(sampleRecord(), "nrt_old"))
	require.NoError(t, store.Clear())

	err := store.ReplaceToken(sampleRecord(), "nrt_old", "nrt_renewed")
	require.ErrorIs(t, err, ErrNoCredential)

	_, _, loadErr := store.Load()
	assert.ErrorIs(t, loadErr, ErrNoCredential, "a logout must not be undone by a refresh already in flight")
}

// The gap the instance/device comparison left, and it is the ordinary way to sign in again: same instance
// (resolveInstance falls back to the stored URL), same device (it is per installation now), so neither
// field changes — while the backend revokes the device's previous family on that login and issues a new
// token. Overwriting it puts back the one the login just killed, and the next start 401s on a session the
// person had just successfully created.
func TestRenewingRefusesWhenTheTokenWasReplacedForTheSameSession(t *testing.T) {
	store := storeIn(t, newMemoryStore())

	read := sampleRecord()
	require.NoError(t, store.Save(read, "nrt_old"))

	// `norite login` to the same instance, same device: the record is identical, the token is not.
	require.NoError(t, store.Save(read, "nrt_from_the_new_login"))

	err := store.ReplaceToken(read, "nrt_old", "nrt_renewed")
	require.ErrorIs(t, err, ErrCredentialChanged)

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_from_the_new_login", token, "the login's token must survive")
}

// A lock this could not take says nothing about what is on disk, so it must be distinguishable from a write
// that was refused: the caller clears the credential on the second and must not on the first.
func TestAnUnreadableStoreIsItsOwnAnswer(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, store.Save(sampleRecord(), "nrt_old"))
	store.lockWait = 20 * time.Millisecond

	// Hold the lock the way another process would, for longer than the wait.
	held := flock.New(filepath.Join(dir, lockFileName))
	require.NoError(t, held.Lock())
	t.Cleanup(func() { _ = held.Unlock() })

	err = store.ReplaceToken(sampleRecord(), "nrt_old", "nrt_renewed")
	require.ErrorIs(t, err, ErrStoreUnavailable)
	assert.NotErrorIs(t, err, ErrCredentialChanged)
}

// A secret that cannot be read is not a write that was refused. The caller answers a refused write by
// deleting the credential, so reporting an unreadable one the same way deletes a credential that was
// perfectly good — a token file whose mode changed under a restore, or a keyring that stopped answering.
func TestRenewingReportsAnUnreadableSecretAsUnavailable(t *testing.T) {
	secrets := newMemoryStore()
	store := storeIn(t, secrets)
	require.NoError(t, store.Save(sampleRecord(), "nrt_old"))

	secrets.failNext = true
	err := store.ReplaceToken(sampleRecord(), "nrt_old", "nrt_renewed")

	require.ErrorIs(t, err, ErrStoreUnavailable)
	assert.NotErrorIs(t, err, ErrCredentialChanged)
}

// The ordinary case: nothing else touched the store, so the token is replaced and the record is left alone.
func TestRenewingReplacesOnlyTheSecret(t *testing.T) {
	store := storeIn(t, newMemoryStore())
	require.NoError(t, store.Save(sampleRecord(), "nrt_old"))

	before, err := store.LoadRecord()
	require.NoError(t, err)

	require.NoError(t, store.ReplaceToken(sampleRecord(), "nrt_old", "nrt_renewed"))

	after, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_renewed", token)
	assert.Equal(t, before, after, "a renewal has nothing to say about the record")
}

// Both files this reads are plain files a person can edit, and the instance URL is re-parsed on every use
// for exactly that reason while the device ID was taken on trust. A value the backend refuses makes login
// answer 401, which the CLI maps to the same vague "email and password did not match" a wrong password
// gets — so the person retypes a correct password forever, and an ID adopted from a broken record was kept.
func TestAnUnusableStoredDeviceIDIsReplaced(t *testing.T) {
	for name, stored := range map[string]string{
		"too long for the instance to accept": strings.Repeat("d", maxDeviceID+1),
		// json.Marshal turns each invalid byte into a 3-byte U+FFFD on the way to the instance, so this
		// arrives well over the 128-byte bound even though the file is comfortably under it.
		"invalid UTF-8 that grows when marshaled": "dev_" + strings.Repeat("\xff", 50),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := openIn(dir, newMemoryStore())
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, deviceFileName), []byte(stored), 0o600))

			id, err := store.DeviceID()
			require.NoError(t, err)
			assert.NotEqual(t, stored, id, "a value the instance can only refuse must not be kept")
			assert.True(t, strings.HasPrefix(id, "dev_"))
		})
	}
}

// The other direction, and the one that costs more to get wrong: an identifier the instance accepts must
// be kept, whatever it looks like. An earlier version of this check also demanded printable runes, which
// discarded working IDs — and replacing a working device ID strands the previous refresh family for its
// full TTL and adds a session-list entry nobody created, which is the harm the ID exists to avoid.
func TestAnUnusualButAcceptableDeviceIDIsKept(t *testing.T) {
	// auth.normalizeDeviceID rejects only empty and over-128-bytes; nothing in either client renders this.
	for _, stored := range []string{"dev_\x1b[2Kada", "dev_\u200bada", "device id with spaces", "夢"} {
		dir := t.TempDir()
		store, err := openIn(dir, newMemoryStore())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, deviceFileName), []byte(stored), 0o600))

		id, err := store.DeviceID()
		require.NoError(t, err)
		assert.Equal(t, stored, id, "an identifier the instance would accept must not be replaced")
	}
}

// The adoption path takes the same care: a hand-edited record must not put an unusable ID into the file
// where it would then be kept forever.
func TestAnUnusableDeviceIDIsNotAdoptedFromTheRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)

	broken := sampleRecord()
	broken.DeviceID = strings.Repeat("d", maxDeviceID+1)
	require.NoError(t, store.writeRecord(broken))

	id, err := store.DeviceID()
	require.NoError(t, err)
	assert.NotEqual(t, broken.DeviceID, id)
}

// OpenLocalForTest promises a store that never touches the machine's keyring. openIn wires "keyring" to the
// real one, so the promise has to be re-made after it — a record naming that backend would otherwise send
// reads and deletes to the developer's own keyring under the real service name.
func TestOpenLocalForTestNeverReachesTheRealKeyring(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenLocalForTest(dir)
	require.NoError(t, err)

	for _, backend := range []string{backendKeyring, backendFile} {
		if _, ok := store.backends[backend].(fileStore); !ok {
			t.Errorf("backend %q resolves to %T, which is not the file store", backend, store.backends[backend])
		}
	}

	// And a record naming the keyring round-trips through the directory rather than the machine.
	record := sampleRecord()
	record.SecretBackend = backendKeyring
	require.NoError(t, store.writeRecord(record))
	require.NoError(t, fileStore{dir: dir}.set(keyringService, record.InstanceURL, "nrt_secret"))

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_secret", token)
}

// ---------- switching instances ----------

// Signing in to a different instance has to take the previous instance's secret with it. Without that the
// old refresh token stays live at rest for its full TTL, and `norite logout` cannot reach it — Clear only
// knows the instance the current record names — so it would report "Removed the credential" untruthfully.
func TestSigningInToAnotherInstanceRemovesTheOldSecret(t *testing.T) {
	secrets := newMemoryStore()
	store := storeIn(t, secrets)

	first := sampleRecord()
	first.InstanceURL = "https://a.example.com"
	require.NoError(t, store.Save(first, "nrt_a"))

	second := sampleRecord()
	second.InstanceURL = "https://b.example.com"
	require.NoError(t, store.Save(second, "nrt_b"))

	assert.NotContains(t, secrets.entries, "https://a.example.com",
		"the old instance's refresh token must not be left live at rest")
	assert.Equal(t, "nrt_b", secrets.entries["https://b.example.com"])

	// ...and a logout afterwards leaves nothing at all behind.
	require.NoError(t, store.Clear())
	assert.Empty(t, secrets.entries)
}

// Signing in again to the *same* instance is the ordinary case and must not disturb anything.
func TestSigningInAgainToTheSameInstanceKeepsWorking(t *testing.T) {
	secrets := newMemoryStore()
	store := storeIn(t, secrets)

	require.NoError(t, store.Save(sampleRecord(), "nrt_first"))
	require.NoError(t, store.Save(sampleRecord(), "nrt_second"))

	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_second", token)
	assert.Len(t, secrets.entries, 1)
}

// ---------- recovering from a broken record ----------

// An unreadable record is exactly when someone reaches for `norite logout`, and it used to be the one case
// it refused: Clear read the record first and returned the parse error, leaving them to delete files by
// hand. The record goes; what cannot be done is identify the secret it named, so that is said rather than
// passed over in silence.
func TestClearRemovesAnUnreadableRecordAndSaysWhatItCouldNotDo(t *testing.T) {
	dir := t.TempDir()
	store, err := openIn(dir, newMemoryStore())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, recordFileName), []byte("{not json"), 0o600))

	err = store.Clear()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may remain")

	assert.NoFileExists(t, filepath.Join(dir, recordFileName))

	// ...and the store is usable again, which is the point.
	require.NoError(t, store.Save(sampleRecord(), "nrt_fresh"))
	_, token, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "nrt_fresh", token)
}
