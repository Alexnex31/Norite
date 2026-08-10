// Package config holds the backend's typed, environment-bound configuration.
//
// Everything the process needs to boot is read once, at startup, into a single validated Config value —
// no scattered os.Getenv calls deeper in the tree. Invalid configuration fails the process immediately
// with an actionable message rather than surfacing as a confusing runtime error later.
//
// The instance config *file* written by `app instance init` (Milestone M2) layers on top of this later;
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
}

// envPrefix namespaces every variable this package reads.
const envPrefix = "NORITE_"

// Load reads configuration from the environment, applies defaults, and validates the result.
//
// It returns an error rather than exiting so the caller (the composition root) owns process lifetime.
func Load() (Config, error) {
	env := Environment(getEnvString("ENV", string(EnvDevelopment)))

	cfg := Config{
		Env:        env,
		ListenAddr: getEnvString("LISTEN_ADDR", ":8080"),
		// No default: a Postgres URL is deployment-specific and there is no safe guess. An empty value
		// fails validation with a clear message instead of silently trying localhost.
		DatabaseURL: getEnvString("DATABASE_URL", ""),
		LogLevel:    getEnvString("LOG_LEVEL", "info"),
		LogFormat:   getEnvString("LOG_FORMAT", defaultLogFormat(env)),
		RateLimit:   getEnvString("RATELIMIT", "600-M"),
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	maxConns, err := getEnvInt32("DB_MAX_CONNS", defaultDBMaxConns())
	collect(err)
	cfg.DBMaxConns = maxConns

	minConns, err := getEnvInt32("DB_MIN_CONNS", defaultDBMinConns(maxConns))
	collect(err)
	cfg.DBMinConns = minConns

	connectTimeout, err := getEnvDuration("DB_CONNECT_TIMEOUT", 10*time.Second)
	collect(err)
	cfg.DBConnectTimeout = connectTimeout

	lockTimeout, err := getEnvDuration("MIGRATE_LOCK_TIMEOUT", 2*time.Minute)
	collect(err)
	cfg.MigrateLockTimeout = lockTimeout

	shutdownTimeout, err := getEnvDuration("SHUTDOWN_TIMEOUT", 15*time.Second)
	collect(err)
	cfg.ShutdownTimeout = shutdownTimeout

	trustProxy, err := getEnvBool("TRUST_PROXY_HEADERS", false)
	collect(err)
	cfg.TrustProxyHeaders = trustProxy

	proxyHops, err := getEnvInt32("TRUSTED_PROXY_HOPS", 1)
	collect(err)
	cfg.TrustedProxyHops = int(proxyHops)

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
				msgs = append(msgs, describeFieldError(fe))
			}
			return fmt.Errorf("config: invalid configuration: %s", strings.Join(msgs, "; "))
		}
		return fmt.Errorf("config: invalid configuration: %w", err)
	}
	return nil
}

// IsProduction reports whether production-flavored defaults apply.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// describeFieldError turns a validator failure into a message naming the environment variable an operator
// would actually edit, not the Go field name.
func describeFieldError(fe validator.FieldError) string {
	name := envVarFor(fe.Field())
	if fe.Param() == "" {
		return fmt.Sprintf("%s: failed %q", name, fe.Tag())
	}
	return fmt.Sprintf("%s: failed %q (%s)", name, fe.Tag(), fe.Param())
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
