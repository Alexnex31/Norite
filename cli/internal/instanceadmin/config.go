package instanceadmin

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Alexnex31/Norite/cli/internal/instanceinit"
)

// Reading back the file `norite instance init` wrote.
//
// # Why the CLI reads a server's config file at all
//
// Because that file *is* the authority this command runs on. Instance administration before any account
// exists cannot authenticate as anybody, so what stands in for a credential is possession of the instance
// signing key — and the key lives here, beside the database password, readable only by the account that
// runs the server. Reading it is how the command proves it is being run by the operator.
//
// This is deliberately a small, targeted reader rather than a mirror of backend/internal/config. It takes
// the two settings the command needs and ignores everything else, so a config key added anywhere in the
// document never breaks this command — and so nothing here can drift into being a second opinion on what
// the backend's configuration means.

// Config is what this command needs out of an instance's configuration.
type Config struct {
	// JWTSecret signs the operator token. The whole authority of this command rests on it.
	JWTSecret string
	// PublicBaseURL is where the instance answers. Empty is legal — an instance is not required to know
	// its own address until something needs to build a link — so the command asks for --instance instead.
	PublicBaseURL string
	// Path is the file these came from, for messages. Never printed with the secret.
	Path string
	// SecretFromEnv records that the signing key came from the environment rather than the file, which
	// changes what a signature failure means and therefore what the advice should be.
	SecretFromEnv bool
}

// instanceFile is the subset of instance.toml this command parses.
//
// Unknown keys are ignored here, unlike at the backend, which rejects them at startup. The difference is
// deliberate: the backend rejects because a typo silently leaving a setting on its default is a real
// misconfiguration it can catch. This command has no opinion on settings it does not use, and refusing to
// bootstrap because of a typo in the SMTP section would be an unrelated obstacle at the worst moment.
type instanceFile struct {
	HTTP struct {
		PublicBaseURL string `toml:"public_base_url"`
	} `toml:"http"`
	Auth struct {
		JWTSecret string `toml:"jwt_secret"`
	} `toml:"auth"`
}

// ErrNoConfig means no instance configuration could be found.
var ErrNoConfig = errors.New("no instance configuration found")

// ErrNoSigningKey means the configuration carries no signing key, so no operator token can be minted.
var ErrNoSigningKey = errors.New("this instance's configuration has no signing key")

// LoadConfig resolves the instance configuration, honoring the same precedence the backend does.
//
// Discovery: an explicit path, then NORITE_CONFIG_FILE, then the conventional location. A file named
// explicitly must exist — starting on defaults that look nothing like what was asked for is worse than
// refusing — while the conventional location is allowed to be absent.
//
// Precedence within it: environment overrides file, which is the direction docs/architecture.md §4 fixes
// and is load-bearing here rather than cosmetic. The flagship injects NORITE_JWT_SECRET from a Kubernetes
// Secret, and a config file baked into an image must never shadow it — an operator token minted from a
// stale file key would be refused by a server running on the injected one, and the error would point at
// the wrong thing entirely.
func LoadConfig(explicitPath string) (Config, error) {
	path, mustExist := resolveConfigPath(explicitPath)

	var file instanceFile
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own, from a flag or a convention
	switch {
	case err == nil:
		if err := toml.Unmarshal(raw, &file); err != nil {
			return Config{}, fmt.Errorf("reading %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !mustExist:
		// The conventional location is allowed to be absent: an instance configured entirely through the
		// environment is a fully supported setup (§4), and this command works on one.
	default:
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	cfg := Config{
		JWTSecret:     strings.TrimSpace(file.Auth.JWTSecret),
		PublicBaseURL: strings.TrimSpace(file.HTTP.PublicBaseURL),
		Path:          path,
	}
	if v := os.Getenv("NORITE_JWT_SECRET"); v != "" {
		cfg.JWTSecret = v
		cfg.SecretFromEnv = true
	}
	if v := os.Getenv("NORITE_PUBLIC_BASE_URL"); v != "" {
		cfg.PublicBaseURL = v
	}

	if cfg.JWTSecret == "" {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("%w at %s", ErrNoConfig, path)
		}
		return Config{}, fmt.Errorf("%w (%s)", ErrNoSigningKey, path)
	}
	return cfg, nil
}

// resolveConfigPath reports where to look, and whether absence is an error.
func resolveConfigPath(explicitPath string) (path string, mustExist bool) {
	if explicitPath != "" {
		return explicitPath, true
	}
	if fromEnv := os.Getenv("NORITE_CONFIG_FILE"); fromEnv != "" {
		return fromEnv, true
	}
	return instanceinit.DefaultConfigPath(), false
}
