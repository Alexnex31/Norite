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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// The two programs a client archive holds (.goreleaser.yaml), which are one install.
	cliName    = "norite"
	daemonName = "norite-daemon"

	// stagedMode is a program's mode: the account's to run, nobody else's to change.
	stagedMode = 0o755
)

// Variables so the tests can reach them with files of a few bytes.
var (
	// maxProgram bounds one extracted program, and maxUnpacked everything read out of an archive on the
	// way to them, so an archive that expands without end is stopped by a number and not by the disk.
	maxProgram  int64 = 128 << 20
	maxUnpacked int64 = 512 << 20

	smokeTimeout = 10 * time.Second
)

// errDownload is a file of a release that could not be fetched: the host's or the network's failure, and
// not the release's.
var errDownload = errors.New("downloading")

// Staged is a release downloaded, verified, unpacked and run once: ready to be installed.
type Staged struct {
	Release Release
	// CLI and Daemon are the two programs, in the staging directory.
	CLI, Daemon string
}

// Stager turns a release into a Staged.
type Stager struct {
	Source Source
	// Verify says whether checksums is what the release workflow signed for the tag. Verifier.Verify.
	Verify func(checksums, bundle []byte, tag string) error
	// Dir is where a release is staged: a directory of the daemon's own, in its state directory.
	Dir string
	// GOOS and GOARCH name the archive this machine takes.
	GOOS, GOARCH string
	// smoke runs a staged daemon and returns the version it reports. Nil is the real run.
	smoke func(ctx context.Context, daemon string) (string, error)
}

// Stage downloads r, checks it, and leaves its two programs in the staging directory.
//
// The order is the trust: the signature over checksums.txt, then the archive against checksums.txt, then
// the two programs taken out by name, then the new daemon run once to say which version it is. Anything
// that fails removes what was downloaded and returns an error, and the caller goes on running what it has
// (ADR 0020: fail closed).
func (s *Stager) Stage(ctx context.Context, r Release) (staged Staged, err error) {
	dir := filepath.Join(s.Dir, r.Tag)
	// Whatever an earlier attempt left is not built on, and one release is staged at a time: a release
	// staged and never installed, because the daemon stopped first, is not kept beside the next one.
	if err := os.RemoveAll(s.Dir); err != nil {
		return Staged{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Staged{}, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	small, cancel := context.WithTimeout(ctx, requestTimeout)
	checksums, err := s.Source.get(small, s.Source.fileURL(r, "checksums.txt"), maxChecksums)
	if err != nil {
		cancel()
		return Staged{}, fmt.Errorf("%w checksums.txt: %w", errDownload, err)
	}
	bundle, err := s.Source.get(small, s.Source.fileURL(r, "checksums.txt.sigstore.json"), maxBundle)
	cancel()
	if err != nil {
		return Staged{}, fmt.Errorf("%w the signature bundle: %w", errDownload, err)
	}
	// Before anything checksums.txt vouches for is so much as downloaded.
	if err := s.Verify(checksums, bundle, r.Tag); err != nil {
		return Staged{}, err
	}

	archive := archiveName(r, s.GOOS, s.GOARCH)
	want, ok := checksumFor(checksums, archive)
	if !ok {
		return Staged{}, fmt.Errorf("%w: checksums.txt does not list %s", ErrNotVerified, archive)
	}
	archivePath := filepath.Join(dir, archive)
	if err := s.fetchArchive(ctx, r, archive, archivePath, want); err != nil {
		return Staged{}, err
	}

	staged = Staged{Release: r,
		CLI:    filepath.Join(dir, cliName+exeSuffix(s.GOOS)),
		Daemon: filepath.Join(dir, daemonName+exeSuffix(s.GOOS))}
	if err := unpack(archivePath, s.GOOS, map[string]string{
		cliName + exeSuffix(s.GOOS):    staged.CLI,
		daemonName + exeSuffix(s.GOOS): staged.Daemon,
	}); err != nil {
		return Staged{}, fmt.Errorf("unpacking %s: %w", archive, err)
	}
	_ = os.Remove(archivePath)

	// A program that cannot start cannot undo its own installation, so it is started once here, where
	// failing costs nothing. It is run only now: after the signature and the checksum.
	smoke := s.smoke
	if smoke == nil {
		smoke = reportedVersion
	}
	got, err := smoke(ctx, staged.Daemon)
	if err != nil {
		return Staged{}, fmt.Errorf("the new daemon did not run: %w", err)
	}
	if got != r.Version.String() {
		return Staged{}, fmt.Errorf("the new daemon says it is %q, and the release is %s", got, r.Version)
	}
	return staged, nil
}

// fetchArchive downloads the archive to path and checks its SHA-256 as it arrives.
func (s *Stager) fetchArchive(ctx context.Context, r Release, name, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = s.Source.download(ctx, s.Source.fileURL(r, name), maxArchive, io.MultiWriter(file, sum))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("%w %s: %w", errDownload, name, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s is not the file checksums.txt names", ErrNotVerified, name)
	}
	return nil
}

// archiveName is the client archive for a platform, as .goreleaser.yaml names it.
func archiveName(r Release, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return "norite_" + r.Version.String() + "_" + goos + "_" + goarch + ext
}

func exeSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// checksumFor finds a file's SHA-256 in a checksums.txt: lines of a hash, two spaces and a name, as
// sha256sum writes them. The name must be the whole name; a line that is anything else is passed over.
func checksumFor(checksums []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(checksums), "\n") {
		hash, file, ok := strings.Cut(strings.TrimRight(line, "\r"), "  ")
		if !ok || strings.TrimPrefix(file, "*") != name {
			continue
		}
		// Whatever is written there is compared with the hash the download actually has, so a line whose
		// hash is not one is a line that matches nothing.
		return strings.ToLower(hash), true
	}
	return "", false
}

// unpack takes the named files out of an archive and nothing else. wanted maps a name at the archive's
// top level to where it goes. Each must be there exactly once, as a regular file.
//
// Names are matched whole and never joined to a path, so an entry called ../../somewhere is simply not one
// of the names; nothing an archive says decides where a byte is written.
func unpack(archivePath, goos string, wanted map[string]string) error {
	found := map[string]bool{}
	take := func(name string, regular bool, content io.Reader) error {
		dest, ok := wanted[name]
		if !ok {
			return nil
		}
		if !regular {
			return fmt.Errorf("%s is not a regular file", name)
		}
		if found[name] {
			return fmt.Errorf("%s is in the archive twice", name)
		}
		found[name] = true
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, stagedMode)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(content, maxProgram+1))
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if n > maxProgram {
			return fmt.Errorf("%s: %w", name, ErrTooLarge)
		}
		return nil
	}

	var err error
	if goos == "windows" {
		err = unpackZip(archivePath, take)
	} else {
		err = unpackTarGz(archivePath, take)
	}
	if err != nil {
		return err
	}
	for name := range wanted {
		if !found[name] {
			return fmt.Errorf("the archive holds no %s", name)
		}
	}
	return nil
}

func unpackTarGz(path string, take func(name string, regular bool, content io.Reader) error) error {
	file, err := os.Open(path) //nolint:gosec // the staging directory's own file
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	// Everything read out of the archive, wanted or passed over, counts toward one bound.
	limited := &io.LimitedReader{R: compressed, N: maxUnpacked + 1}
	entries := tar.NewReader(limited)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if limited.N <= 0 {
			return ErrTooLarge
		}
		if err != nil {
			return err
		}
		if err := take(header.Name, header.Typeflag == tar.TypeReg, entries); err != nil {
			return err
		}
	}
}

func unpackZip(path string, take func(name string, regular bool, content io.Reader) error) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close() }()
	for _, entry := range archive.File {
		// Opened only when it is one of the names wanted, which take decides; an entry passed over is
		// never decompressed.
		err := func() error {
			var content io.Reader = bytes.NewReader(nil)
			regular := entry.Mode().IsRegular()
			if regular {
				opened, err := entry.Open()
				if err != nil {
					return err
				}
				defer func() { _ = opened.Close() }()
				content = opened
			}
			return take(entry.Name, regular, content)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// reportedVersion runs a daemon with -version and returns what it prints: the version it was built as.
// The environment is empty, so nothing of this daemon's reaches it, and it has a few seconds.
func reportedVersion(ctx context.Context, daemon string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, daemon, "-version") //nolint:gosec // a program whose signature and checksum verified
	cmd.Env = []string{}
	cmd.Dir = filepath.Dir(daemon)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, left: 256}
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// limitedWriter keeps the first bytes written to it and drops the rest, so a program that prints without
// end is not held in memory while its time runs out.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if keep := min(len(p), l.left); keep > 0 {
		_, _ = l.w.Write(p[:keep])
		l.left -= keep
	}
	return len(p), nil
}
