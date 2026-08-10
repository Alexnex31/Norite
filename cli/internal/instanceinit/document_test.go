package instanceinit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contractPath is the reference document listing every key the backend understands. The backend proves it
// loads that file; this side proves the wizard writes nothing outside it. Between them, a key can't drift
// out of step without a test failing — the two live in separate Go modules and share no types.
const contractPath = "../../../contracts/instance-config.toml"

func localDocument() Document {
	return Document{
		Env:              envProduction,
		ListenAddr:       ":8080",
		DatabaseURL:      "postgres://norite:pw@localhost:5432/norite?sslmode=disable",
		StorageBackend:   storageLocal,
		StorageLocalPath: "/var/lib/norite/attachments",
		RegistrationMode: registrationOpen,
	}
}

func s3Document() Document {
	d := localDocument()
	d.StorageBackend = storageS3
	d.S3Endpoint = "https://minio.example.com"
	d.S3Region = "us-east-1"
	d.S3Bucket = "norite-attachments"
	d.S3AccessKeyID = "norite"
	d.S3SecretAccessKey = "s3cret"
	d.S3ForcePathStyle = true
	d.ACMEEnabled = true
	d.ACMEDomain = "chat.example.com"
	d.ACMEEmail = "admin@example.com"
	d.RegistrationMode = registrationInvite
	return d
}

// flattenKeys renders a parsed TOML document as sorted dotted paths, so two documents can be compared by
// the settings they mention rather than by their formatting.
func flattenKeys(t *testing.T, body string) []string {
	t.Helper()
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(body), &parsed), "document must be valid TOML")

	var keys []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if nested, ok := v.(map[string]any); ok {
				walk(path, nested)
				continue
			}
			keys = append(keys, path)
		}
	}
	walk("", parsed)
	sort.Strings(keys)
	return keys
}

func TestRenderProducesValidTOML(t *testing.T) {
	for name, doc := range map[string]Document{"local": localDocument(), "s3": s3Document()} {
		t.Run(name, func(t *testing.T) {
			body, err := doc.Render()
			require.NoError(t, err)

			var parsed map[string]any
			require.NoError(t, toml.Unmarshal([]byte(body), &parsed))
		})
	}
}

// The backend rejects unknown keys outright, so a key the wizard invents would turn a successful setup
// run into a server that refuses to start. Every key written must appear in the contract.
func TestWizardWritesOnlyContractKeys(t *testing.T) {
	contract, err := os.ReadFile(contractPath)
	require.NoError(t, err)
	allowed := flattenKeys(t, string(contract))

	for name, doc := range map[string]Document{"local": localDocument(), "s3": s3Document()} {
		t.Run(name, func(t *testing.T) {
			body, err := doc.Render()
			require.NoError(t, err)

			for _, key := range flattenKeys(t, body) {
				assert.Contains(t, allowed, key,
					"%q is not in contracts/instance-config.toml, so the backend would reject it", key)
			}
		})
	}
}

// Sections that do not apply to the chosen answers are left out entirely, so the file describes this
// instance rather than every possible one.
func TestRenderOmitsInapplicableSections(t *testing.T) {
	local, err := localDocument().Render()
	require.NoError(t, err)
	assert.NotContains(t, local, "[storage.s3]", "a local-storage instance has no S3 section")
	assert.NotContains(t, local, "domain =", "ACME is off, so it has no domain")
	assert.Contains(t, local, "local_path =")

	remote, err := s3Document().Render()
	require.NoError(t, err)
	assert.Contains(t, remote, "[storage.s3]")
	assert.Contains(t, remote, "bucket =")
	assert.NotContains(t, remote, "local_path =", "an S3 instance has no local attachment directory")
	assert.Contains(t, remote, `domain = "chat.example.com"`)
}

// A password containing TOML metacharacters must survive the round trip exactly. Getting this wrong
// produces either a file that fails to parse or, worse, one that parses into a different password.
func TestRenderEscapesAwkwardValues(t *testing.T) {
	doc := localDocument()
	doc.DatabaseURL = `postgres://u:p"a\ss` + "\t" + `word@localhost:5432/norite`
	doc.StorageLocalPath = `C:\Program Files\Norite\attachments`

	body, err := doc.Render()
	require.NoError(t, err)

	var parsed struct {
		Database struct {
			URL string `toml:"url"`
		} `toml:"database"`
		Storage struct {
			LocalPath string `toml:"local_path"`
		} `toml:"storage"`
	}
	require.NoError(t, toml.Unmarshal([]byte(body), &parsed))
	assert.Equal(t, doc.DatabaseURL, parsed.Database.URL)
	assert.Equal(t, doc.StorageLocalPath, parsed.Storage.LocalPath)
}

func TestWriteCreatesAnOwnerOnlyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "nested", "instance.toml")

	require.NoError(t, localDocument().Write(path, false))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, FileMode, info.Mode().Perm(),
		"the file holds the database password and must not be readable by other users")
}

// Re-running the wizard on a configured instance is an easy mistake, and silently overwriting would
// discard the working credentials already in the file.
func TestWriteRefusesToClobberWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path, []byte("# hand-edited\n"), FileMode))

	err := localDocument().Write(path, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")

	body, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "# hand-edited\n", string(body), "the existing file must be left untouched")
}

func TestWriteReplacesWithForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance.toml")
	require.NoError(t, os.WriteFile(path, []byte("# stale\n"), 0o644))

	require.NoError(t, localDocument().Write(path, true))

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "stale")

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.Equal(t, FileMode, info.Mode().Perm(),
			"replacing a loosely-permissioned file must tighten it, not inherit its mode")
	}
}

func TestTOMLStringEscaping(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`with "quotes"`, `"with \"quotes\""`},
		{`back\slash`, `"back\\slash"`},
		{"tab\there", `"tab\there"`},
		{"new\nline", `"new\nline"`},
		{"bell\x07", `"bell\u0007"`},
		{"héllo", `"héllo"`},
	} {
		assert.Equal(t, tt.want, tomlString(tt.in), "input %q", tt.in)
	}
}

// The generated file is meant to be hand-edited later, so it has to explain itself.
func TestRenderedFileDocumentsItself(t *testing.T) {
	body, err := localDocument().Render()
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(body, "# Norite instance configuration"))
	assert.Contains(t, body, "0600", "the file must say why its permissions matter")
	assert.Contains(t, body, "NORITE_", "the file must say that environment variables override it")

	// Every section heading should be preceded by explanatory comments rather than dropped in bare.
	for _, section := range []string{"[http]", "[database]", "[storage]", "[acme]", "[registration]"} {
		assert.Contains(t, body, section, fmt.Sprintf("missing section %s", section))
	}
}

// A failed write must leave the previous configuration exactly as it was.
//
// On a real deployment this file holds the only copy of the database credentials, so a half-written one is
// an instance that cannot start and an operator with nothing to restore from. The staging file is created
// in the destination's directory, so making that directory unwritable is what a full disk looks like here.
func TestFailedWriteLeavesTheExistingFileIntact(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions and a non-root user to make a directory unwritable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.toml")
	original := "# the working configuration\n"
	require.NoError(t, os.WriteFile(path, []byte(original), FileMode))

	require.NoError(t, os.Chmod(dir, 0o500)) // read+execute: existing files readable, no new ones
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := localDocument().Write(path, true)
	require.Error(t, err, "an unwritable directory must fail rather than damage the file")

	body, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, original, string(body), "the previous configuration must survive a failed write")
}

// The staging file must not be left behind on either path.
func TestWriteLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, localDocument().Write(filepath.Join(dir, "instance.toml"), false))
	require.NoError(t, localDocument().Write(filepath.Join(dir, "instance.toml"), true))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the config file itself should remain")
	assert.Equal(t, "instance.toml", entries[0].Name())
}

// A failed first write must not leave a placeholder that makes the retry look like a clobber.
func TestFailedFirstWriteDoesNotBlockARetry(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions and a non-root user to make a directory unwritable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.toml")

	// Writable enough to create the reservation, then made read-only so the staging write fails.
	reserved, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	require.NoError(t, err)
	require.NoError(t, reserved.Close())
	require.NoError(t, os.Remove(path))

	require.NoError(t, os.Chmod(dir, 0o500))
	require.Error(t, localDocument().Write(path, false))
	require.NoError(t, os.Chmod(dir, 0o700))

	// The retry must behave like a first run, not report an existing file.
	err = localDocument().Write(path, false)
	require.NoError(t, err, "a retry after a failed write must not be blocked by a leftover placeholder")
	assert.FileExists(t, path)
}
