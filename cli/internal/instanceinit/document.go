// Package instanceinit implements `norite instance init`, the self-hosted operator's first-run setup flow.
//
// The wizard's output is the instance config file the backend reads at startup. The two live in separate
// Go modules — the CLI cannot import backend/internal/config — so the shared contract is the reference
// document at contracts/instance-config.toml: the backend proves it loads that file, and this package
// proves it writes only keys that appear in it (see document_test.go). That pair is what keeps the two
// sides from drifting apart.
//
// See docs/architecture.md §4 for the design, including why the wizard lives in the client rather than
// the server binary and why its prompts are plain sequential I/O rather than a full-screen TUI.
package instanceinit

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// FileMode is the permission the config file is created with.
//
// The document holds the database password, and an S3 secret key when object storage is used. 0600 is not
// advisory here: writeDocument refuses to produce a file it cannot restrict to the owner, because a
// world-readable credential store that looks like it worked is worse than a failed setup run.
const FileMode fs.FileMode = 0o600

// Document is the fully-resolved set of answers the wizard writes out.
//
// It is deliberately a flat value with no pointers or optionality: by the time a Document exists every
// question has an answer, whether it came from a prompt, a flag, or a default. Deciding what to ask is
// the wizard's job (wizard.go); this type only knows how to become a file.
type Document struct {
	Env        string
	ListenAddr string

	DatabaseURL string

	StorageBackend   string
	StorageLocalPath string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  bool

	ACMEEnabled bool
	ACMEDomain  string
	ACMEEmail   string

	RegistrationMode string
}

// UsesS3 reports whether the S3 section is meaningful for this document.
func (d Document) UsesS3() bool { return d.StorageBackend == storageS3 }

const (
	storageLocal = "local"
	storageS3    = "s3"

	registrationOpen   = "open"
	registrationInvite = "invite"

	envDevelopment = "development"
	envProduction  = "production"
)

// documentTemplate renders the config file.
//
// A template rather than a struct marshal, for one reason: the generated file is meant to be hand-edited
// afterwards, and a TOML marshaller produces keys with no explanation of what they do or what happens if
// you change them. The comments are the point. Sections that do not apply to the chosen answers are left
// out entirely rather than emitted empty, so the file describes this instance instead of every instance.
//
// Only the keys documented in contracts/instance-config.toml may appear here — the backend rejects
// unknown keys outright, so an invented key would turn a successful wizard run into a server that refuses
// to start.
var documentTemplate = template.Must(template.New("instance.toml").Funcs(template.FuncMap{
	"toml": tomlString,
}).Parse(`# Norite instance configuration
#
# Written by ` + "`norite instance init`" + `. Safe to hand-edit afterwards — this is a plain TOML file, and the
# backend validates it at startup, so a mistake here fails immediately with a message naming the setting
# rather than surfacing later as strange behavior.
#
# This file contains credentials. It is created with 0600 permissions; keep it that way.
#
# Precedence, highest first: NORITE_* environment variable, then this file, then the built-in default.
# An environment variable always wins over anything written here.
#
# Every key the backend understands is documented in contracts/instance-config.toml. Settings this file
# does not mention simply keep their defaults — adding them here later is always safe.

# "development" or "production". Only changes cross-cutting defaults (log format), never behavior.
env = {{ .Env | toml }}

[http]
# Address the backend listens on.
listen_addr = {{ .ListenAddr | toml }}

[database]
# Postgres connection string. Carries the password — see the 0600 note above.
url = {{ .DatabaseURL | toml }}

[storage]
# Where uploaded attachments live: "local" disk or "s3" (AWS S3, MinIO, or anything speaking that API).
backend = {{ .StorageBackend | toml }}
{{- if not .UsesS3 }}

# Directory attachments are written to.
local_path = {{ .StorageLocalPath | toml }}
{{- end }}
{{- if .UsesS3 }}

[storage.s3]
{{- if .S3Endpoint }}
# Base URL of the S3-compatible service. Empty for AWS S3 itself, where the region picks the endpoint.
endpoint = {{ .S3Endpoint | toml }}
{{- end }}
region = {{ .S3Region | toml }}
bucket = {{ .S3Bucket | toml }}

# secret_access_key is a credential, like the database password above.
access_key_id = {{ .S3AccessKeyID | toml }}
secret_access_key = {{ .S3SecretAccessKey | toml }}

# Address buckets as endpoint/bucket rather than bucket.endpoint. MinIO and most self-hosted S3
# implementations need this; AWS S3 does not.
force_path_style = {{ .S3ForcePathStyle }}
{{- end }}

[acme]
# Let the backend provision and renew its own TLS certificate. Leave this off for a LAN-only instance, or
# when a reverse proxy, an Ingress, or anything else already terminates TLS in front of it.
enabled = {{ .ACMEEnabled }}
{{- if .ACMEEnabled }}

# The hostname a certificate is issued for, and where the CA sends expiry notices.
domain = {{ .ACMEDomain | toml }}
email = {{ .ACMEEmail | toml }}
{{- end }}

[registration]
# "open" (anyone may create an account) or "invite" (an instance invite code is required).
mode = {{ .RegistrationMode | toml }}
`))

// Render produces the file's contents.
func (d Document) Render() (string, error) {
	var sb strings.Builder
	if err := documentTemplate.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("rendering instance config: %w", err)
	}
	return sb.String(), nil
}

// Write renders the document to path, creating parent directories as needed.
//
// force controls whether an existing file may be replaced. Refusing by default is deliberate: re-running
// the wizard on a configured instance is an easy mistake, and silently overwriting would discard hand
// edits — including, on a real deployment, the working database credentials.
func (d Document) Write(path string, force bool) error {
	body, err := d.Render()
	if err != nil {
		return err
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 0700: the directory holds a credential file, so it should not be listable by other users.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	if !force {
		// Reserve the name before doing any work. O_EXCL makes "does it already exist?" the kernel's
		// decision at the moment of creation, rather than a separate Stat another process could
		// invalidate in between. The empty file it leaves is replaced by the rename below.
		reserved, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode) //nolint:gosec // operator's chosen destination
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s already exists; pass --force to replace it", path)
			}
			return fmt.Errorf("creating %s: %w", path, err)
		}
		if err := reserved.Close(); err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}

		// If the real write then fails, take the placeholder with it. Leaving it behind would make the
		// obvious next step — run the command again — report "already exists; pass --force", sending the
		// operator to force past a file that only exists because the previous attempt failed.
		if err := writeAtomically(path, body); err != nil {
			_ = os.Remove(path)
			return err
		}
		return nil
	}

	return writeAtomically(path, body)
}

// writeAtomically replaces path's contents in one step, or leaves them entirely untouched.
//
// Writing in place would mean a full disk or a lost power rail could truncate a working configuration
// halfway through — and on a real deployment this file holds the only copy of the database credentials,
// so a half-written one is an instance that cannot start and an operator with nothing to restore from.
// Staging into a sibling file and renaming makes the swap atomic: readers see either the old file or the
// complete new one.
func writeAtomically(path, body string) error {
	dir := filepath.Dir(path)

	// Same directory, so the rename stays within one filesystem — across a mount boundary it would fall
	// back to a copy and lose the atomicity this exists for.
	tmp, err := os.CreateTemp(dir, ".instance-*.toml")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmpName := tmp.Name()

	// Any failure from here on leaves the destination as it was, and takes the staging file with it.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// CreateTemp already makes the file 0600, but say so explicitly rather than depending on that: this
	// file holds a password from the moment the first byte lands.
	if err := tmp.Chmod(FileMode); err != nil {
		return fmt.Errorf("securing %s to %#o: %w", tmpName, FileMode, err)
	}
	if _, err := tmp.WriteString(body); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// Get the bytes to disk before the rename publishes the name. Without this, a crash right after the
	// rename can leave the new name pointing at empty content on some filesystems.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// tomlString renders a Go string as a TOML basic string.
//
// Hand-written rather than borrowed from the TOML library because the template emits the document
// directly: an unescaped quote or backslash in a password would produce a file that either fails to parse
// or, worse, parses into something other than what the operator typed.
func tomlString(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 2)
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			// TOML forbids raw control characters in basic strings; everything else goes through as-is so
			// non-ASCII passwords and paths survive unmangled.
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&sb, `\u%04X`, r)
				continue
			}
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}
