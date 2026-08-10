// Package config holds the backend's typed, environment-bound configuration.
//
// Everything the process needs to boot is read once, at startup, into a single validated Config value —
// no scattered os.Getenv calls deeper in the tree. Invalid configuration fails the process immediately
// with an actionable message rather than surfacing as a confusing runtime error later.
//
// The instance config *file* written by `norite instance init` (Milestone M2) layers on top of this later;
// environment variables stay the lowest, always-available layer, which is what the docker-compose and
// Kubernetes deployment shapes both drive.
package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

// Environment names the deployment shape. It only changes cross-cutting defaults (log format, error
// verbosity) — never business behavior.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// Config is the fully-resolved backend configuration.
//
// Field tags: `env` names the environment variable, `validate` is enforced by Load before the value is
// handed to any caller.
type Config struct {
	// SourcePath is the instance config file that contributed to this Config, or "" when it was built
	// from environment variables and defaults alone. Diagnostic metadata, not a setting — an operator
	// debugging "why is this instance behaving that way" needs to know which file it actually read.
	SourcePath string `validate:"-"`

	// Env selects development- vs. production-flavored defaults.
	Env Environment `validate:"required,oneof=development production"`

	// ListenAddr is the HTTP listen address, e.g. ":8080".
	ListenAddr string `validate:"required,hostname_port"`

	// DatabaseURL is the Postgres connection string (postgres://user:pass@host:port/db?...).
	//
	// Never log this value — it carries the database password. See CLAUDE.md rule 8.
	DatabaseURL string `validate:"required,startswith=postgres"`

	// DBMaxConns / DBMinConns size the pgx pool. Deliberately small per backend replica — see
	// docs/architecture.md §11 "Database connection management": Postgres treats connections as
	// relatively heavy processes with a low default limit (often 100), and Norite's daemon-per-OS-user
	// model means real connection demand grows with both instance popularity and how many devices each
	// user runs a daemon on. Self-hosters who outgrow a small direct pool are pointed at PgBouncer
	// rather than the backend holding a large direct pool itself.
	DBMaxConns int32 `validate:"required,gte=1,lte=100"`
	DBMinConns int32 `validate:"gte=0,ltefield=DBMaxConns"`

	// DBConnectTimeout bounds how long startup waits for the very first successful connection.
	DBConnectTimeout time.Duration `validate:"required,gt=0"`

	// MigrateLockTimeout bounds how long startup waits for the migration advisory lock. A second
	// process mid-migration is the normal reason to wait; a stuck one is the reason to give up.
	MigrateLockTimeout time.Duration `validate:"required,gt=0"`

	// LogLevel is a zerolog level name (trace/debug/info/warn/error/fatal/panic).
	LogLevel string `validate:"required,oneof=trace debug info warn error fatal panic"`

	// LogFormat is "json" (machine-readable, the production default) or "console" (human-readable,
	// the development default).
	LogFormat string `validate:"required,oneof=json console"`

	// RateLimit is the base REST rate limit in ulule/limiter format, "<limit>-<period>" where period is
	// one of S, M, H, D — e.g. "600-M" is 600 requests per minute.
	RateLimit string `validate:"required"`

	// TrustProxyHeaders decides whether forwarded headers are honored: X-Forwarded-For for the client
	// address, X-Forwarded-Proto for the HSTS decision.
	//
	// Default false, and that default is load-bearing: the client IP is the rate limiter's grouping key
	// (docs/architecture.md §11), so honoring a client-settable header when the process is directly
	// internet-facing would let any caller forge a fresh identity per request and walk straight through
	// every IP-based limit. Turn it on only when the process genuinely sits behind a proxy that overwrites
	// those headers (the flagship's Kubernetes Ingress, §12; a self-hoster's own reverse proxy).
	//
	// X-Real-IP and True-Client-IP are never consulted, even when this is on: they carry no positional
	// information, so a value set by a trusted proxy is indistinguishable from one a client sent (see
	// httpx.RealIP). A proxy configured to set only X-Real-IP — a common nginx recipe — must also be set
	// to append X-Forwarded-For, or every client collapses into a single rate-limit bucket keyed by the
	// proxy's own address.
	TrustProxyHeaders bool

	// TrustedProxyHops is how many trusted proxies sit between the client and this process. Only
	// consulted when TrustProxyHeaders is true.
	//
	// X-Forwarded-For is append-only, so the client's real address is the entry this many positions from
	// the *end* of the header — everything further left was supplied by the client and is worthless. Set
	// it to the number of proxies that each append one entry: 1 for a single reverse proxy or Ingress, 2
	// for a CDN in front of one, and so on. Too low reads a proxy's address as the client's; too high
	// reads a client-supplied value as trusted, so this must match the real topology.
	//
	// Validated only when TrustProxyHeaders is on: an operator who sets 0 to mean "no proxies in front of
	// me" while leaving trust off should not get a hard startup failure over a value nothing reads.
	TrustedProxyHops int `validate:"required_if=TrustProxyHeaders true,omitempty,gte=1"`

	// ShutdownTimeout bounds graceful shutdown before in-flight requests are dropped.
	ShutdownTimeout time.Duration `validate:"required,gt=0"`

	// The settings below are written by `norite instance init` (Milestone M2) and validated from this
	// milestone on, but the features that read them arrive later: attachment storage, automatic HTTPS
	// (docs/adr/0020-operations.md), and invite-gated registration (Milestone M10). They are validated
	// now so a misconfigured instance fails at startup, next to every other configuration error, rather
	// than at the moment someone first uploads a file months later.

	// StorageBackend selects where uploaded attachments live: "local" disk or an S3-compatible service
	// (AWS S3, MinIO, and anything else speaking the same API).
	StorageBackend string `validate:"required,oneof=local s3"`

	// StorageLocalPath is the directory attachments are written to when StorageBackend is "local".
	StorageLocalPath string `validate:"required_if=StorageBackend local"`

	// S3Endpoint is the S3-compatible service's base URL. Required for MinIO and other non-AWS services;
	// left empty for AWS S3 itself, where the region determines the endpoint.
	S3Endpoint string `validate:"omitempty,url"`

	// S3Region, S3Bucket, and the credential pair address the bucket.
	//
	// Never log S3SecretAccessKey (CLAUDE.md rule 8) — it is a credential, exactly like DatabaseURL's
	// embedded password.
	S3Region          string `validate:"required_if=StorageBackend s3"`
	S3Bucket          string `validate:"required_if=StorageBackend s3"`
	S3AccessKeyID     string `validate:"required_if=StorageBackend s3"`
	S3SecretAccessKey string `validate:"required_if=StorageBackend s3"`

	// S3ForcePathStyle addresses buckets as endpoint/bucket rather than bucket.endpoint. MinIO and most
	// self-hosted S3 implementations need this; AWS S3 does not.
	S3ForcePathStyle bool

	// ACMEEnabled turns on the backend's own certificate provisioning and renewal (certmagic). Off by
	// default, which is what a LAN-only self-hoster and the flagship both need — the flagship terminates
	// TLS at the Ingress with cert-manager instead, and every replica racing for the same Let's Encrypt
	// certificate would hit rate limits (docs/adr/0021-flagship-kubernetes-deployment.md).
	ACMEEnabled bool

	// ACMEDomain is the hostname a certificate is issued for; ACMEEmail receives expiry notices from the
	// CA. Both are required once ACME is on — the protocol cannot proceed without them.
	ACMEDomain string `validate:"required_if=ACMEEnabled true,omitempty,hostname"`
	ACMEEmail  string `validate:"required_if=ACMEEnabled true,omitempty,email"`

	// RegistrationMode is "open" (anyone may create an account) or "invite" (an instance invite code is
	// required). Enforced by the registration endpoint from Milestone M10.
	RegistrationMode string `validate:"required,oneof=open invite"`
}

// envPrefix namespaces every variable this package reads.
const envPrefix = "NORITE_"

// Load reads configuration, applies defaults, and validates the result.
//
// Values are layered, highest precedence first: NORITE_* environment variables, then the instance config
// file (see file.go), then the built-in defaults. configPath is the server's -config flag; empty means
// "discover the file the usual way", and no file at all is a perfectly normal, fully supported setup.
//
// It returns an error rather than exiting so the caller (the composition root) owns process lifetime.
// SourcePath reports which file, if any, contributed — the caller logs it so an operator can tell at a
// glance which file a running instance actually read.
func Load(configPath string) (Config, error) {
	file, sourcePath, err := loadFile(configPath)
	if err != nil {
		return Config{}, err
	}
	if file == nil {
		// A zero fileConfig has every pointer nil, so every setting falls through to its built-in
		// default. That keeps the layering below single-path — there is no separate "no file" branch.
		file = &fileConfig{}
		sourcePath = ""
	}

	env := Environment(getEnvString("ENV", fileString(file.Env, string(EnvDevelopment))))

	cfg := Config{
		Env:        env,
		SourcePath: sourcePath,
		ListenAddr: getEnvString("LISTEN_ADDR", fileString(file.HTTP.ListenAddr, ":8080")),
		// No default: a Postgres URL is deployment-specific and there is no safe guess. An empty value
		// fails validation with a clear message instead of silently trying localhost.
		DatabaseURL: getEnvString("DATABASE_URL", fileString(file.Database.URL, "")),
		LogLevel:    getEnvString("LOG_LEVEL", fileString(file.Log.Level, "info")),
		LogFormat:   getEnvString("LOG_FORMAT", fileString(file.Log.Format, defaultLogFormat(env))),
		RateLimit:   getEnvString("RATELIMIT", fileString(file.RateLimit.REST, "600-M")),

		StorageBackend:   getEnvString("STORAGE_BACKEND", fileString(file.Storage.Backend, "local")),
		StorageLocalPath: getEnvString("STORAGE_LOCAL_PATH", fileString(file.Storage.LocalPath, defaultStorageLocalPath())),

		S3Endpoint:        getEnvString("S3_ENDPOINT", fileString(file.Storage.S3.Endpoint, "")),
		S3Region:          getEnvString("S3_REGION", fileString(file.Storage.S3.Region, "")),
		S3Bucket:          getEnvString("S3_BUCKET", fileString(file.Storage.S3.Bucket, "")),
		S3AccessKeyID:     getEnvString("S3_ACCESS_KEY_ID", fileString(file.Storage.S3.AccessKeyID, "")),
		S3SecretAccessKey: getEnvString("S3_SECRET_ACCESS_KEY", fileString(file.Storage.S3.SecretAccessKey, "")),

		ACMEDomain: getEnvString("ACME_DOMAIN", fileString(file.ACME.Domain, "")),
		ACMEEmail:  getEnvString("ACME_EMAIL", fileString(file.ACME.Email, "")),

		RegistrationMode: getEnvString("REGISTRATION_MODE", fileString(file.Registration.Mode, "open")),
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	maxConns, err := getEnvInt32("DB_MAX_CONNS", fileInt32(file.Database.MaxConns, defaultDBMaxConns()))
	collect(err)
	cfg.DBMaxConns = maxConns

	minConns, err := getEnvInt32("DB_MIN_CONNS", fileInt32(file.Database.MinConns, defaultDBMinConns(maxConns)))
	collect(err)
	cfg.DBMinConns = minConns

	connectTimeoutDefault, err := fileDuration(file.Database.ConnectTimeout, 10*time.Second, "[database].connect_timeout")
	collect(err)
	connectTimeout, err := getEnvDuration("DB_CONNECT_TIMEOUT", connectTimeoutDefault)
	collect(err)
	cfg.DBConnectTimeout = connectTimeout

	lockTimeoutDefault, err := fileDuration(file.Database.MigrateLockTimeout, 2*time.Minute, "[database].migrate_lock_timeout")
	collect(err)
	lockTimeout, err := getEnvDuration("MIGRATE_LOCK_TIMEOUT", lockTimeoutDefault)
	collect(err)
	cfg.MigrateLockTimeout = lockTimeout

	shutdownTimeoutDefault, err := fileDuration(file.HTTP.ShutdownTimeout, 15*time.Second, "[http].shutdown_timeout")
	collect(err)
	shutdownTimeout, err := getEnvDuration("SHUTDOWN_TIMEOUT", shutdownTimeoutDefault)
	collect(err)
	cfg.ShutdownTimeout = shutdownTimeout

	trustProxy, err := getEnvBool("TRUST_PROXY_HEADERS", fileBool(file.HTTP.TrustProxyHeaders, false))
	collect(err)
	cfg.TrustProxyHeaders = trustProxy

	proxyHops, err := getEnvInt32("TRUSTED_PROXY_HOPS", fileInt32(file.HTTP.TrustedProxyHops, 1))
	collect(err)
	cfg.TrustedProxyHops = int(proxyHops)

	forcePathStyle, err := getEnvBool("S3_FORCE_PATH_STYLE", fileBool(file.Storage.S3.ForcePathStyle, false))
	collect(err)
	cfg.S3ForcePathStyle = forcePathStyle

	acmeEnabled, err := getEnvBool("ACME_ENABLED", fileBool(file.ACME.Enabled, false))
	collect(err)
	cfg.ACMEEnabled = acmeEnabled

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the struct tags and any cross-field rules that don't express well as tags.
func (c Config) Validate() error {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.Struct(c); err != nil {
		var invalid *validator.InvalidValidationError
		if errors.As(err, &invalid) {
			return fmt.Errorf("config: validation could not run: %w", err)
		}
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			msgs := make([]string, 0, len(verrs))
			for _, fe := range verrs {
				msgs = append(msgs, describeFieldError(fe, c.SourcePath))
			}
			return fmt.Errorf("config: invalid configuration: %s", strings.Join(msgs, "; "))
		}
		return fmt.Errorf("config: invalid configuration: %w", err)
	}
	return nil
}

// IsProduction reports whether production-flavored defaults apply.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// describeFieldError turns a validator failure into a message naming what an operator would actually go
// and edit, rather than the Go field name.
//
// Which name that is depends on how the instance is configured. Naming only the environment variable
// sends an operator who configured everything through a file looking for a variable they never set, so
// when a file contributed, the setting is named in both vocabularies. The layer a value actually came
// from isn't tracked per-field — that would mean threading provenance through every getEnv* call for a
// message — so this points at both possibilities rather than guessing wrong.
func describeFieldError(fe validator.FieldError, sourcePath string) string {
	name := envVarFor(fe.Field())
	if sourcePath != "" {
		if key := fileKeyFor(fe.Field()); key != "" {
			name = fmt.Sprintf("%s (%s in %s)", name, key, sourcePath)
		}
	}
	if fe.Param() == "" {
		return fmt.Sprintf("%s: failed %q", name, fe.Tag())
	}
	return fmt.Sprintf("%s: failed %q (%s)", name, fe.Tag(), fe.Param())
}

// fileKeyFor maps a Go field name to its key in the instance config file. Kept beside envVarFor because
// the two must move together: a setting that gains one name needs the other.
func fileKeyFor(field string) string {
	switch field {
	case "Env":
		return "env"
	case "ListenAddr":
		return "[http].listen_addr"
	case "ShutdownTimeout":
		return "[http].shutdown_timeout"
	case "TrustedProxyHops":
		return "[http].trusted_proxy_hops"
	case "DatabaseURL":
		return "[database].url"
	case "DBMaxConns":
		return "[database].max_conns"
	case "DBMinConns":
		return "[database].min_conns"
	case "DBConnectTimeout":
		return "[database].connect_timeout"
	case "MigrateLockTimeout":
		return "[database].migrate_lock_timeout"
	case "LogLevel":
		return "[log].level"
	case "LogFormat":
		return "[log].format"
	case "RateLimit":
		return "[rate_limit].rest"
	case "StorageBackend":
		return "[storage].backend"
	case "StorageLocalPath":
		return "[storage].local_path"
	case "S3Endpoint":
		return "[storage.s3].endpoint"
	case "S3Region":
		return "[storage.s3].region"
	case "S3Bucket":
		return "[storage.s3].bucket"
	case "S3AccessKeyID":
		return "[storage.s3].access_key_id"
	case "S3SecretAccessKey":
		return "[storage.s3].secret_access_key"
	case "ACMEDomain":
		return "[acme].domain"
	case "ACMEEmail":
		return "[acme].email"
	case "RegistrationMode":
		return "[registration].mode"
	default:
		return ""
	}
}

// envVarFor maps a Go field name back to the environment variable that sets it.
func envVarFor(field string) string {
	switch field {
	case "Env":
		return envPrefix + "ENV"
	case "ListenAddr":
		return envPrefix + "LISTEN_ADDR"
	case "DatabaseURL":
		return envPrefix + "DATABASE_URL"
	case "DBMaxConns":
		return envPrefix + "DB_MAX_CONNS"
	case "DBMinConns":
		return envPrefix + "DB_MIN_CONNS"
	case "DBConnectTimeout":
		return envPrefix + "DB_CONNECT_TIMEOUT"
	case "MigrateLockTimeout":
		return envPrefix + "MIGRATE_LOCK_TIMEOUT"
	case "LogLevel":
		return envPrefix + "LOG_LEVEL"
	case "LogFormat":
		return envPrefix + "LOG_FORMAT"
	case "RateLimit":
		return envPrefix + "RATELIMIT"
	case "TrustedProxyHops":
		return envPrefix + "TRUSTED_PROXY_HOPS"
	case "ShutdownTimeout":
		return envPrefix + "SHUTDOWN_TIMEOUT"
	case "StorageBackend":
		return envPrefix + "STORAGE_BACKEND"
	case "StorageLocalPath":
		return envPrefix + "STORAGE_LOCAL_PATH"
	case "S3Endpoint":
		return envPrefix + "S3_ENDPOINT"
	case "S3Region":
		return envPrefix + "S3_REGION"
	case "S3Bucket":
		return envPrefix + "S3_BUCKET"
	case "S3AccessKeyID":
		return envPrefix + "S3_ACCESS_KEY_ID"
	case "S3SecretAccessKey":
		return envPrefix + "S3_SECRET_ACCESS_KEY"
	case "ACMEDomain":
		return envPrefix + "ACME_DOMAIN"
	case "ACMEEmail":
		return envPrefix + "ACME_EMAIL"
	case "RegistrationMode":
		return envPrefix + "REGISTRATION_MODE"
	default:
		return field
	}
}

func defaultLogFormat(env Environment) string {
	if env == EnvProduction {
		return "json"
	}
	return "console"
}

// defaultDBMaxConns sizes the pool relative to available cores but keeps it deliberately small
// (docs/architecture.md §11, §15.3). pgx's own default is max(4, NumCPU) with no ceiling, which on a
// large host would quietly claim a big share of Postgres's connection budget per replica.
func defaultDBMaxConns() int32 {
	const (
		floor   = 4
		ceiling = 16
	)
	n := runtime.NumCPU()
	if n < floor {
		n = floor
	}
	if n > ceiling {
		n = ceiling
	}
	return int32(n)
}

// defaultDBMinConns keeps a couple of connections warm so the first request after an idle period doesn't
// pay full connection-establishment cost, without pinning the whole pool open.
func defaultDBMinConns(maxConns int32) int32 {
	const warm = 2
	if maxConns < warm {
		return maxConns
	}
	return warm
}

func getEnvString(key, fallback string) string {
	if v, ok := os.LookupEnv(envPrefix + key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt32(key string, fallback int32) (int32, error) {
	raw, ok := os.LookupEnv(envPrefix + key)
	if !ok || raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("config: %s%s: %q is not an integer", envPrefix, key, raw)
	}
	return int32(n), nil
}

func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(envPrefix + key)
	if !ok || raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s%s: %q is not a duration (e.g. \"30s\", \"2m\")", envPrefix, key, raw)
	}
	return d, nil
}

func getEnvBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(envPrefix + key)
	if !ok || raw == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s%s: %q is not a boolean (true/false/1/0)", envPrefix, key, raw)
	}
	return b, nil
}
