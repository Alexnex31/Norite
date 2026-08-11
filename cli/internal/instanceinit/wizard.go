package instanceinit

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Options is everything the command line can supply.
//
// Each field doubles as the answer to its question: a value present here is used as-is and its prompt is
// skipped entirely, which is what makes one code path serve the interactive wizard, a partially-scripted
// run, and a fully non-interactive one. Tri-state flags are pointers so "not passed" stays distinct from
// "passed as false".
type Options struct {
	// Full asks about every setting rather than only those with no safe default.
	Full bool
	// NonInteractive suppresses all prompting: flags and defaults only.
	NonInteractive bool
	// Output is where the config file is written. Empty means the platform's conventional location.
	Output string
	// Force allows replacing an existing file.
	Force bool

	Env        string
	ListenAddr string

	DatabaseURL string
	DBHost      string
	DBPort      string
	DBName      string
	DBUser      string
	DBPassword  string
	DBSSLMode   string

	Storage          string
	StorageLocalPath string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  *bool

	ACME       *bool
	ACMEDomain string
	ACMEEmail  string

	Registration string
}

// Run executes the wizard against the real terminal: gather answers, write the file, print what happened.
func Run(opts Options, stdin *os.File, stdout io.Writer) error {
	return run(opts, newTerminalPrompter(stdin, stdout, !opts.NonInteractive))
}

// run is Run with the input source already decided, so tests can script the answers.
func run(opts Options, p *prompter) error {
	doc, err := gather(p, opts)
	if err != nil {
		return err
	}

	path := opts.Output
	if path == "" {
		path = DefaultConfigPath()
	}
	if err := doc.Write(path, opts.Force); err != nil {
		return err
	}

	printSummary(p, doc, path, opts.Full)
	return nil
}

// gather turns flags plus answers into a complete Document.
//
// Quick-start asks only what has no safe default — the database connection, and the registration policy,
// where "open" is conventional but is a real decision for a private instance and shouldn't be made
// silently on the operator's behalf. Everything else takes its default and is reported in the summary, so
// nothing is chosen invisibly. --full asks about all of it.
func gather(p *prompter, opts Options) (Document, error) {
	doc := Document{
		Env:              orDefault(opts.Env, envProduction),
		ListenAddr:       orDefault(opts.ListenAddr, ":8080"),
		StorageBackend:   orDefault(opts.Storage, storageLocal),
		StorageLocalPath: orDefault(opts.StorageLocalPath, DefaultStoragePath()),
		RegistrationMode: orDefault(opts.Registration, registrationOpen),
	}

	// Generated, never prompted for. Asking an operator to invent a signing key produces a memorable one,
	// and a memorable HS256 key is a forgeable one — the whole security of an access token rests on this
	// value's entropy. Generating it here means no instance can be set up with a weak key by accident.
	secret, err := generateSigningKey()
	if err != nil {
		return Document{}, err
	}
	doc.JWTSecret = secret

	if p.asks() {
		p.println("Setting up a Norite instance.")
		p.println("Press Enter to accept the value in [brackets].")
	}

	if opts.Full {
		p.section("Instance")
		var err error
		doc.Env, err = p.askChoice("Environment", []string{envDevelopment, envProduction}, opts.Env, envProduction)
		if err != nil {
			return Document{}, err
		}
		p.note("Address the backend listens on. Use 127.0.0.1:8080 to accept only local connections.")
		doc.ListenAddr, err = p.ask("Listen address", opts.ListenAddr, ":8080")
		if err != nil {
			return Document{}, err
		}
	}

	dsn, err := gatherDatabase(p, opts)
	if err != nil {
		return Document{}, err
	}
	doc.DatabaseURL = dsn

	// Quick-start normally leaves storage and HTTPS on their defaults, but a flag that selects a
	// non-default backend drags in companion values the backend requires — an S3 bucket, an ACME domain.
	// Skipping their questions there would write a file that cannot start, so asking is driven by what was
	// actually selected rather than by --full alone.
	if opts.Full || opts.Storage == storageS3 {
		if err := gatherStorage(p, opts, &doc); err != nil {
			return Document{}, err
		}
	}
	if opts.Full || (opts.ACME != nil && *opts.ACME) {
		if err := gatherACME(p, opts, &doc); err != nil {
			return Document{}, err
		}
	}

	p.section("Registration")
	p.note("%q lets anyone create an account on this instance.", registrationOpen)
	p.note("%q requires an invite code, which is what a private instance wants.", registrationInvite)
	doc.RegistrationMode, err = p.askChoice("Registration policy",
		[]string{registrationOpen, registrationInvite}, opts.Registration, registrationOpen)
	if err != nil {
		return Document{}, err
	}

	return doc, nil
}

// gatherDatabase produces the Postgres connection string.
//
// The parts are asked separately and assembled with net/url rather than asking for a whole DSN, because
// a password containing @, /, or : — which a generated password very often does — has to be
// percent-encoded to survive in a URL. Making the operator get that right by hand is a trap; url.URL
// encodes it correctly by construction.
func gatherDatabase(p *prompter, opts Options) (string, error) {
	if opts.DatabaseURL != "" {
		return opts.DatabaseURL, nil
	}

	p.section("Database")
	p.note("Norite needs an existing, empty Postgres database. It creates its own schema on first start.")

	host, err := p.ask("Postgres host", opts.DBHost, "localhost")
	if err != nil {
		return "", err
	}
	port, err := p.askPort("Postgres port", opts.DBPort, 5432)
	if err != nil {
		return "", err
	}
	name, err := p.ask("Database name", opts.DBName, "norite")
	if err != nil {
		return "", err
	}
	user, err := p.ask("Database user", opts.DBUser, "norite")
	if err != nil {
		return "", err
	}

	password := opts.DBPassword
	if password == "" && p.asks() {
		// The one answer with no default and no safe guess. Read without echo — this ends up in a file
		// and should not also end up in the scrollback of a shared terminal.
		password, err = p.askSecret("Database password", "", false)
		if err != nil {
			return "", err
		}
	}

	sslDefault := defaultSSLMode(host)
	p.note("sslmode: %q is right over a local socket or a private container network; use %q or "+
		"verify-full for anything crossing a real network.", "disable", "require")
	sslMode, err := p.askChoice("SSL mode",
		[]string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"},
		opts.DBSSLMode, sslDefault)
	if err != nil {
		return "", err
	}

	return buildDSN(host, port, name, user, password, sslMode), nil
}

func gatherStorage(p *prompter, opts Options, doc *Document) error {
	p.section("Attachment storage")
	p.note("%q keeps uploads on this machine's disk. %q uses AWS S3, MinIO, or any compatible service.",
		storageLocal, storageS3)

	backend, err := p.askChoice("Storage backend", []string{storageLocal, storageS3}, opts.Storage, storageLocal)
	if err != nil {
		return err
	}
	doc.StorageBackend = backend

	if backend == storageLocal {
		doc.StorageLocalPath, err = p.ask("Attachment directory", opts.StorageLocalPath, DefaultStoragePath())
		return err
	}

	p.note("Leave the endpoint empty for AWS S3 itself; set it for MinIO and other compatible services.")
	if doc.S3Endpoint, err = p.ask("S3 endpoint URL", opts.S3Endpoint, ""); err != nil {
		return err
	}
	if doc.S3Region, err = p.ask("S3 region", opts.S3Region, "us-east-1"); err != nil {
		return err
	}
	if doc.S3Bucket, err = p.askRequiredOr("Bucket name", opts.S3Bucket); err != nil {
		return err
	}
	if doc.S3AccessKeyID, err = p.askRequiredOr("Access key ID", opts.S3AccessKeyID); err != nil {
		return err
	}
	if doc.S3SecretAccessKey, err = p.askSecret("Secret access key", opts.S3SecretAccessKey, false); err != nil {
		return err
	}

	p.note("Path-style addressing (endpoint/bucket) is what MinIO and most self-hosted services need.")
	doc.S3ForcePathStyle, err = p.askBool("Use path-style bucket addressing", opts.S3ForcePathStyle, doc.S3Endpoint != "")
	return err
}

func gatherACME(p *prompter, opts Options, doc *Document) error {
	p.section("HTTPS")
	p.note("Norite can obtain and renew its own certificate from Let's Encrypt.")
	p.note("Say no for a LAN-only instance, or when a reverse proxy already terminates TLS in front of it.")

	enabled, err := p.askBool("Obtain certificates automatically", opts.ACME, false)
	if err != nil {
		return err
	}
	doc.ACMEEnabled = enabled
	if !enabled {
		return nil
	}

	p.note("The certificate authority needs a public hostname that resolves to this machine.")
	if doc.ACMEDomain, err = p.askRequiredOr("Public hostname", opts.ACMEDomain); err != nil {
		return err
	}
	doc.ACMEEmail, err = p.askRequiredOr("Contact email for expiry notices", opts.ACMEEmail)
	return err
}

// askRequiredOr takes a flag value when present, and otherwise insists on an answer.
func (p *prompter) askRequiredOr(question, preset string) (string, error) {
	if preset != "" {
		return preset, nil
	}
	if !p.asks() {
		if err := p.unattended(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s is required; pass it as a flag when running non-interactively", question)
	}
	return p.askRequired(question)
}

// buildDSN assembles a Postgres connection URL with every component correctly escaped.
func buildDSN(host string, port int, name, user, password, sslMode string) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + name,
	}
	if password != "" {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	u.RawQuery = url.Values{"sslmode": []string{sslMode}}.Encode()
	return u.String()
}

// defaultSSLMode picks a default that is safe for where the database actually is: unencrypted is fine
// over loopback and pointless to demand there, but anything reached across a network should be encrypted
// unless the operator deliberately says otherwise.
func defaultSSLMode(host string) string {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "":
		return "disable"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "disable"
	}
	return "require"
}

// DefaultConfigPath is where the backend looks for its config file when nothing points it elsewhere. It
// must stay in step with backend/internal/config's own discovery, which is asserted by the contract test.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "Norite", "instance.toml")
		}
		return "instance.toml"
	}
	return "/etc/norite/instance.toml"
}

// DefaultStoragePath is the backend's own default attachment directory.
func DefaultStoragePath() string {
	if runtime.GOOS == "windows" {
		if programData := os.Getenv("ProgramData"); programData != "" {
			return filepath.Join(programData, "Norite", "attachments")
		}
		return "attachments"
	}
	return "/var/lib/norite/attachments"
}

// printSummary reports what was written and what to do next.
//
// It deliberately never prints the connection string: it carries the password the operator just typed,
// and the whole point of reading that without echo is defeated by printing it back out (CLAUDE.md rule 8).
func printSummary(p *prompter, doc Document, path string, full bool) {
	p.printf("\nWrote %s (permissions %#o — it contains credentials).\n\n", path, uint32(FileMode))

	p.println("  environment:  " + doc.Env)
	p.println("  listening on: " + doc.ListenAddr)
	p.println("  database:     " + redactDSN(doc.DatabaseURL))
	if doc.UsesS3() {
		p.printf("  attachments:  s3 bucket %q\n", doc.S3Bucket)
	} else {
		p.println("  attachments:  " + doc.StorageLocalPath)
	}
	if doc.ACMEEnabled {
		p.printf("  https:        automatic, for %s\n", doc.ACMEDomain)
	} else {
		p.println("  https:        off (terminate TLS in front of this instance)")
	}
	p.println("  registration: " + doc.RegistrationMode)
	// The value itself is never printed (CLAUDE.md rule 8). Saying it exists matters, though: an operator
	// who does not know the file now holds a credential may copy it somewhere it should not go.
	p.println("  signing key:  generated (" + strconv.Itoa(signingKeyBytes*8) + "-bit, stored in this file)")

	if !full {
		p.println("\nEverything not asked about took its default. Re-run with --full to be asked " +
			"about all of them,")
		p.println("or edit the file directly — it documents every setting it writes.")
	}

	p.println("\nStart the backend, and it will migrate its own schema on first run.")
	if path != DefaultConfigPath() {
		p.printf("Point it at this file with -config %s (or NORITE_CONFIG_FILE=%s).\n", path, path)
	}
}

// redactDSN renders a connection string with its password removed, for display.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// Never risk echoing a credential from a string that didn't parse the way we expected.
		return "(configured)"
	}
	// Redacted replaces the password with a fixed placeholder, leaving the rest legible — which is
	// exactly what an operator confirming "did it pick up the right host and database?" needs.
	return u.Redacted()
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// signingKeyBytes is the size of the generated HS256 signing key.
//
// 32 bytes matches SHA-256's output size, which is the point past which a longer HMAC key adds nothing and
// below which it starts to matter. The backend enforces the same floor at startup (config.JWTSecret).
const signingKeyBytes = 32

// generateSigningKey returns a fresh base64 signing key.
func generateSigningKey() (string, error) {
	buf := make([]byte, signingKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means there is no usable entropy source. Writing a predictable signing key
		// would be far worse than refusing to finish setup, so this is fatal rather than fallback-worthy.
		return "", fmt.Errorf("generating the token signing key: %w", err)
	}
	// Standard base64 rather than raw-URL: this value only ever appears inside a quoted TOML string, and
	// the padded alphabet is what `openssl rand -base64 32` produces, so a hand-replaced key looks the same.
	return base64.StdEncoding.EncodeToString(buf), nil
}
