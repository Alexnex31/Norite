// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// The GlobalShortcuts portal (xdg-desktop-portal 1.16), which the desktop and not Norite owns: it suggests
// a key, the desktop and the person choose, and it says when the shortcut is pressed and released whatever
// program is in front. M25 ran it on Hyprland 0.56 and Plasma 6.6, where a tap and a hold each arrived as
// one press and one release.
const (
	portalName      = "org.freedesktop.portal.Desktop"
	portalPath      = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	globalShortcuts = "org.freedesktop.portal.GlobalShortcuts"
	portalRequest   = "org.freedesktop.portal.Request"
	portalSession   = "org.freedesktop.portal.Session"
	hostRegistry    = "org.freedesktop.host.portal.Registry"
	// shortcutID is the one shortcut Norite asks for.
	shortcutID = "push-to-talk"
	// sessionWithin bounds the portal's answer to a session, which asks nobody anything.
	sessionWithin = 10 * time.Second
	// maxTrigger bounds what a desktop says the key is.
	maxTrigger = 256
)

// noPortal is a desktop with no portal this binding can use, and why: none answers, it offers no global
// shortcuts, or it cannot be told Norite's app id. Where the session is X's, a grab on its X server is
// tried instead.
type noPortal struct{ why string }

func (n *noPortal) Error() string { return n.why }

// bindPortal binds key through the portal on bus, a connection of its own, and reports through ev until
// ctx ends. It returns a *noPortal, having reported nothing, when there is no portal to use; nil otherwise,
// having reported whatever became of the key.
func bindPortal(ctx context.Context, bus *dbus.Conn, key Key, ev Events) error {
	signals := make(chan *dbus.Signal, 64)
	bus.Signal(signals)
	defer bus.RemoveSignal(signals)
	for _, iface := range []string{portalRequest, globalShortcuts, portalSession} {
		if err := bus.AddMatchSignal(dbus.WithMatchInterface(iface)); err != nil {
			return &noPortal{why: "the session bus refused to deliver the desktop portal's signals"}
		}
	}
	desktop := bus.Object(portalName, portalPath)

	// An unsandboxed program names itself, once per connection, before anything else it asks (1.19.4).
	callCtx, cancel := context.WithTimeout(ctx, askWithin)
	err := desktop.CallWithContext(callCtx, hostRegistry+".Register", 0, AppID, map[string]dbus.Variant{}).Err
	cancel()
	if err != nil {
		return portalRefusal(err)
	}
	callCtx, cancel = context.WithTimeout(ctx, askWithin)
	err = desktop.CallWithContext(callCtx, "org.freedesktop.DBus.Properties.Get", 0, globalShortcuts,
		"version").Err
	cancel()
	if err != nil {
		return &noPortal{why: "the desktop portal offers no global shortcuts"}
	}

	req := requests{bus: bus, signals: signals}
	code, results, err := req.ask(ctx, sessionWithin, nil, desktop, globalShortcuts+".CreateSession",
		map[string]dbus.Variant{"session_handle_token": dbus.MakeVariant(token())})
	if err != nil || code != 0 {
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the desktop portal did not open a session " +
			"for the shortcut"})
		return nil
	}
	handle, _ := results["session_handle"].Value().(string)
	session := dbus.ObjectPath(handle)
	if !session.IsValid() {
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the desktop portal named no session"})
		return nil
	}
	defer func() {
		// Closing the session releases the shortcut; the desktop keeps the person's binding for next time.
		closeCtx, cancel := context.WithTimeout(context.Background(), askWithin)
		defer cancel()
		_ = bus.Object(portalName, session).CallWithContext(closeCtx, portalSession+".Close", 0).Err
	}()

	shortcuts := []shortcut{{ID: shortcutID, Options: map[string]dbus.Variant{
		"description":       dbus.MakeVariant("Push to talk in a Norite call"),
		"preferred_trigger": dbus.MakeVariant(key.Trigger()),
	}}}
	// The desktop may ask the person now, and waits for them, however long they take.
	waiting := func() { ev.Status(Status{State: ipc.VoiceHotkeyWaiting}) }
	code, results, err = req.ask(ctx, 0, waiting, desktop, globalShortcuts+".BindShortcuts", session, shortcuts,
		"", map[string]dbus.Variant{})
	switch {
	case ctx.Err() != nil:
		return nil
	case err != nil:
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the desktop portal could not bind the shortcut"})
		return nil
	case code == 1:
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the shortcut was refused on the desktop; " +
			"join again to be asked again"})
		return nil
	case code != 0:
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the desktop did not bind the shortcut"})
		return nil
	}
	ev.Status(bound(results["shortcuts"]))

	down := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case s, ok := <-signals:
			if !ok {
				ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the session bus went away"})
				return nil
			}
			switch {
			case s.Path == session && s.Name == portalSession+".Closed":
				ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: "the desktop closed the shortcut"})
				return nil
			case s.Name == globalShortcuts+".ShortcutsChanged" && ours(s.Body, session, ""):
				ev.Status(bound(dbus.MakeVariant(s.Body[1])))
			case s.Name == globalShortcuts+".Activated" && ours(s.Body, session, shortcutID) && !down:
				down = true
				ev.Held(true)
			case s.Name == globalShortcuts+".Deactivated" && ours(s.Body, session, shortcutID) && down:
				down = false
				ev.Held(false)
			}
		}
	}
}

// shortcut is one entry of the a(sa{sv}) the portal takes.
type shortcut struct {
	ID      string
	Options map[string]dbus.Variant
}

// portalRefusal is what the host registry's refusal says of the portal: none, one too old to know the
// registry, or one that does not know Norite's app id.
func portalRefusal(err error) error {
	var de dbus.Error
	if !errors.As(err, &de) {
		return &noPortal{why: "the desktop portal did not answer"}
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
		return &noPortal{why: "no desktop portal answers on the session bus"}
	case "org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownObject":
		return &noPortal{why: "the desktop portal is older than xdg-desktop-portal 1.19.4, which an unsandboxed " +
			"program needs to name itself"}
	}
	return &noPortal{why: "the desktop portal does not know Norite: " + AppID + ".desktop is installed by " +
		"install.sh and by Norite's packages"}
}

// bound is what became of the shortcut as the portal's list of bound shortcuts says: the key the desktop
// bound it to, or, where the desktop binds keys only in its own settings and says none, what to do there.
func bound(list dbus.Variant) Status {
	entries, _ := list.Value().([][]any)
	for _, e := range entries {
		if len(e) != 2 {
			continue
		}
		id, _ := e[0].(string)
		opts, _ := e[1].(map[string]dbus.Variant)
		if id != shortcutID || opts == nil {
			continue
		}
		if desc, _ := opts["trigger_description"].Value().(string); desc != "" {
			return Status{State: ipc.VoiceHotkeyRegistered, Trigger: cut(desc, maxTrigger)}
		}
	}
	return Status{State: ipc.VoiceHotkeyRegistered, Reason: "the desktop names no key for it: bind Norite's " +
		"push to talk in the desktop's settings"}
}

// ours is whether a GlobalShortcuts signal is about session, and id when one is named.
func ours(body []any, session dbus.ObjectPath, id string) bool {
	if len(body) < 2 {
		return false
	}
	if path, _ := body[0].(dbus.ObjectPath); path != session {
		return false
	}
	if id == "" {
		return true
	}
	got, _ := body[1].(string)
	return got == id
}

// requests asks the portal's methods that answer through a Request object's Response signal.
type requests struct {
	bus     *dbus.Conn
	signals <-chan *dbus.Signal
}

// ask calls method with args and a handle_token added to its last argument, an options dictionary, and
// waits for the Response, within within unless it is 0, calling waiting once the call is made. The
// request's path is known before the call, from the connection's name and the token, so a Response that
// arrives before the call returns is not missed.
func (r requests) ask(ctx context.Context, within time.Duration, waiting func(), obj dbus.BusObject,
	method string, args ...any) (uint32, map[string]dbus.Variant, error) {
	tok := token()
	if opts, ok := args[len(args)-1].(map[string]dbus.Variant); ok {
		opts["handle_token"] = dbus.MakeVariant(tok)
	}
	names := r.bus.Names()
	if len(names) == 0 {
		return 0, nil, errors.New("no name on the session bus")
	}
	sender := strings.ReplaceAll(strings.TrimPrefix(names[0], ":"), ".", "_")
	want := dbus.ObjectPath(fmt.Sprintf("%s/request/%s/%s", portalPath, sender, tok))
	callCtx, cancel := context.WithTimeout(ctx, askWithin)
	err := obj.CallWithContext(callCtx, method, 0, args...).Err
	cancel()
	if err != nil {
		return 0, nil, err
	}
	if waiting != nil {
		waiting()
	}
	var deadline <-chan time.Time
	if within > 0 {
		t := time.NewTimer(within)
		defer t.Stop()
		deadline = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-deadline:
			return 0, nil, errors.New("the desktop portal did not answer")
		case s, ok := <-r.signals:
			if !ok {
				return 0, nil, errors.New("the session bus went away")
			}
			if s.Path != want || s.Name != portalRequest+".Response" || len(s.Body) != 2 {
				continue
			}
			code, _ := s.Body[0].(uint32)
			results, _ := s.Body[1].(map[string]dbus.Variant)
			return code, results, nil
		}
	}
}

func token() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "norite_" + hex.EncodeToString(b)
}

// cut is s cut to at most n bytes, on a character's boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
