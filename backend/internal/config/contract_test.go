package config

import (
	"os"
	"path/filepath"
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

	// The contract deliberately selects the settings with dependent validation rules — S3 storage and
	// ACME both require companion values — so that loading it proves those paths are satisfiable.
	assert.Equal(t, "s3", cfg.StorageBackend)
	assert.NotEmpty(t, cfg.S3Bucket)
	assert.NotEmpty(t, cfg.S3Region)
	assert.True(t, cfg.ACMEEnabled)
	assert.NotEmpty(t, cfg.ACMEDomain)
	assert.Equal(t, "invite", cfg.RegistrationMode)
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
		"[storage]", "[storage.s3]", "[acme]", "[registration]",
	} {
		assert.Contains(t, doc, section, "the contract must document every configuration section")
	}
}
