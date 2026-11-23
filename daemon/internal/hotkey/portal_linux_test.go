// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// privateBus starts a D-Bus daemon of the test's own and returns its address. Its configuration names no
// service directory, so nothing on it can be activated: the developer's real xdg-desktop-portal, which a
// session configuration would start on the first call, could show a dialog on their desktop.
func privateBus(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon is not installed: the portal is walked by hand on Wayland (M35's manual pass)")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	require.NoError(t, os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=`+filepath.Join(dir, "bus")+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`), 0o600))
	r, w, err := os.Pipe()
	require.NoError(t, err)
	cmd := exec.Command(path, "--config-file="+conf, "--nofork", "--print-address=3")
	cmd.ExtraFiles = []*os.File{w}
	require.NoError(t, cmd.Start())
	_ = w.Close()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		_ = r.Close()
	})
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case addr := <-line:
		require.NotEmpty(t, addr)
		return addr
	case <-time.After(10 * time.Second):
		t.Fatal("dbus-daemon did not start")
		return ""
	}
}

func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	c, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fakePortal is a desktop portal as xdg-desktop-portal 1.19.4 and a desktop behind it answer: the host
// registry, GlobalShortcuts' session, listing and binding, each answered through a Request object's Response
// signal after the call returns, and the shortcut's signals when the test says. Like Plasma, it keeps what
// a binding decided for the sessions after it, which ListShortcuts names.
type fakePortal struct {
	t    *testing.T
	conn *dbus.Conn

	mu sync.Mutex
	// bindCode is what BindShortcuts responds, and bindTrigger what it says the key is ("" for none), both
	// set with answers.
	bindCode    uint32
	bindTrigger string
	app         string
	suggested   string
	session     dbus.ObjectPath
	closed      bool
	// binds counts BindShortcuts, and stored is what the last one bound, nil before any: the key the
	// desktop names, "" for none.
	binds  int
	stored *string
}

// newFakePortal owns the portal's name on addr. registry false plays a portal older than 1.19.4.
func newFakePortal(t *testing.T, addr string, registry bool) *fakePortal {
	t.Helper()
	p := &fakePortal{t: t, conn: connect(t, addr), bindTrigger: "Ctrl+Shift+Space"}
	require.NoError(t, p.conn.Export(p, portalPath, globalShortcuts))
	require.NoError(t, p.conn.Export(properties{}, portalPath, "org.freedesktop.DBus.Properties"))
	if registry {
		require.NoError(t, p.conn.Export(registryOf{p}, portalPath, hostRegistry))
	}
	reply, err := p.conn.RequestName(portalName, dbus.NameFlagDoNotQueue)
	require.NoError(t, err)
	require.Equal(t, dbus.RequestNameReplyPrimaryOwner, reply)
	return p
}

type registryOf struct{ p *fakePortal }

func (r registryOf) Register(app string, _ map[string]dbus.Variant) *dbus.Error {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if app != AppID {
		// What xdg-desktop-portal 1.20 answers for an app id with no .desktop file.
		return dbus.NewError(portalFailed, []any{fmt.Sprintf("Could not register app ID: App info not found for %q", app)})
	}
	r.p.app = app
	return nil
}

type properties struct{}

func (properties) Get(iface, prop string) (dbus.Variant, *dbus.Error) {
	if iface == globalShortcuts && prop == "version" {
		return dbus.MakeVariant(uint32(1)), nil
	}
	return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("no property %s.%s", iface, prop))
}

// respond answers the request sender made under token, a moment after its call returned.
func (p *fakePortal) respond(sender dbus.Sender, token string, code uint32, results map[string]dbus.Variant) dbus.ObjectPath {
	path := dbus.ObjectPath(fmt.Sprintf("%s/request/%s/%s", portalPath,
		strings.ReplaceAll(strings.TrimPrefix(string(sender), ":"), ".", "_"), token))
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = p.conn.Emit(path, portalRequest+".Response", code, results)
	}()
	return path
}

func (p *fakePortal) CreateSession(sender dbus.Sender, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	tok, _ := opts["handle_token"].Value().(string)
	stok, _ := opts["session_handle_token"].Value().(string)
	session := dbus.ObjectPath(fmt.Sprintf("%s/session/%s/%s", portalPath,
		strings.ReplaceAll(strings.TrimPrefix(string(sender), ":"), ".", "_"), stok))
	p.mu.Lock()
	p.session = session
	p.mu.Unlock()
	if err := p.conn.Export(sessionOf{p}, session, portalSession); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	return p.respond(sender, tok, 0, map[string]dbus.Variant{"session_handle": dbus.MakeVariant(string(session))}), nil
}

func (p *fakePortal) BindShortcuts(sender dbus.Sender, session dbus.ObjectPath, shortcuts []shortcut, _ string,
	opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	tok, _ := opts["handle_token"].Value().(string)
	p.mu.Lock()
	if len(shortcuts) == 1 && shortcuts[0].ID == shortcutID {
		p.suggested, _ = shortcuts[0].Options["preferred_trigger"].Value().(string)
	}
	code, trigger := p.bindCode, p.bindTrigger
	p.binds++
	if code == 0 {
		p.stored = &trigger
	}
	p.mu.Unlock()
	return p.respond(sender, tok, code, map[string]dbus.Variant{"shortcuts": described(trigger)}), nil
}

// ListShortcuts names what an earlier binding stored, as Plasma's does for a new session of the component.
func (p *fakePortal) ListShortcuts(sender dbus.Sender, session dbus.ObjectPath, opts map[string]dbus.Variant) (
	dbus.ObjectPath, *dbus.Error) {
	tok, _ := opts["handle_token"].Value().(string)
	p.mu.Lock()
	stored := p.stored
	p.mu.Unlock()
	list := dbus.MakeVariant([]shortcut{})
	if stored != nil {
		list = described(*stored)
	}
	return p.respond(sender, tok, 0, map[string]dbus.Variant{"shortcuts": list}), nil
}

// described is the portal's list holding Norite's shortcut, bound to trigger ("" for no key named).
func described(trigger string) dbus.Variant {
	given := map[string]dbus.Variant{"description": dbus.MakeVariant("Push to talk in a Norite call")}
	if trigger != "" {
		given["trigger_description"] = dbus.MakeVariant(trigger)
	}
	return dbus.MakeVariant([]shortcut{{ID: shortcutID, Options: given}})
}

// answers sets what BindShortcuts responds.
func (p *fakePortal) answers(code uint32, trigger string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bindCode, p.bindTrigger = code, trigger
}

type sessionOf struct{ p *fakePortal }

func (s sessionOf) Close() *dbus.Error {
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	s.p.closed = true
	return nil
}

// shortcut emits Activated or Deactivated for session's id.
func (p *fakePortal) shortcut(session dbus.ObjectPath, id string, down bool) {
	name := globalShortcuts + ".Deactivated"
	if down {
		name = globalShortcuts + ".Activated"
	}
	require.NoError(p.t, p.conn.Emit(portalPath, name, session, id, uint64(time.Now().UnixMilli()),
		map[string]dbus.Variant{}))
}

// bindings is how many times BindShortcuts was asked.
func (p *fakePortal) bindings() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.binds
}

func (p *fakePortal) state() (app, suggested string, session dbus.ObjectPath, closed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.app, p.suggested, p.session, p.closed
}

// portalBinding binds key through the portal on addr, as the Linux backend does, and returns its reports
// and what stops it and says what it returned.
func portalBinding(t *testing.T, addr, key string) (*recorder, func() error) {
	t.Helper()
	k, err := ParseKey(key)
	require.NoError(t, err)
	bus := connect(t, addr)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- bindPortal(ctx, bus, k, rec.events()) }()
	var once sync.Once
	var got error
	stop := func() error {
		once.Do(func() {
			cancel()
			got = <-result
		})
		return got
	}
	t.Cleanup(func() { _ = stop() })
	return rec, stop
}

// returned waits for a portal binding that returns by itself, a portal it does not use, and says what it
// returned.
func returned(t *testing.T, addr, key string) (*recorder, error) {
	t.Helper()
	k, err := ParseKey(key)
	require.NoError(t, err)
	rec := &recorder{}
	result := make(chan error, 1)
	go func() { result <- bindPortal(context.Background(), connect(t, addr), k, rec.events()) }()
	select {
	case err := <-result:
		return rec, err
	case <-time.After(10 * time.Second):
		t.Fatal("the binding went on with no portal to use")
		return nil, nil
	}
}

// Through the portal the key is suggested in the shortcuts specification's words, Norite is named by its
// app id first, the desktop's own words for the key come back, its press and release reach the call once
// each, and the session closes as the binding stops (M25: one press and one release on Plasma 6.6).
func TestThePortalBindsTheKeyAndSaysWhenItIsHeld(t *testing.T) {
	addr := privateBus(t)
	p := newFakePortal(t, addr, true)
	rec, stop := portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond, "%+v", rec.last())
	rec.mu.Lock()
	require.Equal(t, ipc.VoiceHotkeyWaiting, rec.statuses[0].State, "the desktop's question was not said")
	rec.mu.Unlock()
	require.Equal(t, "Ctrl+Shift+Space", rec.last().Trigger)
	app, suggested, session, _ := p.state()
	require.Equal(t, AppID, app)
	require.Equal(t, "CTRL+SHIFT+space", suggested)

	// Another session's shortcut, and another shortcut of this one, are nobody's key.
	p.shortcut("/org/freedesktop/portal/desktop/session/1_99/other", shortcutID, true)
	p.shortcut(session, "another", true)
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, rec.heldSoFar())
	p.shortcut(session, shortcutID, true)
	p.shortcut(session, shortcutID, true) // a desktop saying so twice
	p.shortcut(session, shortcutID, false)
	require.Eventually(t, func() bool { return len(rec.heldSoFar()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, []bool{true, false}, rec.heldSoFar())

	require.NoError(t, stop())
	_, _, _, closed := p.state()
	require.True(t, closed, "the portal's session outlived the binding")
}

// A portal older than 1.19.4 has no host registry, cannot be told Norite's app id, and is not used; with no
// portal at all, nothing answers. Either way nothing is reported, so X can be tried where the session is X's.
func TestAPortalThatCannotNameNoriteIsNotUsed(t *testing.T) {
	addr := privateBus(t)
	newFakePortal(t, addr, false)
	rec, err := returned(t, addr, "ctrl+shift+space")
	var none *noPortal
	require.ErrorAs(t, err, &none)
	require.Contains(t, none.why, "older than xdg-desktop-portal 1.19.4")
	require.Empty(t, rec.statuses)

	empty := privateBus(t)
	rec, err = returned(t, empty, "ctrl+shift+space")
	require.ErrorAs(t, err, &none)
	require.Contains(t, none.why, "no desktop portal answers")
	require.Empty(t, rec.statuses)
}

// What the desktop decides is said: a shortcut refused in its dialog, one bound with no key named, which
// Hyprland does since its keys are bound in its own configuration (M25), and a session it closes.
func TestWhatTheDesktopDecidesIsSaid(t *testing.T) {
	addr := privateBus(t)
	p := newFakePortal(t, addr, true)
	p.answers(1, "Ctrl+Shift+Space")
	rec, _ := portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyUnavailable }, 10*time.Second,
		5*time.Millisecond)
	require.Contains(t, rec.last().Reason, "refused")

	addr = privateBus(t)
	p = newFakePortal(t, addr, true)
	p.answers(0, "")
	rec, _ = portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond)
	require.Contains(t, rec.last().Reason, "bind Norite's push to talk in the desktop's settings")

	_, _, session, _ := p.state()
	require.NoError(t, p.conn.Emit(session, portalSession+".Closed", map[string]dbus.Variant{}))
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyUnavailable }, 5*time.Second,
		5*time.Millisecond)
	require.Contains(t, rec.last().Reason, "closed the shortcut")
}

// A desktop's words for the key are bounded where they are kept.
func TestTheDesktopsWordsForTheKeyAreBounded(t *testing.T) {
	long := strings.Repeat("é", maxTrigger)
	// As godbus decodes the portal's a(sa{sv}) off the wire.
	st := bound(dbus.MakeVariant([][]any{{shortcutID, map[string]dbus.Variant{
		"trigger_description": dbus.MakeVariant(long)}}}))
	require.NotEmpty(t, st.Trigger)
	require.LessOrEqual(t, len(st.Trigger), maxTrigger)
	require.True(t, strings.HasPrefix(long, st.Trigger), "cut off a character's boundary")
}

// A shortcut the desktop already binds to a key is taken from ListShortcuts, and BindShortcuts is not asked
// again: on Plasma 6.3 it opens System Settings each time, which M35's manual pass saw at every join. The
// key still reaches the call. One the desktop holds with no key is asked for again, which is the person's
// way back to a key.
func TestABoundShortcutIsNotAskedForAgain(t *testing.T) {
	addr := privateBus(t)
	p := newFakePortal(t, addr, true)
	rec, stop := portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond, "%+v", rec.last())
	require.NoError(t, stop())
	require.Equal(t, 1, p.bindings())

	rec, stop = portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond, "%+v", rec.last())
	require.Equal(t, Status{State: ipc.VoiceHotkeyRegistered, Trigger: "Ctrl+Shift+Space"}, rec.last())
	rec.mu.Lock()
	require.Len(t, rec.statuses, 1, "the desktop was asked again: %+v", rec.statuses)
	rec.mu.Unlock()
	require.Equal(t, 1, p.bindings(), "BindShortcuts was asked again")
	_, _, session, _ := p.state()
	p.shortcut(session, shortcutID, true)
	p.shortcut(session, shortcutID, false)
	require.Eventually(t, func() bool { return len(rec.heldSoFar()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, []bool{true, false}, rec.heldSoFar())
	require.NoError(t, stop())

	// The person took the key away in the desktop's settings: asked again.
	none := ""
	p.mu.Lock()
	p.stored = &none
	p.mu.Unlock()
	rec, stop = portalBinding(t, addr, "ctrl+shift+space")
	require.Eventually(t, func() bool { return rec.last().State == ipc.VoiceHotkeyRegistered }, 10*time.Second,
		5*time.Millisecond)
	require.NoError(t, stop())
	require.Equal(t, 2, p.bindings(), "a shortcut held with no key was not asked for again")
}

// The registry's refusal is said for what it is. Only the portal's own failure to take the app id is a
// missing .desktop file; a portal the bus could not start names the bus's error (M35's manual pass masked
// it, and was sent to install a file that was there).
func TestAPortalRefusalSaysWhatTheBusSaid(t *testing.T) {
	for name, want := range map[string]string{
		"org.freedesktop.DBus.Error.ServiceUnknown": "no desktop portal answers on the session bus",
		"org.freedesktop.DBus.Error.UnknownMethod": "the desktop portal is older than xdg-desktop-portal 1.19.4, " +
			"which an unsandboxed program needs to name itself",
		portalFailed: "the desktop portal does not know Norite: io.github.alexnex31.Norite.desktop is installed by " +
			"install.sh and by Norite's packages",
		"org.freedesktop.systemd1.UnitMasked": "the desktop portal could not be asked " +
			"(org.freedesktop.systemd1.UnitMasked)",
		"org.freedesktop.DBus.Error.Spawn.ChildExited": "the desktop portal could not be asked " +
			"(org.freedesktop.DBus.Error.Spawn.ChildExited)",
	} {
		var none *noPortal
		require.ErrorAs(t, portalRefusal(*dbus.NewError(name, []any{"words"})), &none)
		require.Equal(t, want, none.why, name)
	}
	var none *noPortal
	require.ErrorAs(t, portalRefusal(errors.New("context deadline exceeded")), &none)
	require.Equal(t, "the desktop portal did not answer", none.why)
}
