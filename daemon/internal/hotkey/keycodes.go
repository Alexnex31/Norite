// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import "errors"

// The keys each OS's backend registers, in a file every OS compiles so that CI, which runs Linux alone,
// tests the tables the Windows and macOS backends read (M32's rule for each OS's decisions).

// Windows' modifiers for RegisterHotKey (winuser.h). MOD_NOREPEAT keeps a held key from repeating.
const (
	winModAlt      = 0x0001
	winModControl  = 0x0002
	winModShift    = 0x0004
	winModWin      = 0x0008
	winModNoRepeat = 0x4000
)

// windowsKey is key as RegisterHotKey takes it: its modifiers and its virtual-key code. A letter's code is
// its uppercase ASCII, a digit's its ASCII, F1 to F24 0x70 to 0x87, and space 0x20.
func windowsKey(key Key) (mods, vk uint32) {
	mods = winModNoRepeat
	for _, m := range []struct {
		on   bool
		mask uint32
	}{{key.Ctrl, winModControl}, {key.Alt, winModAlt}, {key.Shift, winModShift}, {key.Super, winModWin}} {
		if m.on {
			mods |= m.mask
		}
	}
	switch c := key.Name[0]; {
	case key.Name == "space":
		vk = 0x20
	case key.Function() != 0:
		vk = uint32(0x70 + key.Function() - 1)
	case c >= 'a' && c <= 'z':
		vk = uint32(c - 'a' + 'A')
	default:
		vk = uint32(c)
	}
	return mods, vk
}

// Carbon's modifiers for RegisterEventHotKey (Events.h).
const (
	macCmd     = 0x0100
	macShift   = 0x0200
	macOption  = 0x0800
	macControl = 0x1000
)

// macCodes are Carbon's virtual key codes (HIToolbox/Events.h, kVK_*), which name a key by its place on a
// US keyboard: on another layout a letter is the key at that letter's place there. Space and the function
// keys are the same everywhere, which is why the default key is space. macOS has no F21 to F24.
var macCodes = map[string]uint32{
	"a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04, "g": 0x05, "z": 0x06, "x": 0x07, "c": 0x08,
	"v": 0x09, "b": 0x0B, "q": 0x0C, "w": 0x0D, "e": 0x0E, "r": 0x0F, "y": 0x10, "t": 0x11, "1": 0x12,
	"2": 0x13, "3": 0x14, "4": 0x15, "6": 0x16, "5": 0x17, "9": 0x19, "7": 0x1A, "8": 0x1C, "0": 0x1D,
	"o": 0x1F, "u": 0x20, "i": 0x22, "p": 0x23, "l": 0x25, "j": 0x26, "k": 0x28, "n": 0x2D, "m": 0x2E,
	"f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76, "f5": 0x60, "f6": 0x61, "f7": 0x62, "f8": 0x64,
	"f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F, "f13": 0x69, "f14": 0x6B, "f15": 0x71, "f16": 0x6A,
	"f17": 0x40, "f18": 0x4F, "f19": 0x50, "f20": 0x5A, "space": 0x31,
}

// macKey is key as RegisterEventHotKey takes it. macOS refuses a key whose only modifiers are Option or
// Shift and Option on recent systems (ADR 0034, read), and a letter with Shift alone would be taken from
// typing, so a key other than a function key needs Control or Command.
func macKey(key Key) (mods, code uint32, err error) {
	code, ok := macCodes[key.Name]
	if !ok {
		return 0, 0, errors.New("macOS has no " + key.Name + " key; the function keys go to f20")
	}
	if key.Function() == 0 && !key.Ctrl && !key.Super {
		return 0, 0, errors.New("on macOS the key needs ctrl or super, Control or Command")
	}
	for _, m := range []struct {
		on   bool
		mask uint32
	}{{key.Ctrl, macControl}, {key.Alt, macOption}, {key.Shift, macShift}, {key.Super, macCmd}} {
		if m.on {
			mods |= m.mask
		}
	}
	return mods, code, nil
}
