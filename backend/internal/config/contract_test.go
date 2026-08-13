package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contractPath is the reference instance config every side of the project is checked against.
// `norite instance init` is verified against the same file from the CLI module (cli/internal/instanceinit),
// which is what keeps the wizard's output and the backend's understanding of it from drifting apart —
// the two live in separate Go modules and cannot import each other's types.
const contractPath = "../../../contracts/instance-config.toml"

// The contract lists every key the backend understands, so loading it must exercise every one of them
// without hitting an unknown-key rejection or a validation failure. If this breaks, either a key was
// added to the backend without being documented, or one was removed from the backend while still being
// advertised to clients.
func TestContractFileLoadsAndValidates(t *testing.T) {
	abs, err := filepath.Abs(contractPath)
	require.NoError(t, err)
	require.FileExists(t, abs, "the instance config contract must exist")

	// The contract is a complete document, so nothing should need supplementing from the environment.
	// Anything Load reads from the environment here would be a key the contract failed to cover.
	cfg, err := Load(abs)
	require.NoError(t, err, "the contract document must be loadable by the backend that publishes it")

	assert.Equal(t, abs, cfg.SourcePath)
	assert.Equal(t, EnvProduction, cfg.Env)
	assert.NotEmpty(t, cfg.DatabaseURL)

	// The contract deliberately selects the settings with dependent validation rules — S3 storage, ACME
	// and SMTP each require companion values — so that loading it proves those paths are satisfiable.
	assert.Equal(t, "s3", cfg.StorageBackend)
	assert.NotEmpty(t, cfg.S3Bucket)
	assert.NotEmpty(t, cfg.S3Region)
	assert.True(t, cfg.ACMEEnabled)
	assert.NotEmpty(t, cfg.ACMEDomain)
	assert.Equal(t, "invite", cfg.RegistrationMode)

	// SMTP's chain is the longest: enabling it requires host, port, encryption, a from address, and
	// [http].public_base_url, which lives in a different section entirely.
	assert.True(t, cfg.SMTPEnabled)
	assert.NotEmpty(t, cfg.SMTPHost)
	assert.NotZero(t, cfg.SMTPPort)
	assert.NotEmpty(t, cfg.SMTPFromAddress)
	assert.NotEmpty(t, cfg.PublicBaseURL)
}

// Turning SMTP on without the values it depends on must fail at startup rather than at the moment someone
// first requests a password reset. The cross-section dependency is the one worth pinning: public_base_url
// is under [http], so nothing about the [smtp] block hints that it is needed.
func TestEnablingSMTPRequiresItsCompanionValues(t *testing.T) {
	base := map[string]string{
		envPrefix + "DATABASE_URL":      validDSN,
		envPrefix + "JWT_SECRET":        testJWTSecret,
		envPrefix + "SMTP_ENABLED":      "true",
		envPrefix + "SMTP_HOST":         "smtp.example.com",
		envPrefix + "SMTP_FROM_ADDRESS": "no-reply@example.com",
		envPrefix + "PUBLIC_BASE_URL":   "https://chat.example.com",
	}

	// setAll applies base minus one variable, so each subtest sees exactly one missing companion.
	setAll := func(t *testing.T, omit string) {
		t.Helper()
		withoutConfigFile(t)
		for name, value := range base {
			if name != omit {
				t.Setenv(name, value)
			}
		}
	}

	t.Run("fully specified", func(t *testing.T) {
		setAll(t, "")
		_, err := Load("")
		require.NoError(t, err, "a complete SMTP config must load")
	})

	for _, missing := range []string{
		envPrefix + "SMTP_HOST",
		envPrefix + "SMTP_FROM_ADDRESS",
		envPrefix + "PUBLIC_BASE_URL",
	} {
		t.Run("without "+missing, func(t *testing.T) {
			setAll(t, missing)

			_, err := Load("")
			require.Error(t, err, "%s is required once SMTP is on", missing)
			assert.Contains(t, err.Error(), missing,
				"the error must name the variable an operator has to set, not just the Go field")
		})
	}

	// The inverse: with SMTP off, none of them is required. Otherwise every LAN-only self-hoster would be
	// forced to invent a relay to start the process, which is the opposite of the opt-out ADR 0020 wants.
	t.Run("not required while SMTP is off", func(t *testing.T) {
		withoutConfigFile(t)
		t.Setenv(envPrefix+"DATABASE_URL", validDSN)
		t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)

		cfg, err := Load("")
		require.NoError(t, err)
		assert.False(t, cfg.SMTPEnabled)
		assert.Empty(t, cfg.PublicBaseURL)
	})
}

// Every key in the contract must be one the decoder accepts. Loading with unknown-field rejection already
// proves that, but only for keys the contract happens to set — this asserts the contract is not silently
// empty or truncated, which would make the check above pass vacuously.
func TestContractFileCoversEveryConfigSection(t *testing.T) {
	body, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	doc := string(body)

	for _, section := range []string{
		"[http]", "[database]", "[log]", "[rate_limit]",
		"[storage]", "[storage.s3]", "[acme]", "[smtp]",
		"[oauth.google]", "[oauth.github]", "[registration]", "[auth]",
	} {
		assert.Contains(t, doc, section, "the contract must document every configuration section")
	}
}

// The list above is hand-maintained, so it can only catch a section someone remembered to add to it. This
// catches the case it cannot: a Config field with no key in the contract at all.
//
// Every field must be reachable from the file, because the contract's stated job is to list every key the
// backend understands — an operator who reads it and finds nothing about a setting reasonably concludes
// the setting does not exist.
func TestEveryConfigFieldHasAContractKey(t *testing.T) {
	body, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	doc := string(body)

	// Fields that are deliberately not file-settable.
	exempt := map[string]bool{
		"SourcePath": true, // diagnostic metadata, filled in by Load itself
	}

	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if exempt[name] {
			continue
		}

		key := fileKeyFor(name)
		require.NotEmpty(t, key,
			"Config.%s has no [section].key mapping — add one to fileKeyFor, or exempt it here", name)

		// fileKeyFor returns "[section].key" for most fields and a bare key for the handful that sit at
		// the document's top level (env). Cut on "]." rather than the first ".", or a nested section like
		// "[storage.s3].bucket" splits in the wrong place.
		bare := key
		if _, after, sectioned := strings.Cut(key, "]."); sectioned {
			bare = after
		}
		assert.Contains(t, doc, bare+" =",
			"Config.%s maps to %s, which contracts/instance-config.toml never sets", name, key)
	}
}

// A provider is configured when both halves are present, and half a provider is always a mistake rather
// than a configuration — a client ID with no secret fails at the token exchange, with a message from
// Google or GitHub rather than from here.
func TestAnOAuthProviderNeedsBothHalves(t *testing.T) {
	base := map[string]string{
		envPrefix + "DATABASE_URL":    validDSN,
		envPrefix + "JWT_SECRET":      testJWTSecret,
		envPrefix + "PUBLIC_BASE_URL": "https://chat.example.com",
	}

	load := func(t *testing.T, extra map[string]string) (Config, error) {
		t.Helper()
		withoutConfigFile(t)
		for k, v := range base {
			t.Setenv(k, v)
		}
		for k, v := range extra {
			t.Setenv(k, v)
		}
		return Load("")
	}

	t.Run("neither half is fine", func(t *testing.T) {
		cfg, err := load(t, nil)
		require.NoError(t, err, "an instance with no OAuth provider must start normally")
		assert.False(t, cfg.OAuthConfigured())
	})

	t.Run("both halves configure the provider", func(t *testing.T) {
		cfg, err := load(t, map[string]string{
			envPrefix + "GOOGLE_CLIENT_ID":     "id.apps.googleusercontent.com",
			envPrefix + "GOOGLE_CLIENT_SECRET": "secret",
		})
		require.NoError(t, err)
		assert.True(t, cfg.GoogleOAuthConfigured())
		assert.False(t, cfg.GitHubOAuthConfigured(), "one provider must not imply the other")
		assert.True(t, cfg.OAuthConfigured())
	})

	for name, half := range map[string]string{
		"id without secret": envPrefix + "GOOGLE_CLIENT_ID",
		"secret without id": envPrefix + "GOOGLE_CLIENT_SECRET",
		"github id alone":   envPrefix + "GITHUB_CLIENT_ID",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, map[string]string{half: "value"})
			require.Error(t, err, "half a provider must fail at startup")
			assert.Contains(t, err.Error(), envPrefix, "the error must name the variable to set")
		})
	}
}

// public_base_url is required by two independent features, which no single struct tag can express: the
// provider redirects back to it, exactly as password reset builds a link from it.
func TestOAuthRequiresThePublicBaseURL(t *testing.T) {
	withoutConfigFile(t)
	t.Setenv(envPrefix+"DATABASE_URL", validDSN)
	t.Setenv(envPrefix+"JWT_SECRET", testJWTSecret)
	t.Setenv(envPrefix+"GITHUB_CLIENT_ID", "Iv1.example")
	t.Setenv(envPrefix+"GITHUB_CLIENT_SECRET", "secret")

	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), envPrefix+"PUBLIC_BASE_URL")
	assert.Contains(t, err.Error(), "redirects back to it",
		"the message must say why, not just that it is required")
}
