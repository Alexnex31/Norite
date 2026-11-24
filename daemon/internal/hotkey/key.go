// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Alexnex31/Norite/daemon/ipc"
)

// Key is a push-to-talk key as every backend reads it: which modifiers, and one key.
type Key struct {
	Ctrl, Alt, Shift, Super bool
	// Name is a letter or a digit, "space", or "f1" to "f24".
	Name string
}

// ParseKey reads a key in ipc.CanonicalKey's spelling.
func ParseKey(s string) (Key, error) {
	canonical, err := ipc.CanonicalKey(s)
	if err != nil {
		return Key{}, err
	}
	parts := strings.Split(canonical, "+")
	k := Key{Name: parts[len(parts)-1]}
	for _, m := range parts[:len(parts)-1] {
		switch m {
		case "ctrl":
			k.Ctrl = true
		case "alt":
			k.Alt = true
		case "shift":
			k.Shift = true
		case "super":
			k.Super = true
		}
	}
	return k, nil
}

// Function is the key's number when it is f1 to f24, and 0 otherwise.
func (k Key) Function() int {
	if len(k.Name) < 2 || k.Name[0] != 'f' {
		return 0
	}
	n, _ := strconv.Atoi(k.Name[1:])
	return n
}

// Trigger is the key in the XDG shortcuts specification's spelling, which the GlobalShortcuts portal takes
// as the key it suggests: CTRL, ALT, SHIFT and LOGO, then the key's keysym name, as in CTRL+SHIFT+space.
func (k Key) Trigger() string {
	var parts []string
	for _, m := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "CTRL"}, {k.Alt, "ALT"}, {k.Shift, "SHIFT"}, {k.Super, "LOGO"}} {
		if m.on {
			parts = append(parts, m.name)
		}
	}
	name := k.Name
	if n := k.Function(); n != 0 {
		name = fmt.Sprintf("F%d", n)
	}
	return strings.Join(append(parts, name), "+")
}
