// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// A passive grab on an X server's root window, where the session is X's and no portal answers (ADR 0034
// §2): the key is taken from every other program while it is held, which is why a call holds it and
// nothing else does. M25 ran it under Xvfb with another window focused: a tap and a hold each gave one
// press and one release, once X's autorepeat was told from a release.

func init() {
	// xgb logs what it could not do to the standard logger, which would put its words in the daemon's
	// stderr. The binding says what failed itself.
	xgb.Logger = log.New(io.Discard, "", 0)
}

// maxXauthority bounds the Xauthority file read: a few entries per display and host.
const maxXauthority = 1 << 20

// Keysyms the grab needs (X11/keysymdef.h).
const (
	keysymSpace   = 0x0020
	keysymF1      = 0xffbe
	keysymNumLock = 0xff7f
	keysymAltL    = 0xffe9
	keysymAltR    = 0xffea
	keysymSuperL  = 0xffeb
	keysymSuperR  = 0xffec
)

// bindX11 grabs key on the session's X server and reports through ev until ctx ends. whyNoPortal is why the
// portal was not used, which a refusal here says too.
func bindX11(ctx context.Context, env map[string]string, key Key, ev Events, whyNoPortal string) {
	unavailable := func(why string) {
		ev.Status(Status{State: ipc.VoiceHotkeyUnavailable, Reason: why + " (and " + whyNoPortal + ")"})
	}
	conn, err := dialX(env)
	if err != nil {
		unavailable(err.Error())
		return
	}
	defer conn.Close()
	grab, err := grabKey(conn, key)
	if err != nil {
		unavailable(err.Error())
		return
	}
	// Released by requests the server answers, before the binding stops, so the key is free when stop
	// returns. Closing the connection releases the grabs too, but whenever the server gets to the close; no
	// test sees the difference (thirty runs under Xvfb passed without this), and it costs four round trips.
	defer grab.release()

	events := make(chan xgb.Event, 64)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			e, err := conn.WaitForEvent()
			if e == nil && err == nil {
				return // the connection closed
			}
			if e != nil {
				select {
				case events <- e:
				default: // a flood the grab did not ask for
				}
			}
		}
	}()
	ev.Status(Status{State: ipc.VoiceHotkeyRegistered})

	var r repeats
	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-gone:
			unavailable("the X server went away")
			return
		case e := <-events:
			switch e := e.(type) {
			case xproto.KeyPressEvent:
				if e.Detail == grab.code && r.press(uint32(e.Time)) {
					ev.Held(true)
				}
			case xproto.KeyReleaseEvent:
				if e.Detail == grab.code {
					r.release(uint32(e.Time))
					settle = time.After(repeatWithin)
				}
			}
		case <-settle:
			settle = nil
			if r.settle() {
				ev.Held(false)
			}
		}
	}
}

// dialX connects to the session's X server, which must be this machine's: a display forwarded over SSH is
// somebody's keyboard on another machine.
func dialX(env map[string]string) (*xgb.Conn, error) {
	display := env["DISPLAY"]
	n, ok := localDisplay(display)
	if !ok {
		return nil, fmt.Errorf("the X display %q is not one on this machine", cut(display, 64))
	}
	socket := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
	c, err := net.DialTimeout("unix", socket, askWithin)
	if err != nil {
		// Linux's abstract socket of the same name, which an X server also listens on.
		var err2 error
		if c, err2 = net.DialTimeout("unix", "@"+socket, askWithin); err2 != nil {
			return nil, fmt.Errorf("no X server answers on display :%d", n)
		}
	}
	// The session's cookie, never xgb's own lookup, which reads this process's environment: a daemon started
	// before the login has none, or a stale one.
	cookie, err := xCookie(env, n)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("no authorization for display :%d: %w", n, err)
	}
	conn, err := xgb.NewConnNetWithCookieHex(c, cookie)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the X server on display :%d refused the connection", n)
	}
	return conn, nil
}

// localDisplay reads a display on this machine, ":0" or ":0.1", as its number.
func localDisplay(display string) (int, bool) {
	rest, ok := strings.CutPrefix(display, ":")
	if !ok {
		return 0, false
	}
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 0 && n < 1<<16 && strconv.Itoa(n) == rest
}

// xCookie is the MIT-MAGIC-COOKIE-1 for local display n from the session's Xauthority file, in hex.
func xCookie(env map[string]string, n int) (string, error) {
	path := env["XAUTHORITY"]
	if path == "" {
		home := env["HOME"]
		if home == "" {
			return "", errors.New("no Xauthority file")
		}
		path = filepath.Join(home, ".Xauthority")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	host, _ := os.Hostname()
	return readCookie(bufio.NewReader(io.LimitReader(f, maxXauthority)), host, strconv.Itoa(n))
}

// readCookie walks an Xauthority file (X11/Xauth.h): a family, an address, a display number, a name and
// data, each but the first a length-prefixed string. The entry for this display is a local or a wild one,
// for this host or for any.
func readCookie(r io.Reader, host, display string) (string, error) {
	const familyLocal, familyWild = 256, 65535
	field := func() ([]byte, error) {
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	for {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			return "", errors.New("no entry for this display")
		}
		var parts [4][]byte
		for i := range parts {
			b, err := field()
			if err != nil {
				return "", errors.New("the Xauthority file is cut short")
			}
			parts[i] = b
		}
		addr, disp, name, data := string(parts[0]), string(parts[1]), string(parts[2]), parts[3]
		hostOK := family == familyWild || (family == familyLocal && addr == host)
		if hostOK && (disp == "" || disp == display) && name == "MIT-MAGIC-COOKIE-1" && len(data) == 16 {
			return hex.EncodeToString(data), nil
		}
	}
}

// keycodes is the server's keyboard mapping, as what finds the keycode a keysym is on.
func keycodes(conn *xgb.Conn) (func(xproto.Keysym) (xproto.Keycode, bool), error) {
	setup := xproto.Setup(conn)
	count := byte(int(setup.MaxKeycode) - int(setup.MinKeycode) + 1)
	km, err := xproto.GetKeyboardMapping(conn, setup.MinKeycode, count).Reply()
	if err != nil {
		return nil, errors.New("the X server did not say its keyboard's mapping")
	}
	return func(sym xproto.Keysym) (xproto.Keycode, bool) {
		per := int(km.KeysymsPerKeycode)
		for i := 0; i*per < len(km.Keysyms); i++ {
			for j := 0; j < per && i*per+j < len(km.Keysyms); j++ {
				if km.Keysyms[i*per+j] == sym {
					return xproto.Keycode(int(setup.MinKeycode) + i), true
				}
			}
		}
		return 0, false
	}, nil
}

// grabbed is a key grabbed on the root window, under each of masks.
type grabbed struct {
	conn  *xgb.Conn
	root  xproto.Window
	code  xproto.Keycode
	masks []uint16
}

// release ungrabs the key under each mask, each answered by the server.
func (g *grabbed) release() {
	for _, m := range g.masks {
		_ = xproto.UngrabKeyChecked(g.conn, g.code, g.root, m).Check()
	}
}

// grabKey finds key's keycode and the modifiers' masks in the server's own mappings, and grabs it on the root
// window under every combination of the lock modifiers: X matches a grab's modifiers exactly, so one made
// without NumLock does not fire with NumLock on.
func grabKey(conn *xgb.Conn, key Key) (*grabbed, error) {
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	codeOf, err := keycodes(conn)
	if err != nil {
		return nil, err
	}
	code, ok := codeOf(keysymOf(key))
	if !ok {
		return nil, fmt.Errorf("this keyboard has no %s key", key.Name)
	}
	// Which of Mod1 to Mod5 holds Alt, Super and NumLock is the server's mapping, Mod1, Mod4 and Mod2 on
	// most.
	alt, super, numLock := uint16(xproto.ModMask1), uint16(xproto.ModMask4), uint16(xproto.ModMask2)
	if mm, err := xproto.GetModifierMapping(conn).Reply(); err == nil && mm.KeycodesPerModifier > 0 {
		per := int(mm.KeycodesPerModifier)
		holds := func(sym xproto.Keysym) (uint16, bool) {
			c, ok := codeOf(sym)
			if !ok {
				return 0, false
			}
			for m := 0; m < 8; m++ {
				for k := 0; k < per && m*per+k < len(mm.Keycodes); k++ {
					if mm.Keycodes[m*per+k] == c {
						return 1 << m, true
					}
				}
			}
			return 0, false
		}
		for _, s := range []xproto.Keysym{keysymAltL, keysymAltR} {
			if m, ok := holds(s); ok {
				alt = m
				break
			}
		}
		for _, s := range []xproto.Keysym{keysymSuperL, keysymSuperR} {
			if m, ok := holds(s); ok {
				super = m
				break
			}
		}
		if m, ok := holds(keysymNumLock); ok {
			numLock = m
		}
	}
	var mods uint16
	for _, m := range []struct {
		on   bool
		mask uint16
	}{{key.Ctrl, xproto.ModMaskControl}, {key.Shift, xproto.ModMaskShift}, {key.Alt, alt}, {key.Super, super}} {
		if m.on {
			mods |= m.mask
		}
	}
	g := &grabbed{conn: conn, root: root, code: code}
	for _, extra := range []uint16{0, xproto.ModMaskLock, numLock, xproto.ModMaskLock | numLock} {
		m := mods | extra
		err := xproto.GrabKeyChecked(conn, true, root, m, code, xproto.GrabModeAsync, xproto.GrabModeAsync).Check()
		if err != nil {
			g.release()
			var access xproto.AccessError
			if errors.As(err, &access) {
				return nil, errors.New("another program on this X display holds the key")
			}
			return nil, errors.New("the X server refused the key's grab")
		}
		g.masks = append(g.masks, m)
	}
	return g, nil
}

// keysymOf is key's keysym: a letter's lowercase one, a digit's, space's, or F1 to F24's.
func keysymOf(key Key) xproto.Keysym {
	switch {
	case key.Name == "space":
		return keysymSpace
	case key.Function() != 0:
		return xproto.Keysym(keysymF1 + key.Function() - 1)
	}
	return xproto.Keysym(key.Name[0])
}
