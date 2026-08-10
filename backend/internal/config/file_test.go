package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfigFile writes an instance config file and points discovery at it via NORITE_CONFIG_FILE.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv(ConfigFileEnvVar, path)
	return path
}

// noSystemConfigFile makes discovery find nothing at the conventional location, which is the state of a
// machine that has never run `norite instance init`.
func noSystemConfigFile(t *testing.T) {
	t.Helper()
	previous := systemConfigPath
	systemConfigPath = func() string { return filepath.Join(t.TempDir(), "absent", "instance.toml") }
	t.Cleanup(func() { systemConfigPath = previous })
}

func TestFileSuppliesValuesTheEnvironmentDoesNot(t *testing.T) {
	writeConfigFile(t, `
env = "production"

[http]
listen_addr = "127.0.0.1:9999"
shutdown_timeout = "42s"
trust_proxy_headers = true
trusted_proxy_hops = 3

[database]
url = "postgres://from-file@localhost:5432/norite"
max_conns = 11
min_conns = 3
connect_timeout = "7s"
migrate_lock_timeout = "90s"

[log]
level = "warn"
format = "json"

[rate_limit]
rest = "42-S"

[storage]
backend = "local"
local_path = "/srv/norite/files"

[acme]
enabled = true
domain = "chat.example.com"
email = "admin@example.com"

[registration]
mode = "invite"
`)

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, EnvProduction, cfg.Env)
	assert.Equal(t, "127.0.0.1:9999", cfg.ListenAddr)
	assert.Equal(t, 42*time.Second, cfg.ShutdownTimeout)
	assert.True(t, cfg.TrustProxyHeaders)
	assert.Equal(t, 3, cfg.TrustedProxyHops)
	assert.Equal(t, "postgres://from-file@localhost:5432/norite", cfg.DatabaseURL)
	assert.Equal(t, int32(11), cfg.DBMaxConns)
	assert.Equal(t, int32(3), cfg.DBMinConns)
	assert.Equal(t, 7*time.Second, cfg.DBConnectTimeout)
	assert.Equal(t, 90*time.Second, cfg.MigrateLockTimeout)
	assert.Equal(t, "warn", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, "42-S", cfg.RateLimit)
	assert.Equal(t, "local", cfg.StorageBackend)
	assert.Equal(t, "/srv/norite/files", cfg.StorageLocalPath)
	assert.True(t, cfg.ACMEEnabled)
	assert.Equal(t, "chat.example.com", cfg.ACMEDomain)
	assert.Equal(t, "invite", cfg.RegistrationMode)
}

// The precedence direction is load-bearing for the flagship: DATABASE_URL arrives from a Kubernetes
// Secret, and a config file baked into the image must never be able to shadow it.
func TestEnvironmentOverridesFile(t *testing.T) {
	writeConfigFile(t, `
env = "development"

[http]
listen_addr = "127.0.0.1:1111"
trust_proxy_headers = false
shutdown_timeout = "1s"

[database]
url = "postgres://from-file@localhost:5432/norite"
max_conns = 5

[log]
level = "error"

[registration]
mode = "invite"
`)

	t.Setenv(envPrefix+"ENV", string(EnvProduction))
	t.Setenv(envPrefix+"LISTEN_ADDR", "0.0.0.0:2222")
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"DB_MAX_CONNS", "9")
	t.Setenv(envPrefix+"SHUTDOWN_TIMEOUT", "30s")
	t.Setenv(envPrefix+"TRUST_PROXY_HEADERS", "true")
	t.Setenv(envPrefix+"LOG_LEVEL", "debug")
	t.Setenv(envPrefix+"REGISTRATION_MODE", "open")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, EnvProduction, cfg.Env)
	assert.Equal(t, "0.0.0.0:2222", cfg.ListenAddr)
	assert.Equal(t, validDSN, cfg.DatabaseURL)
	assert.Equal(t, int32(9), cfg.DBMaxConns)
	assert.Equal(t, 30*time.Second, cfg.ShutdownTimeout)
	assert.True(t, cfg.TrustProxyHeaders, "an env var must win even when the file says false")
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "open", cfg.RegistrationMode)
}

// A file that says nothing about a setting must leave it on its built-in default rather than on the zero
// value — the reason every field in the on-disk document is a pointer.
func TestUnsetFileKeysFallThroughToDefaults(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"
`)

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, EnvDevelopment, cfg.Env)
	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "600-M", cfg.RateLimit)
	assert.Equal(t, 15*time.Second, cfg.ShutdownTimeout)
	assert.Equal(t, 1, cfg.TrustedProxyHops)
	assert.Equal(t, "local", cfg.StorageBackend)
	assert.Equal(t, "open", cfg.RegistrationMode)
}

// "Present and set to the zero value" must be distinguishable from "absent", or a deliberate setting
// silently becomes whatever the default happens to be. trusted_proxy_hops is where that difference is
// observable: absent means 1, while an explicit 0 means "no proxies in front of me" and must survive.
func TestFileCanSetAZeroValue(t *testing.T) {
	writeConfigFile(t, `
[http]
trusted_proxy_hops = 0

[database]
url = "`+validDSN+`"
`)

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.TrustedProxyHops, "an explicit 0 must not fall through to the default of 1")
}

func TestUnknownFileKeyIsRejected(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"
maxconns = 8
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized setting")
	assert.Contains(t, err.Error(), "maxconns")
}

func TestMalformedFileIsRejected(t *testing.T) {
	writeConfigFile(t, "this is not toml = = =\n")

	_, err := Load("")
	require.Error(t, err)
}

// A duration typo should point at the file key the operator would edit, not at an environment variable
// they never set.
func TestBadDurationInFileNamesTheFileKey(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"

[http]
shutdown_timeout = "soon"
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[http].shutdown_timeout")
	assert.NotContains(t, err.Error(), "NORITE_SHUTDOWN_TIMEOUT")
}

// No file anywhere is the ordinary docker-compose and Kubernetes case, not an error.
func TestNoConfigFileIsNotAnError(t *testing.T) {
	noSystemConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Empty(t, cfg.SourcePath, "no file contributed, so none should be reported")
}

// Asking for a specific file that isn't there is a different situation entirely: starting on defaults
// that look nothing like what was asked for would be worse than refusing.
func TestExplicitlyRequestedFileMustExist(t *testing.T) {
	noSystemConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	missing := filepath.Join(t.TempDir(), "nope.toml")

	_, err := Load(missing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

// Same rule when the path came from the environment rather than the flag.
func TestConfigFileEnvVarMustExist(t *testing.T) {
	noSystemConfigFile(t)
	missing := filepath.Join(t.TempDir(), "nope.toml")
	t.Setenv(ConfigFileEnvVar, missing)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), missing)
}

// The -config flag is the operator's most explicit statement of intent, so it outranks the env var.
func TestExplicitPathBeatsTheEnvVar(t *testing.T) {
	fromEnv := writeConfigFile(t, `
[http]
listen_addr = "127.0.0.1:1111"

[database]
url = "`+validDSN+`"
`)
	fromFlag := filepath.Join(t.TempDir(), "flag.toml")
	require.NoError(t, os.WriteFile(fromFlag, []byte(`
[http]
listen_addr = "127.0.0.1:3333"

[database]
url = "`+validDSN+`"
`), 0o600))

	cfg, err := Load(fromFlag)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:3333", cfg.ListenAddr)
	assert.Equal(t, fromFlag, cfg.SourcePath)
	assert.NotEqual(t, fromEnv, cfg.SourcePath)
}

func TestSourcePathReportsTheFileThatWasRead(t *testing.T) {
	path := writeConfigFile(t, `
[database]
url = "`+validDSN+`"
`)

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, path, cfg.SourcePath)
}

// The S3 credential set is only meaningful once the S3 backend is selected, and selecting it without one
// would fail at first upload rather than at startup.
func TestS3BackendRequiresItsSettings(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"

[storage]
backend = "s3"
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NORITE_S3_BUCKET")
}

func TestACMERequiresDomainAndEmailWhenEnabled(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"

[acme]
enabled = true
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NORITE_ACME_DOMAIN")
}

// A file is operator-supplied configuration that carries the database password; a validation failure must
// not echo it back into a log or a bug report.
func TestFileValidationErrorsDoNotLeakTheDatabaseURL(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "mysql://user:hunter2@localhost/norite"
`)

	_, err := Load("")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

// An operator who configured this instance through a file should be told which key to edit — pointing
// them at an environment variable they never set sends them looking in the wrong place.
func TestValidationErrorsNameTheFileKeyWhenAFileWasUsed(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "`+validDSN+`"

[registration]
mode = "everyone"
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[registration].mode", "the file key an operator would edit")
	assert.Contains(t, err.Error(), "NORITE_REGISTRATION_MODE", "the variable that overrides it")
}

// With no file in play there is no file key to mention, and adding one would be noise.
func TestValidationErrorsNameOnlyTheVariableWithoutAFile(t *testing.T) {
	noSystemConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"LOG_LEVEL", "verbose")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NORITE_LOG_LEVEL")
	assert.NotContains(t, err.Error(), "[log].level")
}

// envVarFor and fileKeyFor are two names for the same setting and must not drift apart: a setting that
// gains one without the other produces an error message that names it only half the time.
func TestEveryValidatedFieldHasBothNames(t *testing.T) {
	for _, field := range []string{
		"Env", "ListenAddr", "DatabaseURL", "DBMaxConns", "DBMinConns", "DBConnectTimeout",
		"MigrateLockTimeout", "LogLevel", "LogFormat", "RateLimit", "TrustedProxyHops", "ShutdownTimeout",
		"StorageBackend", "StorageLocalPath", "S3Endpoint", "S3Region", "S3Bucket", "S3AccessKeyID",
		"S3SecretAccessKey", "ACMEDomain", "ACMEEmail", "RegistrationMode",
	} {
		assert.NotEqual(t, field, envVarFor(field), "%s has no environment-variable name", field)
		assert.NotEmpty(t, fileKeyFor(field), "%s has no config-file key", field)
	}
}

// A typo'd key must be named without reproducing any of the file around it.
//
// go-toml's StrictMissingError.String() renders the neighboring lines to show where the unknown key sits,
// and in this file the line above a mistake is very often the database URL — so printing it would put the
// password on an operator's terminal and into whatever bug report they paste it into (rule 8).
func TestUnknownKeyErrorDoesNotEchoTheFile(t *testing.T) {
	writeConfigFile(t, `
[database]
url = "postgres://norite:hunter2@localhost:5432/norite"
maxconns = 8
`)

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxconns", "the offending key must still be named")
	assert.Contains(t, err.Error(), "line 4", "and located")
	assert.NotContains(t, err.Error(), "hunter2", "but no neighboring line may be reproduced")
	assert.NotContains(t, err.Error(), "postgres://")
}

// The same applies to a syntax error, which go-toml can also render with a document excerpt.
func TestMalformedFileErrorDoesNotEchoTheFile(t *testing.T) {
	writeConfigFile(t, "[database]\nurl = \"postgres://norite:hunter2@localhost/norite\"\nbroken = = =\n")

	_, err := Load("")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}
