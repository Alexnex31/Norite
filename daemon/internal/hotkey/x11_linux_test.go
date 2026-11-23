// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// freeDisplay is a display number nothing uses in any of the three ways an X server is found: its socket's
// path, its lock file, and Linux's abstract socket of the same name. Xvfb's own -displayfd picked :0 here,
// whose path socket was the developer's XWayland and whose abstract socket was free, and so listened where a
// program on the developer's desktop would look first, and the binding, which dials the path first, reached
// XWayland instead.
func freeDisplay(t *testing.T) int {
	t.Helper()
	for n := 100; n < 200; n++ {
		socket := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
		if _, err := os.Lstat(socket); err == nil {
			continue
		}
		if _, err := os.Lstat(fmt.Sprintf("/tmp/.X%d-lock", n)); err == nil {
			continue
		}
		if c, err := net.Dial("unix", "@"+socket); err == nil {
			_ = c.Close()
			continue
		}
		return n
	}
	t.Skip("no free X display between :100 and :199")
	return 0
}

// xvfb starts an X server of the test's own, never the developer's, on a display nothing else uses and on
// its path socket alone, and returns its display and the Xauthority file holding its cookie, as a display
// manager writes one. XAUTHORITY names that file for the test's other programs, which xgb's own connections
// read. The server goes with the test.
func xvfb(t *testing.T) (display, auth string) {
	t.Helper()
	path, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skip("Xvfb is not installed: the grab is walked by hand on X11 (M35's manual pass)")
	}
	auth = filepath.Join(t.TempDir(), "Xauthority")
	cookie := make([]byte, 16)
	_, _ = rand.Read(cookie)
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint16(65535)) // FamilyWild: any host, any display
	for _, f := range [][]byte{nil, nil, []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		_ = binary.Write(&b, binary.BigEndian, uint16(len(f)))
		b.Write(f)
	}
	require.NoError(t, os.WriteFile(auth, b.Bytes(), 0o600))
	t.Setenv("XAUTHORITY", auth)
	r, w, err := os.Pipe()
	require.NoError(t, err)
	n := freeDisplay(t)
	// -noreset: an X server resets when its last client goes, which a desktop never lets happen, and a
	// connection made meanwhile is reset with it.
	cmd := exec.Command(path, fmt.Sprintf(":%d", n), "-displayfd", "3", "-noreset", "-nolisten", "tcp", "-nolisten", "local",
		"-auth", auth, "-screen", "0", "640x480x24")
	cmd.ExtraFiles = []*os.File{w}
	require.NoError(t, cmd.Start())
	_ = w.Close()
	// Stopped with SIGTERM and waited for, so it removes its own socket and lock: killed, it left them, and a
	// hundred runs used every display this test looks at. Killed only if it does not go.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = r.Close()
	})
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case got := <-line:
		require.Equal(t, fmt.Sprint(n), got, "Xvfb is on another display than the one asked for")
		return ":" + got, auth
	case <-time.After(10 * time.Second):
		t.Fatal("Xvfb did not start")
		return "", ""
	}
}

// recorder is a binding's reports, as they came.
type recorder struct {
	mu       sync.Mutex
	statuses []Status
	held     []bool
}

func (r *recorder) events() Events {
	return Events{
		Status: func(s Status) { r.mu.Lock(); r.statuses = append(r.statuses, s); r.mu.Unlock() },
		Held:   func(d bool) { r.mu.Lock(); r.held = append(r.held, d); r.mu.Unlock() },
	}
}

func (r *recorder) last() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.statuses) == 0 {
		return Status{}
	}
	return r.statuses[len(r.statuses)-1]
}

func (r *recorder) heldSoFar() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.held...)
}

// x11 binds key on display as the Linux backend would once it found no portal, and returns its reports and
// what releases it.
func x11(t *testing.T, display, auth, key string) (*recorder, func()) {
	t.Helper()
	k, err := ParseKey(key)
	require.NoError(t, err)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bindX11(ctx, map[string]string{"DISPLAY": display, "XAUTHORITY": auth}, k, rec.events(),
			"no portal, in a test")
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	require.Eventually(t, func() bool { return rec.last().State != "" }, 10*time.Second, 5*time.Millisecond)
	return rec, stop
}

// keyboard is another program on the display: a window of its own with the input focus, which is what the
// key must work in spite of, and XTEST's fake keyboard.
type keyboard struct {
	t    *testing.T
	conn *xgb.Conn
	code func(xproto.Keysym) (xproto.Keycode, bool)
	root xproto.Window
}

func newKeyboard(t *testing.T, display string) *keyboard {
	t.Helper()
	conn, err := xgb.NewConnDisplay(display)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	require.NoError(t, xtest.Init(conn))
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	w, err := xproto.NewWindowId(conn)
	require.NoError(t, err)
	require.NoError(t, xproto.CreateWindowChecked(conn, 0, w, root, 0, 0, 200, 100, 0,
		xproto.WindowClassInputOutput, 0, xproto.CwEventMask,
		[]uint32{xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease}).Check())
	require.NoError(t, xproto.MapWindowChecked(conn, w).Check())
	require.NoError(t, xproto.SetInputFocusChecked(conn, xproto.InputFocusParent, w, xproto.TimeCurrentTime).Check())
	codes, err := keycodes(conn)
	require.NoError(t, err)
	return &keyboard{t: t, conn: conn, code: codes, root: root}
}

func (k *keyboard) key(sym xproto.Keysym, down bool) {
	k.t.Helper()
	code, ok := k.code(sym)
	require.True(k.t, ok, "the test keyboard has no keysym %#x", sym)
	typ := byte(xproto.KeyRelease)
	if down {
		typ = xproto.KeyPress
	}
	require.NoError(k.t, xtest.FakeInputChecked(k.conn, typ, byte(code), 0, k.root, 0, 0, 0).Check())
}

const (
	keysymControlL = 0xffe3
	keysymShiftL   = 0xffe1
)

// hold presses ctrl+shift+space, holds it for d, and lets it go, the modifiers last.
func (k *keyboard) hold(d time.Duration) {
	k.key(keysymControlL, true)
	k.key(keysymShiftL, true)
	k.key(keysymSpace, true)
	time.Sleep(d)
	k.key(keysymSpace, false)
	k.key(keysymShiftL, false)
	k.key(keysymControlL, false)
}

// With another window focused, a hold long enough for X's autorepeat is one press and one release, and
// so with NumLock on, which a grab made without it does not match.
func TestAGrabOnXHearsTheKeyWhateverHasTheFocus(t *testing.T) {
	display, auth := xvfb(t)
	rec, _ := x11(t, display, auth, "ctrl+shift+space")
	require.Equal(t, Status{State: ipc.VoiceHotkeyRegistered}, rec.last())
	kb := newKeyboard(t, display)

	kb.hold(700 * time.Millisecond)
	require.Eventually(t, func() bool { return len(rec.heldSoFar()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, []bool{true, false}, rec.heldSoFar())

	kb.key(keysymNumLock, true)
	kb.key(keysymNumLock, false)
	kb.hold(100 * time.Millisecond)
	require.Eventually(t, func() bool { return len(rec.heldSoFar()) >= 4 }, 5*time.Second, 5*time.Millisecond,
		"with NumLock on, the key was not heard: %v", rec.heldSoFar())
	require.Equal(t, []bool{true, false, true, false}, rec.heldSoFar())
}

// A grab is exclusive: a second program asking for the key is told another holds it, and once the first
// lets it go, the key is anybody's again.
func TestAGrabOnXIsTheOneProgramsAlone(t *testing.T) {
	display, auth := xvfb(t)
	_, stop := x11(t, display, auth, "ctrl+shift+space")
	second, _ := x11(t, display, auth, "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyUnavailable, second.last().State)
	require.Contains(t, second.last().Reason, "another program on this X display holds the key")
	require.Contains(t, second.last().Reason, "no portal, in a test", "the refusal does not say why X was tried")
	stop()
	third, _ := x11(t, display, auth, "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyRegistered, third.last().State, "%+v", third.last())
}

// A display not on this machine, which an SSH session forwards, is refused before anything is dialed.
func TestAnXDisplayElsewhereIsRefused(t *testing.T) {
	for _, d := range []string{"localhost:10.0", "host:0", "", ":", ":x", ":01"} {
		_, ok := localDisplay(d)
		require.False(t, ok, "%q", d)
	}
	n, ok := localDisplay(":1.0")
	require.True(t, ok)
	require.Equal(t, 1, n)
}

// The cookie for this display, local to this host or wild, out of an Xauthority file in Xauth.h's form.
func TestTheXCookieIsTheDisplaysOwn(t *testing.T) {
	entry := func(family uint16, addr, disp, name string, data []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, family)
		for _, f := range [][]byte{[]byte(addr), []byte(disp), []byte(name), data} {
			_ = binary.Write(&b, binary.BigEndian, uint16(len(f)))
			b.Write(f)
		}
		return b.Bytes()
	}
	cookie := bytes.Repeat([]byte{0xab}, 16)
	other := bytes.Repeat([]byte{0x01}, 16)
	file := append(append(append(
		entry(256, "otherhost", "0", "MIT-MAGIC-COOKIE-1", other),
		entry(256, "here", "1", "MIT-MAGIC-COOKIE-1", other)...),
		entry(256, "here", "0", "XDM-AUTHORIZATION-1", other)...),
		entry(256, "here", "0", "MIT-MAGIC-COOKIE-1", cookie)...)
	got, err := readCookie(bytes.NewReader(file), "here", "0")
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("ab", 16), got)
	got, err = readCookie(bytes.NewReader(entry(65535, "", "", "MIT-MAGIC-COOKIE-1", cookie)), "here", "3")
	require.NoError(t, err, "a wild entry is every host's and display's")
	require.Equal(t, strings.Repeat("ab", 16), got)
	_, err = readCookie(bytes.NewReader(file[:len(file)-3]), "here", "0")
	require.Error(t, err, "a file cut short")
}

// A display whose session holds no cookie for it is refused, rather than tried with whatever this process's
// own environment names, which for a daemon started before the login is nothing or a stale file.
func TestAnXDisplayWithNoCookieIsRefused(t *testing.T) {
	display, _ := xvfb(t)
	rec, _ := x11(t, display, filepath.Join(t.TempDir(), "none"), "ctrl+shift+space")
	require.Equal(t, ipc.VoiceHotkeyUnavailable, rec.last().State)
	require.Contains(t, rec.last().Reason, "no authorization for display")
}
