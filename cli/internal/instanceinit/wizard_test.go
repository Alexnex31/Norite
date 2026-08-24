package instanceinit

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scripted builds a prompter that answers questions from a canned script, as if typed.
func scripted(answers []string, secrets ...string) (*prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	remaining := secrets
	p := newPrompter(strings.NewReader(strings.Join(answers, "\n")+"\n"), out, promptInteractive, func() (string, error) {
		if len(remaining) == 0 {
			return "", io.EOF
		}
		next := remaining[0]
		remaining = remaining[1:]
		return next, nil
	})
	return p, out
}

// silent builds a non-interactive prompter, the --non-interactive case.
func silent() (*prompter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	p := newPrompter(strings.NewReader(""), out, promptScripted, func() (string, error) { return "", io.EOF })
	return p, out
}

func readBack(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal(body, &parsed))
	return parsed
}

// The unattended provisioning path: every answer arrives as a flag, nothing is asked, and the result is a
// complete file.
func TestNonInteractiveRunWritesAConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	p, out := silent()

	err := run(Options{
		NonInteractive: true,
		Output:         path,
		DBHost:         "db.internal",
		DBPort:         "6432",
		DBName:         "norite",
		DBUser:         "norite",
		DBPassword:     "hunter2",
		DBSSLMode:      "require",
		Registration:   registrationInvite,
	}, p)
	require.NoError(t, err)

	parsed := readBack(t, path)
	database, ok := parsed["database"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "postgres://norite:hunter2@db.internal:6432/norite?sslmode=require", database["url"])

	registration, ok := parsed["registration"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, registrationInvite, registration["mode"])

	assert.NotContains(t, out.String(), "hunter2", "the summary must never echo the password")
}

// Quick-start asks only what has no safe default, and says so rather than leaving the rest invisible.
func TestQuickStartAsksOnlyTheEssentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	// host, port, name, user, sslmode, registration — no storage, ACME, env, or listen-address questions.
	p, out := scripted([]string{"", "", "", "", "", ""}, "s3cret-pw")

	require.NoError(t, run(Options{Output: path}, p))

	transcript := out.String()
	assert.Contains(t, transcript, "Postgres host")
	assert.Contains(t, transcript, "Registration policy")
	assert.NotContains(t, transcript, "Storage backend", "storage has a safe default, so quick-start skips it")
	assert.NotContains(t, transcript, "Obtain certificates", "ACME has a safe default, so quick-start skips it")
	assert.Contains(t, transcript, "--full", "the summary must say how to be asked about the rest")

	parsed := readBack(t, path)
	storage, ok := parsed["storage"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, storageLocal, storage["backend"])
}

func TestFullRunAsksAboutEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	answers := []string{
		"development", "127.0.0.1:9000", "", // env, listen addr, public base URL (left blank)
		"localhost", "5432", "norite", "norite", "disable", // database
		"local", "/srv/attachments", // storage
		"no",     // smtp
		"no",     // acme
		"invite", // registration
	}
	p, out := scripted(answers, "pw")

	require.NoError(t, run(Options{Full: true, Output: path}, p))

	transcript := out.String()
	assert.Contains(t, transcript, "Storage backend")
	assert.Contains(t, transcript, "Obtain certificates")
	assert.NotContains(t, transcript, "--full", "a full run has nothing left to offer")

	parsed := readBack(t, path)
	assert.Equal(t, envDevelopment, parsed["env"])
	assert.Equal(t, "127.0.0.1:9000", parsed["http"].(map[string]any)["listen_addr"])
	assert.Equal(t, "/srv/attachments", parsed["storage"].(map[string]any)["local_path"])
	assert.Equal(t, false, parsed["acme"].(map[string]any)["enabled"])
}

// The relay settings, and the public origin reset links are built from. Answering yes must collect all of
// them, because the backend refuses to start with SMTP on and any of them missing.
func TestFullRunCollectsSMTPSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	answers := []string{
		"production", ":8080",
		// Answered in the Instance section now, not alongside SMTP. gatherSMTP asks only if it is still
		// empty, so a --full run is never asked the same question twice.
		"https://chat.example.com", // public base url
		"localhost", "5432", "norite", "norite", "disable",
		"local", "/srv/attachments",
		"yes",                     // send email
		"smtp.example.com", "587", // host, port
		"norite@example.com",   // username -> triggers the password prompt
		"starttls",             // encryption
		"no-reply@example.com", // from address
		"Norite",               // from name
		"no",                   // acme
		"open",
	}
	p, out := scripted(answers, "db-pw", "relay-pw")

	require.NoError(t, run(Options{Full: true, Output: path}, p))

	parsed := readBack(t, path)
	smtp, ok := parsed["smtp"].(map[string]any)
	require.True(t, ok, "an SMTP instance must get an [smtp] section")
	assert.Equal(t, true, smtp["enabled"])
	assert.Equal(t, "smtp.example.com", smtp["host"])
	assert.Equal(t, int64(587), smtp["port"])
	assert.Equal(t, "norite@example.com", smtp["username"])
	assert.Equal(t, "relay-pw", smtp["password"])
	assert.Equal(t, "starttls", smtp["encryption"])
	assert.Equal(t, "no-reply@example.com", smtp["from_address"])

	// public_base_url lives under [http] but is only required because SMTP is on — the cross-section
	// dependency the backend enforces, and the one a wizard is most likely to forget to collect.
	assert.Equal(t, "https://chat.example.com", parsed["http"].(map[string]any)["public_base_url"])

	assert.NotContains(t, out.String(), "relay-pw", "the summary must never echo the relay password")
}

// A quick-start run that asks for SMTP must still be asked its companion questions. Gating them on --full
// alone would write a file with smtp.enabled = true and no host, which the backend refuses to start on —
// the same trap the storage and ACME branches above already avoid.
func TestQuickStartWithSMTPStillCollectsItsCompanions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	yes := true

	p, _ := silent()
	err := run(Options{
		NonInteractive: true,
		Output:         path,
		DBPassword:     "pw",
		SMTP:           &yes,
		// Deliberately incomplete: no host, no from address, no public base URL.
	}, p)

	require.Error(t, err, "--smtp without its companions must fail rather than write an unstartable file")
	assert.NoFileExists(t, path)
}

func TestQuickStartWithSMTPFlagsWritesTheSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	yes := true

	p, _ := silent()
	require.NoError(t, run(Options{
		NonInteractive:  true,
		Output:          path,
		DBPassword:      "pw",
		SMTP:            &yes,
		SMTPHost:        "relay.example.com",
		SMTPFromAddress: "no-reply@example.com",
		PublicBaseURL:   "https://chat.example.com",
	}, p))

	parsed := readBack(t, path)
	smtp := parsed["smtp"].(map[string]any)
	assert.Equal(t, true, smtp["enabled"], "--smtp must be honored without --full")
	assert.Equal(t, "relay.example.com", smtp["host"])
	assert.Equal(t, "https://chat.example.com", parsed["http"].(map[string]any)["public_base_url"])
}

// Declining leaves the section off entirely rather than half-written. An instance with SMTP off is a
// working instance, not a misconfigured one.
func TestDecliningSMTPLeavesItDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	answers := []string{
		"production", ":8080",
		"", // public base url, left blank
		"localhost", "5432", "norite", "norite", "disable",
		"local", "/srv/attachments",
		"no", // smtp
		"no", // acme
		"open",
	}
	p, _ := scripted(answers, "db-pw")

	require.NoError(t, run(Options{Full: true, Output: path}, p))

	parsed := readBack(t, path)
	smtp, ok := parsed["smtp"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, smtp["enabled"])
	assert.NotContains(t, smtp, "host", "a disabled relay must not leave half a configuration behind")
	assert.NotContains(t, parsed["http"], "public_base_url")
}

// Values that only exist under S3 storage must be collected when it is chosen, and the resulting file has
// to carry all of them or the backend will refuse to start.
func TestFullRunCollectsS3Settings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	answers := []string{
		"production", ":8080",
		"", // public base url, left blank
		"localhost", "5432", "norite", "norite", "disable",
		"s3", "https://minio.example.com", "eu-west-1", "attachments", "minio-user", "yes",
		"no", // smtp
		"no", // acme
		"open",
	}
	p, _ := scripted(answers, "db-pw", "s3-secret")

	require.NoError(t, run(Options{Full: true, Output: path}, p))

	s3, ok := readBack(t, path)["storage"].(map[string]any)["s3"].(map[string]any)
	require.True(t, ok, "an S3 instance must get an [storage.s3] section")
	assert.Equal(t, "https://minio.example.com", s3["endpoint"])
	assert.Equal(t, "eu-west-1", s3["region"])
	assert.Equal(t, "attachments", s3["bucket"])
	assert.Equal(t, "minio-user", s3["access_key_id"])
	assert.Equal(t, "s3-secret", s3["secret_access_key"])
	assert.Equal(t, true, s3["force_path_style"])
}

// Running without a terminal and without --non-interactive must fail with something actionable rather
// than hanging or silently accepting every default, empty password included.
func TestMissingTerminalIsAnActionableError(t *testing.T) {
	out := &bytes.Buffer{}
	// A terminal that closed mid-run: questions are still allowed, but the answers never arrive.
	p := newPrompter(strings.NewReader(""), out, promptInteractive, func() (string, error) { return "", io.EOF })

	err := run(Options{Output: filepath.Join(t.TempDir(), "instance.toml")}, p)
	require.ErrorIs(t, err, ErrNotATerminal)
}

// Non-interactive runs cannot prompt for the values S3 requires, so a missing one has to be named rather
// than written out empty and rejected later by the backend.
func TestNonInteractiveMissingRequiredValueIsNamed(t *testing.T) {
	p, _ := silent()

	err := run(Options{
		NonInteractive: true,
		Full:           true,
		Output:         filepath.Join(t.TempDir(), "instance.toml"),
		Storage:        storageS3,
		// Supplied so the run reaches the S3 questions this test is about. Without it the database
		// password — also required, and asked earlier — is what fails first.
		DBPassword: "pw",
	}, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bucket name")
	assert.Contains(t, err.Error(), "non-interactively")
}

// The S3 secret access key is the one required S3 value read without echo, so it goes through askSecret
// rather than askRequiredOr. askSecret used to return "" unconditionally when it was not prompting, which
// ignored its own allowEmpty=false and wrote the secret out empty — the backend then refused to start on
// `required_if=StorageBackend s3` with nothing pointing back at the wizard.
func TestNonInteractiveMissingS3SecretIsNamed(t *testing.T) {
	p, _ := silent()

	err := run(Options{
		NonInteractive: true,
		Full:           true,
		Output:         filepath.Join(t.TempDir(), "instance.toml"),
		Storage:        storageS3,
		DBPassword:     "pw",
		S3Bucket:       "bucket",
		S3Region:       "us-east-1",
		S3AccessKeyID:  "AKIAEXAMPLE",
	}, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Secret access key")
	assert.Contains(t, err.Error(), "non-interactively")
}

// A non-interactive run with no database password must name it rather than write a passwordless DSN.
func TestNonInteractiveMissingDatabasePasswordIsNamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	p, _ := silent()

	err := run(Options{NonInteractive: true, Output: path}, p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Database password")
	assert.NoFileExists(t, path, "nothing may be written when a required credential was never given")
}

// A password with URL metacharacters is exactly why the DSN is assembled rather than typed: unescaped, it
// would silently produce a connection string pointing somewhere else entirely.
func TestDSNEncodesAwkwardPasswords(t *testing.T) {
	dsn := buildDSN("localhost", 5432, "norite", "norite", "p@ss:w/rd?#", "disable")

	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	password, hasPassword := parsed.User.Password()
	require.True(t, hasPassword)
	assert.Equal(t, "p@ss:w/rd?#", password, "the password must survive the round trip intact")
	assert.Equal(t, "localhost:5432", parsed.Host)
	assert.Equal(t, "/norite", parsed.Path)
	assert.Equal(t, "disable", parsed.Query().Get("sslmode"))
}

func TestDSNOmitsPasswordWhenThereIsNone(t *testing.T) {
	dsn := buildDSN("localhost", 5432, "norite", "norite", "", "disable")
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	_, hasPassword := parsed.User.Password()
	assert.False(t, hasPassword, "an empty password must not become an empty-string password")
}

// Unencrypted is fine over loopback and pointless to demand there; anything across a network should be
// encrypted unless the operator says otherwise.
func TestDefaultSSLModeFollowsWhereTheDatabaseIs(t *testing.T) {
	for host, want := range map[string]string{
		"localhost":      "disable",
		"127.0.0.1":      "disable",
		"::1":            "disable",
		"db.internal":    "require",
		"10.0.0.5":       "require",
		"db.example.com": "require",
	} {
		assert.Equal(t, want, defaultSSLMode(host), "host %q", host)
	}
}

func TestRedactDSNNeverLeaksThePassword(t *testing.T) {
	dsn := buildDSN("db.internal", 5432, "norite", "norite", "hunter2", "require")

	redacted := redactDSN(dsn)
	assert.NotContains(t, redacted, "hunter2")
	assert.Contains(t, redacted, "db.internal", "the rest must stay legible enough to confirm")

	// A string that doesn't parse must fail closed rather than being echoed verbatim.
	assert.Equal(t, "(configured)", redactDSN("://not a url\x7f"))
}

// A port that came from a flag cannot be re-asked, so a bad one has to error rather than loop forever.
func TestBadPortFlagFailsInsteadOfLooping(t *testing.T) {
	p, _ := scripted([]string{""})

	_, err := p.askPort("Postgres port", "not-a-port", 5432)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "65535")
}

// A choice supplied by flag is validated against the same option list a prompt would enforce.
func TestInvalidChoiceFromFlagIsRejected(t *testing.T) {
	p, _ := silent()

	_, err := p.askChoice("Registration policy", []string{registrationOpen, registrationInvite}, "everyone", registrationOpen)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "everyone")
}

// Flags answer questions rather than merely pre-filling them: a supplied value is used without asking.
func TestFlagsSkipTheirPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	// Only the SSL mode and registration questions remain unanswered by flags.
	p, out := scripted([]string{"", ""}, "pw")

	require.NoError(t, run(Options{
		Output: path,
		DBHost: "db.internal",
		DBPort: "5433",
		DBName: "chat",
		DBUser: "chatuser",
	}, p))

	transcript := out.String()
	assert.NotContains(t, transcript, "Postgres host")
	assert.NotContains(t, transcript, "Database name")

	database := readBack(t, path)["database"].(map[string]any)
	assert.Contains(t, database["url"], "db.internal:5433")
	assert.Contains(t, database["url"], "chatuser")
}

// Selecting a non-default backend by flag pulls in the values that backend requires, even in quick-start.
// Without this, `--storage s3` alone would happily write a file the backend refuses to load.
func TestQuickStartWithS3FlagStillCollectsS3Settings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	// database (5 answers, no password prompt because it is supplied), then the S3 questions.
	answers := []string{"", "", "", "", "", "https://minio.example.com", "eu-west-1", "attachments", "key", "yes", ""}
	p, _ := scripted(answers, "s3-secret")

	require.NoError(t, run(Options{Output: path, DBPassword: "pw", Storage: storageS3}, p))

	s3, ok := readBack(t, path)["storage"].(map[string]any)["s3"].(map[string]any)
	require.True(t, ok, "choosing S3 must produce a complete [storage.s3] section")
	assert.Equal(t, "attachments", s3["bucket"])
	assert.Equal(t, "s3-secret", s3["secret_access_key"])
}

// The same holds for the flag that turns automatic HTTPS on: it cannot proceed without a domain and email.
func TestQuickStartWithACMEFlagStillCollectsDomain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	enabled := true
	p, _ := silent()

	err := run(Options{
		NonInteractive: true,
		Output:         path,
		DBPassword:     "pw",
		ACME:           &enabled,
	}, p)
	require.Error(t, err, "enabling ACME without a domain must fail rather than write an unusable file")
	assert.Contains(t, err.Error(), "Public hostname")
}

// Turning a flag off explicitly must not drag its questions in.
func TestQuickStartWithACMEDisabledAsksNothingExtra(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	disabled := false
	p, out := scripted([]string{"", "", "", "", "", ""}, "pw")

	require.NoError(t, run(Options{Output: path, ACME: &disabled}, p))
	assert.NotContains(t, out.String(), "Public hostname")
	assert.Equal(t, false, readBack(t, path)["acme"].(map[string]any)["enabled"])
}

// The case that actually shipped broken: stdin is not a terminal and --non-interactive was NOT passed.
//
// This used to fall through to defaults and write a config with an empty database password, exiting 0 —
// the exact outcome ErrNotATerminal exists to prevent. The earlier test covered a different path (a
// terminal whose input ran out), so it passed while this went unnoticed.
func TestPipedStdinWithoutNonInteractiveIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	out := &bytes.Buffer{}
	// promptNoTerminal is what newTerminalPrompter produces for a pipe, a fifo, or `docker exec` without -t.
	p := newPrompter(strings.NewReader("some piped input\n"), out, promptNoTerminal,
		func() (string, error) { return "", io.EOF })

	err := run(Options{Output: path}, p)
	require.ErrorIs(t, err, ErrNotATerminal)
	assert.NoFileExists(t, path, "nothing may be written when the answers were never actually given")
}

// The same hole, one level deeper, and the reason the test above did not catch it.
//
// That test passes because the *first* question has no preset, so the run fails before ever reaching the
// password. Supply every other db-* flag and the wizard walks straight past them to the one question with
// no default — which was guarded by `password == "" && p.asks()`, so on a pipe it was skipped rather than
// asked, and the run wrote `postgres://norite@host/db` and exited 0.
func TestPipedStdinIsRefusedEvenWhenEveryOtherDBFlagIsSupplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	out := &bytes.Buffer{}
	p := newPrompter(strings.NewReader(""), out, promptNoTerminal,
		func() (string, error) { return "", io.EOF })

	err := run(Options{
		Output:       path,
		DBHost:       "db.example.com",
		DBPort:       "5432",
		DBName:       "norite",
		DBUser:       "norite",
		DBSSLMode:    "require",
		Registration: "open",
	}, p)

	require.ErrorIs(t, err, ErrNotATerminal)
	assert.NoFileExists(t, path, "a passwordless DSN must never be written on the way to exiting 0")
}

// ...but with --non-interactive the same pipe is fine: the operator stated that defaults are intended.
func TestPipedStdinIsFineWhenNonInteractiveIsRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	p, _ := silent()

	require.NoError(t, run(Options{NonInteractive: true, Output: path, DBPassword: "pw"}, p))
	assert.FileExists(t, path)
}

// The wizard is where somebody finds out what has to happen next, and the order is not guessable:
// `norite instance bootstrap` needs a running server to talk to and a migrated schema to write into, so it
// cannot come before either. Pinned because it is prose — nothing else fails if it goes stale or if the
// command it names is renamed.
func TestTheSummarySaysHowToFinishSettingUpTheInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	p, out := scripted([]string{"", "", "", "", "", ""}, "s3cret-pw")

	require.NoError(t, run(Options{Output: path}, p))

	transcript := out.String()
	assert.Contains(t, transcript, "Start the backend")
	assert.Contains(t, transcript, "norite instance bootstrap",
		"an instance with no administrator is not finished, and this is where that is said")

	// The order, not merely the presence of both: starting the backend comes first because bootstrap has
	// nothing to talk to otherwise.
	assert.Less(t, strings.Index(transcript, "Start the backend"),
		strings.Index(transcript, "norite instance bootstrap"))
}

// A config file somewhere other than the conventional location has to be pointed at twice, and the second
// is the one that is easy to miss: bootstrap reads this file too — the signing key in it is what
// authorizes the account it creates — and it looks in the same default place the backend does.
func TestANonDefaultConfigPathIsCalledOutForBothSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "elsewhere.toml")
	p, out := scripted([]string{"", "", "", "", "", ""}, "s3cret-pw")

	require.NoError(t, run(Options{Output: path}, p))

	transcript := out.String()
	assert.Contains(t, transcript, "NORITE_CONFIG_FILE="+path, "the backend needs pointing at it")
	assert.Contains(t, transcript, "norite instance bootstrap --config "+path, "and so does bootstrap")
}

// The bug this closes: --public-base-url was only ever read inside the SMTP branch, so passing it without
// also enabling a mail relay was accepted, silently dropped, and never written. Invisible until something
// downstream needed the value — which is what happened when `norite instance bootstrap` arrived and had
// nowhere to send its request.
//
// Four things build links from this value and only one of them is SMTP: reset mail (M5), OAuth callbacks
// (M6), the device verification URI (M9), and bootstrap (M10).
func TestThePublicBaseURLIsWrittenWithoutSMTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	p, _ := silent()

	require.NoError(t, run(Options{
		NonInteractive: true,
		Output:         path,
		DBPassword:     "pw",
		PublicBaseURL:  "https://chat.example.com",
	}, p))

	http, ok := readBack(t, path)["http"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://chat.example.com", http["public_base_url"],
		"a flag the command accepts must not be silently discarded")
}

// A --full run is offered the question even with no relay, which it was not before: the operator who
// wants to be asked about everything is exactly the one who should be asked about this.
func TestFullRunAsksForThePublicBaseURLWithoutSMTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	answers := []string{
		"production", ":8080",
		"https://chat.example.com", // public base url
		"localhost", "5432", "norite", "norite", "disable",
		"local", "/srv/attachments",
		"no", // smtp
		"no", // acme
		"open",
	}
	p, out := scripted(answers, "db-pw")

	require.NoError(t, run(Options{Full: true, Output: path}, p))

	assert.Equal(t, 1, strings.Count(out.String(), "Public base URL"),
		"asked once, and not again by the SMTP section")

	http, ok := readBack(t, path)["http"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://chat.example.com", http["public_base_url"])
}
