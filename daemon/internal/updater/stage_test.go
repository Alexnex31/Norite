// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// entry is one file in a test archive.
type entry struct {
	name    string
	body    string
	symlink bool
}

func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.symlink {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if !e.symlink {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// host is a release host for one release: its three files, and a count of how often each was asked for.
type host struct {
	server    *httptest.Server
	checksums string
	bundle    string
	archive   []byte
	asked     map[string]*atomic.Int32
}

// newHost serves tag's files. The checksums name the archive with its real hash unless a test changes
// h.checksums afterwards.
func newHost(t *testing.T, tag, archiveName string, archive []byte) *host {
	t.Helper()
	sum := sha256.Sum256(archive)
	h := &host{archive: archive, bundle: "a bundle",
		checksums: hex.EncodeToString(sum[:]) + "  " + archiveName + "\n",
		asked:     map[string]*atomic.Int32{"checksums.txt": {}, "checksums.txt.sigstore.json": {}, archiveName: {}}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/download/"+tag+"/")
		if n, ok := h.asked[name]; ok {
			n.Add(1)
		}
		switch name {
		case "checksums.txt":
			_, _ = fmt.Fprint(w, h.checksums)
		case "checksums.txt.sigstore.json":
			_, _ = fmt.Fprint(w, h.bundle)
		case archiveName:
			_, _ = w.Write(h.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

// stagerFor is a Stager on h, for linux/amd64, whose signature check and smoke run both pass.
func stagerFor(t *testing.T, h *host) *Stager {
	t.Helper()
	return &Stager{Source: sourceAt(h.server), Dir: t.TempDir(), GOOS: "linux", GOARCH: "amd64",
		Verify: func(_, _ []byte, _ string) error { return nil },
		smoke:  func(context.Context, string) (string, error) { return "0.1.1", nil }}
}

func mustTag(t *testing.T, tag string) Release {
	t.Helper()
	r, ok := parseTag(tag)
	if !ok {
		t.Fatalf("%q is not a tag", tag)
	}
	return r
}

const linuxArchive = "norite_0.1.1_linux_amd64.tar.gz"

func goodArchive(t *testing.T) []byte {
	return tarGz(t, entry{name: "LICENSE", body: "terms"}, entry{name: "norite", body: "the cli"},
		entry{name: "norite-daemon", body: "the daemon"}, entry{name: "README.md", body: "readme"})
}

// emptyStaging asserts a refused release left nothing behind.
func emptyStaging(t *testing.T, s *Stager) {
	t.Helper()
	left, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("a refused release left %d entries in the staging directory", len(left))
	}
}

func TestStageLeavesTheTwoProgramsAndNothingElse(t *testing.T) {
	h := newHost(t, "v0.1.1", linuxArchive, goodArchive(t))
	s := stagerFor(t, h)
	var verified struct {
		checksums, bundle, tag string
	}
	s.Verify = func(c, b []byte, tag string) error {
		verified.checksums, verified.bundle, verified.tag = string(c), string(b), tag
		return nil
	}
	staged, err := s.Stage(t.Context(), mustTag(t, "v0.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	// The signature was asked about the bytes downloaded, for this tag.
	if verified.checksums != h.checksums || verified.bundle != "a bundle" || verified.tag != "v0.1.1" {
		t.Errorf("verified %+v", verified)
	}
	for path, want := range map[string]string{staged.CLI: "the cli", staged.Daemon: "the daemon"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s holds %q (%v)", path, got, err)
		}
		if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o755) {
			t.Errorf("%s has mode %v", path, info.Mode())
		}
	}
	left, _ := os.ReadDir(filepath.Dir(staged.CLI))
	if len(left) != 2 {
		t.Errorf("the staging directory holds %d files, want the two programs", len(left))
	}
}

func TestStageTakesAZipOnWindows(t *testing.T) {
	archive := zipOf(t, entry{name: "norite.exe", body: "cli"}, entry{name: "norite-daemon.exe", body: "daemon"},
		entry{name: "LICENSE", body: "terms"})
	h := newHost(t, "v0.1.1", "norite_0.1.1_windows_arm64.zip", archive)
	s := stagerFor(t, h)
	s.GOOS, s.GOARCH = "windows", "arm64"
	staged, err := s.Stage(t.Context(), mustTag(t, "v0.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(staged.Daemon) != "norite-daemon.exe" || filepath.Base(staged.CLI) != "norite.exe" {
		t.Errorf("staged %s and %s", staged.CLI, staged.Daemon)
	}
}

func TestNothingIsDownloadedOnAnUnverifiedSignature(t *testing.T) {
	h := newHost(t, "v0.1.1", linuxArchive, goodArchive(t))
	s := stagerFor(t, h)
	s.Verify = func(_, _ []byte, _ string) error { return fmt.Errorf("%w: test", ErrNotVerified) }
	if _, err := s.Stage(t.Context(), mustTag(t, "v0.1.1")); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("an unverified release: %v", err)
	}
	if n := h.asked[linuxArchive].Load(); n != 0 {
		t.Errorf("the archive was downloaded %d times before its checksums verified", n)
	}
	emptyStaging(t, s)
}

func TestARefusedReleaseLeavesNothing(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(h *host, s *Stager)
		archive []byte
		want    error  // matched with errors.Is when set
		says    string // otherwise a piece of the message
	}{
		{name: "the archive is not the one checksums.txt names", want: ErrNotVerified,
			prepare: func(h *host, _ *Stager) { h.archive = append([]byte{}, h.archive...); h.archive[len(h.archive)-1] ^= 1 }},
		{name: "checksums.txt does not list this platform", want: ErrNotVerified,
			prepare: func(h *host, _ *Stager) { h.checksums = strings.Replace(h.checksums, "linux", "plan9", 1) }},
		{name: "a name that only ends with the archive's", want: ErrNotVerified,
			prepare: func(h *host, _ *Stager) { h.checksums = strings.Replace(h.checksums, "  norite_", "  evil-norite_", 1) }},
		{name: "a line whose hash is not one", want: ErrNotVerified,
			prepare: func(h *host, _ *Stager) { h.checksums = "zz" + h.checksums[2:] }},
		{name: "no signature bundle", says: "signature bundle",
			prepare: func(h *host, _ *Stager) {
				delete(h.asked, "checksums.txt.sigstore.json")
				h.bundle = strings.Repeat("x", maxBundle+1)
			}},
		{name: "no daemon in the archive", says: "holds no norite-daemon",
			archive: tarGz(t, entry{name: "norite", body: "cli"})},
		{name: "the daemon under a directory is not the daemon", says: "holds no norite-daemon",
			archive: tarGz(t, entry{name: "norite", body: "cli"}, entry{name: "bin/norite-daemon", body: "d"})},
		{name: "a path that climbs is not a name", says: "holds no norite",
			archive: tarGz(t, entry{name: "../norite", body: "cli"}, entry{name: "norite-daemon", body: "d"})},
		{name: "a program twice", says: "twice",
			archive: tarGz(t, entry{name: "norite", body: "a"}, entry{name: "norite", body: "b"}, entry{name: "norite-daemon", body: "d"})},
		{name: "a link where a program should be", says: "not a regular file",
			archive: tarGz(t, entry{name: "norite", body: "/bin/sh", symlink: true}, entry{name: "norite-daemon", body: "d"})},
		{name: "not an archive", says: "unpacking", archive: []byte("plain text")},
		{name: "the new daemon does not run", says: "did not run",
			prepare: func(_ *host, s *Stager) {
				s.smoke = func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
			}},
		{name: "the new daemon is another version", says: `says it is "0.1.0"`,
			prepare: func(_ *host, s *Stager) {
				s.smoke = func(context.Context, string) (string, error) { return "0.1.0", nil }
			}},
	}
	for _, tc := range cases {
		archive := tc.archive
		if archive == nil {
			archive = goodArchive(t)
		}
		h := newHost(t, "v0.1.1", linuxArchive, archive)
		s := stagerFor(t, h)
		if tc.prepare != nil {
			tc.prepare(h, s)
		}
		_, err := s.Stage(t.Context(), mustTag(t, "v0.1.1"))
		switch {
		case err == nil:
			t.Errorf("%s: staged", tc.name)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		case tc.says != "" && !strings.Contains(err.Error(), tc.says):
			t.Errorf("%s: %v, want it to say %q", tc.name, err, tc.says)
		}
		emptyStaging(t, s)
	}
}

func lower[T any](t *testing.T, v *T, to T) {
	t.Helper()
	old := *v
	*v = to
	t.Cleanup(func() { *v = old })
}

func TestAnArchiveIsBoundedInWhatItUnpacks(t *testing.T) {
	// One program larger than a program may be.
	lower(t, &maxProgram, 8)
	h := newHost(t, "v0.1.1", linuxArchive, tarGz(t, entry{name: "norite", body: "123456789"}, entry{name: "norite-daemon", body: "d"}))
	s := stagerFor(t, h)
	if _, err := s.Stage(t.Context(), mustTag(t, "v0.1.1")); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized program: %v", err)
	}
	emptyStaging(t, s)

	// A file nobody wants, in front of the two that are, longer than the whole archive may unpack to.
	lower(t, &maxProgram, 1<<20)
	lower(t, &maxUnpacked, 64<<10)
	h = newHost(t, "v0.1.1", linuxArchive, tarGz(t, entry{name: "padding", body: strings.Repeat("\x00", 256<<10)},
		entry{name: "norite", body: "c"}, entry{name: "norite-daemon", body: "d"}))
	s = stagerFor(t, h)
	if _, err := s.Stage(t.Context(), mustTag(t, "v0.1.1")); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an archive that unpacks past the bound: %v", err)
	}
	emptyStaging(t, s)
}

// The real run: a program is started, with an empty environment, and what it prints is its version.
func TestTheStagedDaemonIsRunOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in daemon is a shell script")
	}
	t.Setenv("NORITE_TEST_LEAK", "from the running daemon")
	script := "#!/bin/sh\n[ \"$1\" = -version ] || exit 9\n[ -z \"$NORITE_TEST_LEAK\" ] || exit 8\necho 0.1.1\n"
	h := newHost(t, "v0.1.1", linuxArchive, tarGz(t, entry{name: "norite", body: "c"}, entry{name: "norite-daemon", body: script}))
	s := stagerFor(t, h)
	s.smoke = nil
	if _, err := s.Stage(t.Context(), mustTag(t, "v0.1.1")); err != nil {
		t.Fatalf("a daemon that reports its version: %v", err)
	}

	// One that never answers is given up on, and one that prints without end is not held in memory.
	lower(t, &smokeTimeout, 300*time.Millisecond)
	for name, body := range map[string]string{
		"hangs":          "#!/bin/sh\nexec sleep 30\n",
		"prints forever": "#!/bin/sh\nwhile :; do echo 0.1.1; done\n",
		"exits non-zero": "#!/bin/sh\necho 0.1.1\nexit 3\n",
	} {
		h := newHost(t, "v0.1.1", linuxArchive, tarGz(t, entry{name: "norite", body: "c"}, entry{name: "norite-daemon", body: body}))
		s := stagerFor(t, h)
		s.smoke = nil
		started := time.Now()
		if _, err := s.Stage(t.Context(), mustTag(t, "v0.1.1")); err == nil || !strings.Contains(err.Error(), "did not run") {
			t.Errorf("%s: %v", name, err)
		}
		if time.Since(started) > 5*time.Second {
			t.Errorf("%s: took %s", name, time.Since(started))
		}
		emptyStaging(t, s)
	}
}

// What a staged daemon prints is kept to its first bytes: a version is short, and a program that prints
// without end is not held in memory while its time runs out.
func TestWhatAStagedDaemonPrintsIsCapped(t *testing.T) {
	var kept bytes.Buffer
	w := &limitedWriter{w: &kept, left: 16}
	for i := 0; i < 100; i++ {
		if n, err := w.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("a write past the cap must still be accepted, or the program blocks: %d, %v", n, err)
		}
	}
	if kept.String() != "0123456789012345" {
		t.Errorf("kept %q", kept.String())
	}
}
