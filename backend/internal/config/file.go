package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// The instance config file is the artifact `norite instance init` writes (Milestone M2). It is entirely
// optional: an instance configured purely through NORITE_* variables — which is what the docker-compose
// stack and the flagship's Kubernetes deployment both do — never has one, and startup must not care.
//
// Precedence, highest first: environment variable, then this file, then the built-in default. The
// direction is deliberate and load-bearing for the flagship: DATABASE_URL arrives there from a Kubernetes
// Secret, and a config file baked into the image must never be able to shadow it. It also means the
// existing env-only deployments keep behaving exactly as they did before this file existed.
//
// Every key is optional. A file that sets only [database].url is valid and leaves everything else on its
// default. Unknown keys are rejected rather than ignored, so a typo fails loudly at startup instead of
// silently leaving a setting on its default.

// ConfigFileEnvVar names the variable that points at the instance config file, overriding discovery.
const ConfigFileEnvVar = envPrefix + "CONFIG_FILE"

// defaultConfigFilename is the file's name in whichever directory it is found.
const defaultConfigFilename = "instance.toml"

// fileConfig mirrors the on-disk TOML document.
//
// Every field is a pointer so "absent" is distinguishable from "present and set to the zero value" — the
// difference between a file that says nothing about trust_proxy_headers and one that deliberately says
// false. Only the former should fall through to the built-in default.
type fileConfig struct {
	Env *string `toml:"env"`

	HTTP struct {
		ListenAddr        *string `toml:"listen_addr"`
		ShutdownTimeout   *string `toml:"shutdown_timeout"`
		TrustProxyHeaders *bool   `toml:"trust_proxy_headers"`
		TrustedProxyHops  *int32  `toml:"trusted_proxy_hops"`
	} `toml:"http"`

	Database struct {
		URL                *string `toml:"url"`
		MaxConns           *int32  `toml:"max_conns"`
		MinConns           *int32  `toml:"min_conns"`
		ConnectTimeout     *string `toml:"connect_timeout"`
		MigrateLockTimeout *string `toml:"migrate_lock_timeout"`
	} `toml:"database"`

	Log struct {
		Level  *string `toml:"level"`
		Format *string `toml:"format"`
	} `toml:"log"`

	RateLimit struct {
		REST *string `toml:"rest"`
	} `toml:"rate_limit"`

	Storage struct {
		Backend   *string `toml:"backend"`
		LocalPath *string `toml:"local_path"`

		S3 struct {
			Endpoint        *string `toml:"endpoint"`
			Region          *string `toml:"region"`
			Bucket          *string `toml:"bucket"`
			AccessKeyID     *string `toml:"access_key_id"`
			SecretAccessKey *string `toml:"secret_access_key"`
			ForcePathStyle  *bool   `toml:"force_path_style"`
		} `toml:"s3"`
	} `toml:"storage"`

	ACME struct {
		Enabled *bool   `toml:"enabled"`
		Domain  *string `toml:"domain"`
		Email   *string `toml:"email"`
	} `toml:"acme"`

	Registration struct {
		Mode *string `toml:"mode"`
	} `toml:"registration"`

	Auth struct {
		JWTSecret *string `toml:"jwt_secret"`
	} `toml:"auth"`
}

// resolveFilePath decides which instance config file to read, if any.
//
// Search order: the explicit path (the server's -config flag), then NORITE_CONFIG_FILE, then the
// platform's conventional system location. found reports whether a path was chosen at all; explicit
// reports whether the operator named it, which is what separates "you asked for a file that isn't there"
// (a startup error) from "no file anywhere, run on environment variables alone" (entirely normal).
func resolveFilePath(explicitPath string) (path string, explicit, found bool) {
	if explicitPath != "" {
		return explicitPath, true, true
	}
	if v, ok := os.LookupEnv(ConfigFileEnvVar); ok && v != "" {
		return v, true, true
	}
	if p := systemConfigPath(); p != "" {
		return p, false, true
	}
	return "", false, false
}

// systemConfigPath is indirected through a variable purely so tests can point discovery somewhere other
// than the real machine's /etc — without it, whether the test suite passes would depend on whether the
// developer running it happens to have an instance config installed.
var systemConfigPath = defaultSystemConfigPath

// defaultSystemConfigPath is the conventional whole-machine location for a server's configuration.
//
// A per-user directory (os.UserConfigDir) would be wrong here: this configures a service, typically run by
// a system account or inside a container, not an interactive user's own tooling. The CLI/GUI client config
// at ~/.config/norite/config.toml is the per-user file, and is a different thing entirely
// (docs/architecture.md §3).
func defaultSystemConfigPath() string {
	if runtime.GOOS == "windows" {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			return ""
		}
		return filepath.Join(programData, "Norite", defaultConfigFilename)
	}
	return filepath.Join("/etc/norite", defaultConfigFilename)
}

// defaultStorageLocalPath is where attachments land when no storage path is configured. Alongside the
// config file's own directory convention: machine-scoped service data, not per-user data.
func defaultStorageLocalPath() string {
	if runtime.GOOS == "windows" {
		programData := os.Getenv("ProgramData")
		if programData == "" {
			return ""
		}
		return filepath.Join(programData, "Norite", "attachments")
	}
	return "/var/lib/norite/attachments"
}

// loadFile reads and parses the instance config file selected by resolveFilePath.
//
// A missing file is only an error when the operator named it explicitly: silently ignoring a bad -config
// path would start the instance on defaults that look nothing like what they asked for. A missing file at
// the conventional location is the ordinary env-only case and returns nil, nil.
func loadFile(explicitPath string) (*fileConfig, string, error) {
	path, explicit, found := resolveFilePath(explicitPath)
	if !found {
		return nil, "", nil
	}

	raw, err := readConfigFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("config: reading %s: %w", path, err)
	}

	var fc fileConfig
	dec := toml.NewDecoder(bytes.NewReader(raw))
	// Reject keys this build does not know. A typo'd key is far more likely to be a mistake an operator
	// wants to hear about immediately than a deliberate forward-compatible annotation.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		var sErr *toml.StrictMissingError
		if errors.As(err, &sErr) {
			return nil, "", fmt.Errorf("config: %s: unrecognized setting(s): %s", path, unknownKeys(sErr))
		}
		// Deliberately %w, which yields the library's plain message. The DecodeError's String() method
		// renders an excerpt of the surrounding document instead — and in this file the line above the
		// mistake is very often the database URL, so that excerpt would print the password (rule 8).
		return nil, "", fmt.Errorf("config: %s: %w", path, err)
	}
	return &fc, path, nil
}

// maxConfigFileSize caps how much of a config file is read.
//
// The real document is a couple of kilobytes. A megabyte is far past anything legitimate while still
// bounding what a wrong -config path — a log file, a database dump, /dev/zero — can pull into memory
// before the parser ever sees it. Same reasoning as httpx.DecodeJSON's body cap.
const maxConfigFileSize = 1 << 20

func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path is operator-supplied configuration, not user input
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only; nothing to report on close

	// One byte past the cap, so a file sitting exactly at the limit is still distinguishable from one
	// that exceeds it.
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigFileSize {
		return nil, fmt.Errorf("file is larger than %d bytes, which is not a configuration file", maxConfigFileSize)
	}
	return raw, nil
}

// unknownKeys names the offending settings without reproducing any of the file's contents.
//
// StrictMissingError.String() would be the obvious thing to print, but it renders the surrounding lines of
// the document to show where each unknown key sits — and the line above a typo in this file is very often
// `url = "postgres://user:password@..."`. Printing that would put the database password into an operator's
// terminal and into any bug report they paste it into. Key and position carry no content.
func unknownKeys(sErr *toml.StrictMissingError) string {
	names := make([]string, 0, len(sErr.Errors))
	for i := range sErr.Errors {
		decodeErr := &sErr.Errors[i]
		row, _ := decodeErr.Position()
		names = append(names, fmt.Sprintf("%q (line %d)", strings.Join(decodeErr.Key(), "."), row))
	}
	return strings.Join(names, ", ")
}

// The helpers below turn an optional file value into the fallback the corresponding getEnv* call uses, so
// the layering reads the same way for every setting: environment first, then file, then built-in.

func fileString(v *string, builtin string) string {
	if v != nil && *v != "" {
		return *v
	}
	return builtin
}

func fileInt32(v *int32, builtin int32) int32 {
	if v != nil {
		return *v
	}
	return builtin
}

func fileBool(v *bool, builtin bool) bool {
	if v != nil {
		return *v
	}
	return builtin
}

// fileDuration parses a duration string from the file. A malformed value is reported against the file's
// own key rather than the environment variable, since that is what the operator would go and edit.
func fileDuration(v *string, builtin time.Duration, key string) (time.Duration, error) {
	if v == nil || *v == "" {
		return builtin, nil
	}
	d, err := time.ParseDuration(*v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %q is not a duration (e.g. \"30s\", \"2m\")", key, *v)
	}
	return d, nil
}
