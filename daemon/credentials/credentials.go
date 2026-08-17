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
	"time"
	"unicode"
	"unicode/utf8"

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

// deviceFileName holds this installation's identity, and is deliberately not part of the record.
//
// A device ID belongs to the installation, not to the session (ADR 0011) — which means logging out must not
// take it away. It was inside account.json first, and Clear removes that file, so a logout silently minted
// a new identity at the next login: a second entry in the account's session list, while the refresh family
// the old ID named stayed live for its full TTL, because a local logout revokes nothing server-side. Two
// files is what makes that impossible rather than remembered.
const deviceFileName = "device-id"

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
	// DeviceName is what the account's session list shows a person. Free text, theirs to recognize.
	DeviceName string `json:"device_name"`
	// SecretBackend names where the refresh token actually went — "keyring" or "file".
	//
	// Without it, every read and every delete goes to whichever backend *this process* chose by probing,
	// and the probe answers differently across processes on one machine: a login in a desktop session
	// reaches the keyring, the systemd user unit that starts before the keyring is unlocked does not, and
	// an SSH session has no session bus at all. That produced two failures with one cause. The daemon read
	// the wrong backend, found nothing, and reported "no stored credential; run `norite login`" for a
	// credential sitting in account.json beside it — a loop no amount of logging in resolves. And `norite
	// logout` deleted from the wrong backend, where absent is indistinguishable from already-gone, then
	// removed the record and reported success: the token stayed live for its full TTL, now unreachable,
	// because Clear derives the key from the record it just deleted.
	//
	// Empty means a record written before this field existed; the auto-chosen backend is used, exactly as
	// it was then, and the next Save fills it in.
	SecretBackend string `json:"secret_backend,omitempty"`
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
	// Refused rather than sanitized, because this value is printed, sent, and turned into a filename: a URL
	// quietly altered to make it printable would name a different instance than the one that was typed.
	// Refusing is also the safe direction to be wrong in — the answer is to type it again — which is why
	// this is stricter than the CLI's sanitizer and rejects everything unprintable rather than the subset
	// that can act on a terminal. Nothing unprintable belongs in a URL.
	//
	// url.Parse refuses ASCII control characters itself, but not U+009B — which is CSI, needs no ESC in
	// front of it, and survives into u.Path — and not U+00A0, which is a hostname that reads as another
	// one. Invalid UTF-8 is checked separately: it decodes to U+FFFD, which *is* printable.
	if !utf8.ValidString(trimmed) {
		return "", fmt.Errorf("%q is not valid UTF-8, so it cannot be an instance URL", raw)
	}
	if strings.ContainsFunc(trimmed, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return "", errors.New("an instance URL must not contain control or invisible characters")
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

// newDeviceID returns an identifier for this installation.
//
// Unexported deliberately. Store.DeviceID is the only way to obtain one, because it is the only way that
// mints once, adopts what a record already carries, and survives a logout — and a second exported route to
// a fresh ID is exactly how per-login rotation would come back, which strands the previous refresh family
// and adds a session-list entry every time. The CLI called this directly until M7 moved that decision into
// the store; nothing outside this package needs it now, and the compiler is a better guard than a comment.
//
// Random rather than derived from a hostname or a MAC address: it is sent to the instance and stored on the
// account's session list, so deriving it from anything about the machine would put that detail on a server
// the person may not control, to solve a problem randomness solves for free.
func newDeviceID() (string, error) {
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

	// Notify receives a sentence about state the person running the command should know about, when it is
	// not a failure: a credential left behind somewhere this process cannot reach, a previous record too
	// broken to say what it named. Callers set it to their own output — the CLI prints, the daemon logs.
	//
	// It exists because ADR 0025's rule is that degradation is never silent, and the alternative shapes are
	// both wrong: failing the operation would make `norite login` impossible over SSH on any machine that
	// once logged in at its desktop, and returning nil would hide a live token from the only person able
	// to do anything about it. Nil is allowed and drops the message, which is what tests want.
	Notify func(string)

	// backends maps a name written into a record to the store that holds that secret. Populated by openIn;
	// a test replaces the entries, because resolving "keyring" for real reaches the machine's own keyring
	// and the paths where the record and the probe disagree are precisely the ones that were wrong, so they
	// have to be reachable without depositing live tokens on whichever developer runs the suite.
	//
	// A nil map reads fine, so a Store built without this still falls back to the probe rather than panicking.
	backends map[string]secretStore

	// lockWait overrides how long withLock waits, and exists so the test for a busy store does not spend the
	// real five seconds proving it. Zero means the constant.
	lockWait time.Duration

	// betweenWrites runs between the two halves of a Save, and exists only so a test can hold the window
	// open and prove the lock closes it. Nothing outside this package can set it, and production leaves it
	// nil: the interleaving it makes visible is real but narrow, and a test that waits for it to happen by
	// chance proves nothing on the run where it does not.
	betweenWrites func()
}

func (s *Store) notify(format string, args ...any) {
	if s.Notify != nil {
		s.Notify(fmt.Sprintf(format, args...))
	}
}

// secretsFor returns the backend that holds this record's secret.
//
// The record is believed over the probe. A probe answers "where would a new secret go on this machine, in
// this process"; it is not evidence about where an existing one already is, and treating it as though it
// were is what let a read and a delete miss a token that was plainly there.
func (s *Store) secretsFor(record Record) secretStore {
	if named, ok := s.backends[record.SecretBackend]; ok {
		return named
	}
	// A record written before the field existed, or naming a backend from some later version. The probe's
	// choice is what it would have used then, which is the best guess available and what M7 shipped with.
	return s.secrets
}

// Open resolves the store for the current user.
func Open() (*Store, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	return openIn(dir, newSecretStore(dir))
}

// OpenIn resolves the store in an explicit directory instead of the daemon's own state directory.
//
// For tests, and for anything that has to be told where to look rather than deriving it — the CLI's tests
// drive a whole login against a temporary directory, which is the only way to exercise the flow without
// writing to the machine running them.
func OpenIn(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("a credential store needs a directory")
	}
	return openIn(dir, newSecretStore(dir))
}

func openIn(dir string, secrets secretStore) (*Store, error) {
	return &Store{
		dir:     dir,
		secrets: secrets,
		backends: map[string]secretStore{
			backendKeyring: keyringStore{},
			backendFile:    fileStore{dir: dir},
		},
	}, nil
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

	return s.withLock(true, func() error {
		// A new secret goes where this machine can put one now, which is what the probe answers. Where the
		// *previous* one went is a different question, and only the record it was written with can answer
		// it — see Record.SecretBackend.
		record.SecretBackend = s.secrets.name()

		if err := s.removeSuperseded(record.InstanceURL, record.SecretBackend); err != nil {
			return err
		}

		if err := s.secrets.set(keyringService, record.InstanceURL, refreshToken); err != nil {
			return err
		}
		if s.betweenWrites != nil {
			s.betweenWrites()
		}
		return s.writeRecord(record)
	})
}

// removeSuperseded takes the previous session's secret with the new one, when they are not the same secret.
//
// Without this the old refresh token stays live at rest for its full TTL — in a keyring entry or a file
// nothing references — and `norite logout` cannot reach it either, because Clear only knows what the
// current record names. It would then report "Removed the credential", untruthfully.
//
// Two failures are possible here and they are not the same failure:
//
//   - The previous secret is in the backend this save is about to write to, and the delete failed. That
//     backend will almost certainly refuse the write that follows, so failing costs a login that was not
//     going to work anyway — and proceeding would leave a credential nothing will ever clean up.
//
// The two parameters are the new secret's identity — where it is going and which backend it is going to.
// They were read off the record instead, which only worked because Save stamps SecretBackend two lines
// before calling this: reorder those statements and both comparisons silently become `== ""`, the
// same-secret short-circuit stops firing, and a re-login to the same instance starts by deleting the
// secret it is about to write.
//
//   - The previous secret is in the *other* backend, and this process cannot reach it. The reasoning above
//     does not carry: a keyring this session cannot see says nothing about whether the file write will
//     work. Failing here would mean that a machine which once logged in at its desktop can never log in
//     over SSH again — on a CLI built for SSH. So it proceeds, and says so, which is ADR 0025's rule.
func (s *Store) removeSuperseded(instanceURL, backend string) error {
	previous, err := s.readRecord()
	switch {
	case errors.Is(err, ErrNoCredential):
		return nil
	case err != nil:
		// Unreadable, so it cannot say what it named. Logging in is the fix for a broken record and must
		// not be refused because of one — but something may be left behind, and only a person can chase it.
		s.notify("The previous credential record could not be read, so an earlier refresh token may still " +
			"be stored on this machine. It cannot be identified from here; remove it by hand if you need it gone.")
		return nil
	case previous.InstanceURL == "":
		return nil
	}

	from := s.secretsFor(previous)
	if previous.InstanceURL == instanceURL && from.name() == backend {
		return nil // the same secret, about to be overwritten in place
	}

	if err := from.delete(keyringService, previous.InstanceURL); err != nil {
		if from.name() == backend {
			return fmt.Errorf("removing the credential for %s: %w", previous.InstanceURL, err)
		}
		s.notify("The previous credential for %s is stored in %s, which this session cannot reach, so it "+
			"could not be removed: %v. It stays valid until it expires — run `norite logout` from a session "+
			"that can reach it.", previous.InstanceURL, from.describe(), err)
	}
	return nil
}

// Load returns the stored session.
func (s *Store) Load() (Record, string, error) {
	var (
		record Record
		token  string
	)
	err := s.withLock(false, func() error {
		var err error
		if record, err = s.readRecord(); err != nil {
			return err
		}
		if err := record.Validate(); err != nil {
			return fmt.Errorf("stored credential is unusable: %w", err)
		}
		// From the backend the record names. Asking the probe instead is what made the daemon report "no
		// stored credential" when the secret was in a keyring it happened not to reach: fileStore.get finds
		// no file and answers ErrNoCredential, which is indistinguishable from nobody having logged in.
		token, err = s.secretsFor(record).get(keyringService, record.InstanceURL)
		return err
	})
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
	var record Record
	err := s.withLock(false, func() error {
		var err error
		record, err = s.readRecord()
		return err
	})
	return record, err
}

// Clear removes the stored session. Absent is success: logging out twice is not an error.
func (s *Store) Clear() error {
	return s.withLock(true, func() error {
		record, err := s.readRecord()
		switch {
		case errors.Is(err, ErrNoCredential):
			return nil
		case err != nil:
			// An unreadable record — a truncated write, a full disk, a hand edit — used to make this
			// return, which left `norite logout` unable to clear the very state that was broken and the
			// person deleting files by hand. The record is removed anyway; what cannot be done is find the
			// secret it named, so that is reported rather than passed over.
			if rmErr := os.Remove(s.recordPath()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("removing the unreadable credential record: %w", rmErr)
			}
			return fmt.Errorf("%w; the record has been removed, but any stored secret it named could not "+
				"be identified and may remain — sign in again to replace it", err)
		}

		// The secret goes first, for the same reason it is written first: whichever half survives a partial
		// failure, it should be the inert one.
		//
		// From the backend the record names, and the record is removed only once that has succeeded. Both
		// halves of that matter: deleting from the probe's choice made the delete a no-op whenever the two
		// disagreed — absent reads as already-gone in either backend — and removing the record anyway put
		// the token beyond reach of every future logout, because this key comes from the record. What the
		// caller then printed was "Removed the credential", of a token still valid for its full TTL.
		holder := s.secretsFor(record)
		if err := holder.delete(keyringService, record.InstanceURL); err != nil {
			return fmt.Errorf("the credential for %s is stored in %s and could not be removed, so it may "+
				"remain valid until it expires: %w", record.InstanceURL, holder.describe(), err)
		}
		if err := os.Remove(s.recordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing the credential record: %w", err)
		}
		return nil
	})
}

// ErrCredentialChanged reports that the stored session was replaced while it was being renewed.
var ErrCredentialChanged = errors.New("the stored credential changed while it was being renewed")

// ReplaceToken swaps the secret of a session that is still the one it was read from.
//
// Save is the wrong operation for a token refresh, and using it was a bug with teeth. The daemon reads the
// record under a shared lock, spends up to thirty seconds on a network round trip, and only then writes
// back — so a `norite login` that lands in that window has already stored a session by the time Save runs.
// Save would then take its *stale* record as the truth: seeing an instance that no longer matches, it would
// delete the refresh token the login had just stored and rewrite account.json back to the old instance. The
// lock makes each operation atomic and does nothing about a read-modify-write spanning two of them, which
// is the interleaving the lock file itself was written for — starting the daemon and logging in are things
// people do seconds apart, in either order.
//
// So this refuses instead. It never writes the record, because a renewal has nothing to say about it, and
// it touches the secret only while the session on disk is still the one the caller renewed.
//
// "Still the one" means the *token*, not the account it belongs to. Comparing the instance and the device
// was not enough and the gap was the ordinary case rather than an exotic one: signing in again defaults to
// the same instance (resolveInstance falls back to the stored URL) with the same device ID (it is per
// installation now), so neither field changes — while the backend, on that login, revokes the device's
// previous family and issues a new token (auth.Service.Login). The daemon would then pass the guard and
// overwrite a live credential with the one the login had just killed, and the next start would 401 on a
// session the person had just successfully created. So `spent` is what was read, and it has to still be
// there.
func (s *Store) ReplaceToken(renewed Record, spent, refreshToken string) error {
	if refreshToken == "" {
		return errors.New("refusing to store an empty refresh token")
	}

	return s.withLock(true, func() error {
		current, err := s.readRecord()
		switch {
		case errors.Is(err, ErrNoCredential):
			return err // a logout took it, and it is not coming back
		case err != nil:
			return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
		}
		if current.InstanceURL != renewed.InstanceURL || current.DeviceID != renewed.DeviceID {
			return ErrCredentialChanged
		}

		secrets := s.secretsFor(current)
		stored, err := secrets.get(keyringService, current.InstanceURL)
		switch {
		case errors.Is(err, ErrNoCredential):
			return ErrCredentialChanged
		case err != nil:
			// Unreadable is not refused. A token file whose mode changed under us, or a keyring that has
			// stopped answering, says nothing about what is in it — and the caller's answer to a refused
			// write is to delete the credential, which here would delete one that is perfectly good.
			return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
		}
		if stored != spent {
			return ErrCredentialChanged
		}

		// Past this point the stored secret is known to be the one being replaced, so a failure really is
		// the write being refused, and the caller may act on that.
		return secrets.set(keyringService, current.InstanceURL, refreshToken)
	})
}

// DeviceID returns this installation's device identifier, minting one the first time it is asked for.
//
// Every login on this machine uses the same value, including one that follows a logout. Rotating it is what
// reuse detection reads as a stolen token (M4), and a fresh one per login would strand the previous refresh
// family until it expired while adding a session-list entry nobody created.
func (s *Store) DeviceID() (string, error) {
	var id string
	// Exclusive: this reads, and may then write.
	err := s.withLock(true, func() error {
		var err error
		id, err = s.deviceID()
		return err
	})
	return id, err
}

func (s *Store) deviceID() (string, error) {
	// ReadFile returns nil data on error and TrimSpace("") is "", so a missing file and a file truncated to
	// nothing converge here without either needing a branch: both fall to the minting below.
	data, err := os.ReadFile(s.devicePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading the device identifier: %w", err)
	}
	if id := strings.TrimSpace(string(data)); usableDeviceID(id) {
		return id, nil
	}

	// An installation that predates this file keeps the ID its record already carries. Without this, the
	// first login after an upgrade would look exactly like the logout case it exists to prevent — and it
	// only ever reads the record, so the identity moves one way and cannot be dragged backwards later.
	if record, err := s.readRecord(); err == nil && usableDeviceID(record.DeviceID) {
		return record.DeviceID, s.writeDeviceID(record.DeviceID)
	}

	id, err := newDeviceID()
	if err != nil {
		return "", err
	}
	return id, s.writeDeviceID(id)
}

// maxDeviceID mirrors the backend's own bound (auth.MaxDeviceIDLength). Checked here so that a value which
// can only ever be refused is replaced while that is still cheap.
const maxDeviceID = 128

// usableDeviceID reports whether a stored identifier is one the instance could accept.
//
// Both files this reads are, in this package's own words, plain files a person can edit — and the instance
// URL is re-parsed on every use for exactly that reason while this was taken on trust. The failure it
// prevents is not subtle but is very hard to read: a device ID the backend refuses makes login answer 401,
// which the CLI maps to the same deliberately vague "that email and password did not match an account"
// every wrong password gets. The person would retype a correct password forever. Worse, an ID adopted from
// a broken record was written to device-id and kept, so the state repaired itself only by hand.
//
// It checks exactly what auth.normalizeDeviceID checks — non-empty, at most 128 *bytes* — and nothing
// more. An earlier version also required every rune to be printable, which was wrong in the expensive
// direction: the instance accepts those bytes happily, so this discarded working identifiers, and replacing
// a working device ID is the harm Record.DeviceID exists to prevent — it strands the previous refresh
// family for its full TTL and adds a session-list entry nobody created. Nothing renders this value either,
// so rule 19 has no claim on it.
//
// Valid UTF-8 is required, and that is a length check rather than an aesthetic one: encoding/json coerces
// each invalid byte to U+FFFD when the ID is marshaled into the login request, so a 60-byte identifier can
// arrive at the instance as 140 and be refused for a length the file never had.
//
// A bad value is replaced rather than reported: it is not a credential, nothing is lost by minting another,
// and refusing to log in over a file the person never edited would be the worse answer.
func usableDeviceID(id string) bool {
	return id != "" && len(id) <= maxDeviceID && utf8.ValidString(id)
}

func (s *Store) writeDeviceID(id string) error {
	if err := writeFileAtomically(s.devicePath(), []byte(id+"\n")); err != nil {
		return fmt.Errorf("recording this installation's device identifier: %w", err)
	}
	return nil
}

// SecretLocation describes where the token is actually kept, for a client that has to tell someone.
func (s *Store) SecretLocation() string { return s.secrets.describe() }

func (s *Store) recordPath() string { return filepath.Join(s.dir, recordFileName) }
func (s *Store) devicePath() string { return filepath.Join(s.dir, deviceFileName) }

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

func (s *Store) writeRecord(record Record) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the credential record: %w", err)
	}
	data = append(data, '\n')

	if err := writeFileAtomically(s.recordPath(), data); err != nil {
		return fmt.Errorf("saving the credential record: %w", err)
	}
	return nil
}

// writeFileAtomically replaces a file in the store's directory, owner-only, with no observable half-state.
//
// Temp file plus rename, as every writer of shared client state does (architecture.md §3): a half-written
// file here is a daemon that cannot start, and a crash mid-write is exactly when someone is least able to
// diagnose one. The temp file is created 0600 rather than tightened afterwards, so its contents are never
// briefly world-readable — this writes a refresh token as well as the two plain files beside it.
//
// The temp file is created in the destination's own directory because a rename is only atomic within one
// filesystem, and it is removed on every path out; once the rename has succeeded that removal is a no-op.
func writeFileAtomically(path string, data []byte) error {
	dir, name := filepath.Split(path)

	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restricting %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	// Flushed before the rename, so a crash between the two cannot leave the new name pointing at an empty
	// file — which is the one outcome a rename is supposed to rule out.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flushing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
