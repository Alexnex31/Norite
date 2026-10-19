// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

// Linux only, for the reason handles_linux_test.go is: the state directory comes from XDG_STATE_HOME, and
// the daemon replaces its own process, which is the Unix half of how an update ends.

//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexnex31/Norite/daemon/installreceipt"
	"github.com/Alexnex31/Norite/daemon/ipc"
)

// buildTestRelease builds this package as a given version with the norite_testrelease tag: a daemon that
// takes its releases from a host this test runs, and is otherwise the code a release runs.
func buildTestRelease(t *testing.T, dir, version string) string {
	t.Helper()
	binary := filepath.Join(dir, "norite-daemon-"+version)
	build := exec.Command("go", "build", "-tags", "norite_testrelease", "-ldflags", "-X main.Version="+version, "-o", binary, ".") //nolint:gosec // fixed arguments and a version this test chose
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the daemon as %s: %v\n%s", version, err, out)
	}
	return binary
}

// releaseHost is a release host a test fills: a listing, and each release's three files.
type releaseHost struct {
	t      *testing.T
	server *httptest.Server
	key    ed25519.PrivateKey

	mu    sync.Mutex
	tags  []string
	files map[string][]byte
}

func newReleaseHost(t *testing.T) *releaseHost {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &releaseHost{t: t, key: key, files: map[string][]byte{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.URL.Path == "/releases" {
			// Never nil: with nothing published yet that is `null`, which is not a listing, and the wait
			// below for a first check passed on one that had failed for as long as a failure was dated.
			listed := []map[string]string{}
			for _, tag := range h.tags {
				listed = append(listed, map[string]string{"tag_name": tag})
			}
			_ = json.NewEncoder(w).Encode(listed)
			return
		}
		body, ok := h.files[strings.TrimPrefix(r.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(h.server.Close)
	return h
}

// publish offers version, whose daemon is the program at daemon. signed is whether its checksums carry the
// test key's signature, or somebody else's.
func (h *releaseHost) publish(version, daemon string, signed bool) {
	h.t.Helper()
	program, err := os.ReadFile(daemon) //nolint:gosec // a binary this test built
	if err != nil {
		h.t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range map[string][]byte{"norite": []byte("#!/bin/sh\necho " + version + "\n"), "norite-daemon": program} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			h.t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		h.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		h.t.Fatal(err)
	}
	tag := "v" + version
	name := fmt.Sprintf("norite_%s_linux_%s.tar.gz", version, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	checksums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	key := h.key
	if !signed {
		// A release that is whole and consistent, signed by a key that is not the one believed.
		if _, key, err = ed25519.GenerateKey(rand.Reader); err != nil {
			h.t.Fatal(err)
		}
	}
	sig := ed25519.Sign(key, append([]byte(tag+"\n"), checksums...))

	h.mu.Lock()
	defer h.mu.Unlock()
	h.tags = append(h.tags, tag)
	h.files[tag+"/"+name] = archive.Bytes()
	h.files[tag+"/checksums.txt"] = checksums
	h.files[tag+"/checksums.txt.sigstore.json"] = []byte(base64.StdEncoding.EncodeToString(sig))
}

// only stops offering every release but the ones named.
func (h *releaseHost) only(tags ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tags = tags
}

// TestAnUpdateIsInstalledRefusedAndRolledBack is M24's done-when with real daemon binaries: a signed
// release installs and the daemon is running it; an unsigned one, and then an older one, are each refused
// and the version that was running keeps running; and a release that crashes at start is undone, the previous
// version runs again, and the bad one is not offered a second time.
//
// The releases are this package built four times as four versions, with the norite_testrelease tag, which
// swaps the release host and the signer for this test's and nothing else. The real signer is tested where
// it can be: against the published alpha, and against an authority made in the updater's own tests.
func TestAnUpdateIsInstalledRefusedAndRolledBack(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the daemon four times")
	}
	built := t.TempDir()
	versions := map[string]string{}
	for _, v := range []string{"0.1.0", "0.1.1", "0.1.2", "0.1.3"} {
		versions[v] = buildTestRelease(t, built, v)
	}

	// A short home: a Unix socket's path is bounded.
	home, err := os.MkdirTemp("/tmp", "nu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	stateDir, binDir := filepath.Join(home, "s", "norite"), filepath.Join(home, "bin")
	for _, dir := range []string{stateDir, binDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// What the installer leaves: the two programs, and a receipt saying where.
	installedDaemon := filepath.Join(binDir, "norite-daemon")
	copyProgram(t, versions["0.1.0"], installedDaemon)
	if err := os.WriteFile(filepath.Join(binDir, "norite"), []byte("#!/bin/sh\necho 0.1.0\n"), 0o755); err != nil { //nolint:gosec // a stand-in program
		t.Fatal(err)
	}
	if err := installreceipt.Write(stateDir, installreceipt.Receipt{BinDir: binDir, By: "test"}); err != nil {
		t.Fatal(err)
	}

	host := newReleaseHost(t)
	env := []string{
		"HOME=" + home, "XDG_STATE_HOME=" + filepath.Join(home, "s"), "XDG_CONFIG_HOME=" + filepath.Join(home, "c"),
		"PATH=" + os.Getenv("PATH"),
		"NORITE_TESTRELEASE_HOST=" + host.server.URL,
		"NORITE_TESTRELEASE_KEY=" + base64.StdEncoding.EncodeToString(host.key.Public().(ed25519.PublicKey)),
		"NORITE_TESTRELEASE_FAST=1",
		"NORITE_TESTRELEASE_CRASH=0.1.3",
	}

	// The service manager: it starts the installed program, and starts it again when it exits non-zero,
	// as the systemd unit and the Windows task do. An exit of 0 is a daemon that was asked to stop.
	var (
		stderr lockedBuffer
		procMu sync.Mutex
		starts int
		pid    int // the running daemon's, or 0 between two of them
	)
	supervised, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for supervised.Err() == nil {
			cmd := exec.Command(installedDaemon, "-stderr-log=false") //nolint:gosec // the program this test installed
			cmd.Env = env
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				return
			}
			procMu.Lock()
			starts, pid = starts+1, cmd.Process.Pid
			procMu.Unlock()
			err := cmd.Wait()
			procMu.Lock()
			pid = 0
			procMu.Unlock()
			if err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		stop()
		procMu.Lock()
		running := pid
		procMu.Unlock()
		if running > 1 {
			// By its own id, and only that: the process this test started.
			if p, err := os.FindProcess(running); err == nil {
				_ = p.Kill()
			}
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})

	socket := ipc.SocketPath(stateDir)
	readLog := func() string {
		body, _ := os.ReadFile(filepath.Join(stateDir, "daemon.log"))
		return string(body)
	}
	fail := func(format string, args ...any) {
		t.Helper()
		t.Logf("the daemon's stderr:\n%s", stderr.String())
		t.Logf("the daemon's log:\n%s", readLog())
		t.Fatalf(format, args...)
	}
	// status asks the running daemon where updating stands, or reports that none answered.
	status := func() (ipc.UpdateStatus, bool) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		conn, err := ipc.DialAt(ctx, socket)
		if err != nil {
			return ipc.UpdateStatus{}, false
		}
		client, err := ipc.Attach(ctx, conn, ipc.Options{Client: "norite-test", Version: "dev"})
		if err != nil {
			return ipc.UpdateStatus{}, false
		}
		defer func() { _ = client.Close() }()
		res, err := client.Do(ctx, "GET", ipc.PathUpdate, nil)
		if err != nil || res.Status != 200 {
			return ipc.UpdateStatus{}, false
		}
		var st ipc.UpdateStatus
		return st, json.Unmarshal(res.Body, &st) == nil
	}
	// until waits for the running daemon's status to satisfy cond.
	until := func(what string, cond func(ipc.UpdateStatus) bool) ipc.UpdateStatus {
		t.Helper()
		var last ipc.UpdateStatus
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if st, ok := status(); ok {
				last = st
				if cond(st) {
					return st
				}
			}
		}
		fail("waiting for %s; the last status was %+v", what, last)
		return last
	}

	// ---- nothing newer ----
	st := until("the daemon to be up and to have checked", func(s ipc.UpdateStatus) bool { return s.Version == "0.1.0" && s.LastCheck != "" })
	if !st.Installs || st.Mode != ipc.UpdateAuto || st.Available != "" {
		fail("the installed daemon: %+v", st)
	}

	// ---- an unsigned release is refused and 0.1.0 keeps running ----
	host.publish("0.1.2", versions["0.1.2"], false)
	st = until("the unsigned release to be refused", func(s ipc.UpdateStatus) bool { return strings.Contains(s.LastError, "did not verify") })
	if st.Version != "0.1.0" || st.Trial != "" {
		fail("after an unsigned release: %+v", st)
	}
	if got := programOf(t, installedDaemon); got != programOf(t, versions["0.1.0"]) {
		fail("an unsigned release changed the installed daemon")
	}
	host.only()

	// ---- a signed release installs, and the daemon is running it ----
	procMu.Lock()
	pidBefore, startsBefore := pid, starts
	procMu.Unlock()
	host.publish("0.1.1", versions["0.1.1"], true)
	st = until("0.1.1 to be running", func(s ipc.UpdateStatus) bool { return s.Version == "0.1.1" })
	if st.Trial != "0.1.1" {
		fail("just installed, and not on trial: %+v", st)
	}
	procMu.Lock()
	samePid, sameStarts := pid == pidBefore, starts == startsBefore
	procMu.Unlock()
	if !samePid || !sameStarts {
		fail("the daemon was restarted by its service manager to update; it should have replaced itself")
	}
	// It stays up, and is kept.
	until("0.1.1 to be confirmed", func(s ipc.UpdateStatus) bool { return s.Version == "0.1.1" && s.Trial == "" })
	if _, err := os.Stat(filepath.Join(stateDir, "update", "previous")); !os.IsNotExist(err) {
		fail("the previous pair is still kept after the new version was confirmed")
	}

	// ---- an older release, validly signed, is refused and 0.1.1 keeps running ----
	// 0.1.0 itself: the same minor version, so the instance rule has nothing to say, its daemon reports
	// the version it is offered as, and only the order of the two versions stands in the way.
	host.only()
	host.publish("0.1.0", versions["0.1.0"], true)
	time.Sleep(3 * time.Second) // several checks
	if st, ok := status(); !ok || st.Version != "0.1.1" || st.Available != "" || st.Trial != "" {
		fail("after an older release was offered: %+v (answered %v)", st, ok)
	}

	// ---- a release that crashes at start is undone ----
	host.publish("0.1.3", versions["0.1.3"], true)
	st = until("0.1.3 to be undone and 0.1.1 running again", func(s ipc.UpdateStatus) bool {
		return s.Version == "0.1.1" && s.RolledBack == "0.1.3"
	})
	if got := programOf(t, installedDaemon); got != programOf(t, versions["0.1.1"]) {
		fail("after the rollback the installed daemon is not 0.1.1")
	}
	if cli, _ := os.ReadFile(filepath.Join(binDir, "norite")); !strings.Contains(string(cli), "0.1.1") {
		fail("after the rollback the installed command is %q", cli)
	}
	log := readLog()
	for _, want := range []string{"the daemon's previous run crashed", "did not stay up after it was installed"} {
		if !strings.Contains(log, want) {
			fail("the log does not say %q", want)
		}
	}
	// And it is not installed a second time, however often the listing offers it.
	time.Sleep(3 * time.Second)
	if st, ok := status(); !ok || st.Version != "0.1.1" || st.Available != "" || st.Trial != "" {
		fail("0.1.3 was offered again after it was undone: %+v (answered %v)", st, ok)
	}
	if n := strings.Count(readLog(), "a release is installed; starting it"); n != 2 {
		fail("a release was installed %d times, want twice: 0.1.1 and 0.1.3", n)
	}
}

func copyProgram(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from) //nolint:gosec // a binary this test built
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0o755); err != nil { //nolint:gosec // a program
		t.Fatal(err)
	}
}

// programOf identifies a program by its contents.
func programOf(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a binary this test built
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// lockedBuffer is a buffer a running process writes to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
