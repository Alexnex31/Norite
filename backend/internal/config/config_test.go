package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validDSN = "postgres://norite:norite@localhost:5432/norite?sslmode=disable"

// testJWTSecret is a fixture signing key. Required to boot from M4 on, exactly like the database URL, so
// every fixture that sets one sets both.
const testJWTSecret = "test-signing-key-at-least-32-bytes-long"

// withoutConfigFile points file discovery at an empty document, so these tests exercise the
// environment-and-defaults path regardless of whether the machine running them happens to have a real
// /etc/norite/instance.toml. An empty file leaves every field unset, which is exactly the "no file
// anywhere" case — see TestLoadFindsNoFile for the genuinely-absent path.
func withoutConfigFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	t.Setenv(ConfigFileEnvVar, path)
}

func TestLoadDefaults(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, EnvDevelopment, cfg.Env)
	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, validDSN, cfg.DatabaseURL)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "console", cfg.LogFormat, "development defaults to human-readable logs")
	assert.Equal(t, "600-M", cfg.RateLimit)
	assert.False(t, cfg.TrustProxyHeaders, "proxy headers must not be trusted unless explicitly enabled")
	assert.Equal(t, 1, cfg.TrustedProxyHops)

	// Pool stays deliberately small regardless of host size (docs/architecture.md §11).
	assert.GreaterOrEqual(t, cfg.DBMaxConns, int32(4))
	assert.LessOrEqual(t, cfg.DBMaxConns, int32(16))
	assert.LessOrEqual(t, cfg.DBMinConns, cfg.DBMaxConns)
}

func TestLoadProductionDefaultsToJSONLogs(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	t.Setenv(envPrefix+"ENV", string(EnvProduction))

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.True(t, cfg.IsProduction())
}

func TestLoadOverrides(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	t.Setenv(envPrefix+"LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv(envPrefix+"DB_MAX_CONNS", "8")
	t.Setenv(envPrefix+"DB_MIN_CONNS", "1")
	t.Setenv(envPrefix+"DB_CONNECT_TIMEOUT", "3s")
	t.Setenv(envPrefix+"MIGRATE_LOCK_TIMEOUT", "45s")
	t.Setenv(envPrefix+"SHUTDOWN_TIMEOUT", "5s")
	t.Setenv(envPrefix+"TRUST_PROXY_HEADERS", "true")
	t.Setenv(envPrefix+"TRUSTED_PROXY_HOPS", "2")
	t.Setenv(envPrefix+"LOG_LEVEL", "debug")
	t.Setenv(envPrefix+"LOG_FORMAT", "json")
	t.Setenv(envPrefix+"RATELIMIT", "10-S")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9090", cfg.ListenAddr)
	assert.Equal(t, int32(8), cfg.DBMaxConns)
	assert.Equal(t, int32(1), cfg.DBMinConns)
	assert.Equal(t, 3*time.Second, cfg.DBConnectTimeout)
	assert.Equal(t, 45*time.Second, cfg.MigrateLockTimeout)
	assert.Equal(t, 5*time.Second, cfg.ShutdownTimeout)
	assert.True(t, cfg.TrustProxyHeaders)
	assert.Equal(t, 2, cfg.TrustedProxyHops)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, "10-S", cfg.RateLimit)
}

func TestLoadRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "missing database url",
			env:     map[string]string{},
			wantErr: "NORITE_DATABASE_URL",
		},
		{
			name:    "non-postgres database url",
			env:     map[string]string{"DATABASE_URL": "mysql://localhost/norite"},
			wantErr: "NORITE_DATABASE_URL",
		},
		{
			name:    "unknown environment",
			env:     map[string]string{"DATABASE_URL": validDSN, "ENV": "staging"},
			wantErr: "NORITE_ENV",
		},
		{
			name:    "listen addr without port",
			env:     map[string]string{"DATABASE_URL": validDSN, "LISTEN_ADDR": "localhost"},
			wantErr: "NORITE_LISTEN_ADDR",
		},
		{
			name:    "min conns above max conns",
			env:     map[string]string{"DATABASE_URL": validDSN, "DB_MAX_CONNS": "4", "DB_MIN_CONNS": "8"},
			wantErr: "NORITE_DB_MIN_CONNS",
		},
		{
			name:    "max conns not a number",
			env:     map[string]string{"DATABASE_URL": validDSN, "DB_MAX_CONNS": "many"},
			wantErr: "is not an integer",
		},
		{
			name:    "timeout not a duration",
			env:     map[string]string{"DATABASE_URL": validDSN, "SHUTDOWN_TIMEOUT": "soon"},
			wantErr: "is not a duration",
		},
		{
			name:    "trust proxy headers not a boolean",
			env:     map[string]string{"DATABASE_URL": validDSN, "TRUST_PROXY_HEADERS": "yes please"},
			wantErr: "is not a boolean",
		},
		{
			// Only rejected when the value is actually consulted; see the accepted case below.
			name: "zero trusted proxy hops while trusting proxies",
			env: map[string]string{
				"DATABASE_URL": validDSN, "TRUST_PROXY_HEADERS": "true", "TRUSTED_PROXY_HOPS": "0",
			},
			wantErr: "NORITE_TRUSTED_PROXY_HOPS",
		},
		{
			name:    "unknown log level",
			env:     map[string]string{"DATABASE_URL": validDSN, "LOG_LEVEL": "verbose"},
			wantErr: "NORITE_LOG_LEVEL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withoutConfigFile(t)
			for k, v := range tt.env {
				t.Setenv(envPrefix+k, v)
			}
			_, err := Load("")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// The database URL carries a password, so it must never end up in an error message an operator might
// paste into a bug report.
func TestValidationErrorsDoNotLeakTheDatabaseURL(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", "mysql://user:hunter2@localhost/norite")

	_, err := Load("")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

// The hop count is meaningless when proxy headers aren't trusted, so setting it to 0 to mean "no proxies
// in front of me" must not be a hard startup failure over a value nothing reads.
func TestZeroProxyHopsIsAcceptedWhenProxiesAreNotTrusted(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	t.Setenv(envPrefix+"TRUST_PROXY_HEADERS", "false")
	t.Setenv(envPrefix+"TRUSTED_PROXY_HOPS", "0")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.TrustedProxyHops)
}

func TestDefaultDBMinConnsNeverExceedsMax(t *testing.T) {
	assert.Equal(t, int32(1), defaultDBMinConns(1))
	assert.Equal(t, int32(2), defaultDBMinConns(2))
	assert.Equal(t, int32(2), defaultDBMinConns(16))
}

// Pool sizing follows GOMAXPROCS, not NumCPU. Since Go 1.25 GOMAXPROCS defaults to the cgroup CPU limit
// while NumCPU still reports the whole machine, so on Kubernetes a small pod on a large node would
// otherwise size its pool from the node's core count and claim far more of Postgres's connection budget
// than its share justifies — multiplied by every replica.
func TestPoolSizeFollowsTheCPULimitNotTheMachine(t *testing.T) {
	original := runtime.GOMAXPROCS(0)
	t.Cleanup(func() { runtime.GOMAXPROCS(original) })

	cases := []struct {
		procs int
		want  int32
	}{
		{1, 4},   // floor: a connection pool of 1 would serialize the whole instance
		{2, 4},   // still the floor
		{8, 8},   // proportional in the ordinary range
		{64, 16}, // ceiling: never claim an unbounded share per replica
	}
	for _, tc := range cases {
		runtime.GOMAXPROCS(tc.procs)
		if got := defaultDBMaxConns(); got != tc.want {
			t.Errorf("GOMAXPROCS=%d: pool size %d, want %d", tc.procs, got, tc.want)
		}
	}
}
