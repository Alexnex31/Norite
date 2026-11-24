// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package hotkey

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAKeyIsReadIntoItsParts(t *testing.T) {
	k, err := ParseKey("Shift+Ctrl+Space")
	require.NoError(t, err)
	assert.Equal(t, Key{Ctrl: true, Shift: true, Name: "space"}, k)
	k, err = ParseKey("super+alt+f12")
	require.NoError(t, err)
	assert.Equal(t, Key{Alt: true, Super: true, Name: "f12"}, k)
	assert.Equal(t, 12, k.Function())
	_, err = ParseKey("a")
	require.Error(t, err)
}

// The portal is suggested the key in the shortcuts specification's words, its keysym name included.
func TestThePortalIsSuggestedTheKeyInItsOwnWords(t *testing.T) {
	for in, want := range map[string]string{
		"ctrl+shift+space": "CTRL+SHIFT+space",
		"alt+super+p":      "ALT+LOGO+p",
		"f13":              "F13",
		"ctrl+7":           "CTRL+7",
	} {
		k, err := ParseKey(in)
		require.NoError(t, err)
		assert.Equal(t, want, k.Trigger(), in)
	}
}

// RegisterHotKey's virtual keys and modifiers, a held key never repeating (MOD_NOREPEAT).
func TestWindowsIsAskedForTheKeyItKnows(t *testing.T) {
	for in, want := range map[string][2]uint32{
		"ctrl+shift+space": {winModControl | winModShift | winModNoRepeat, 0x20},
		"alt+super+p":      {winModAlt | winModWin | winModNoRepeat, 'P'},
		"ctrl+7":           {winModControl | winModNoRepeat, '7'},
		"f1":               {winModNoRepeat, 0x70},
		"shift+f24":        {winModShift | winModNoRepeat, 0x87},
	} {
		k, err := ParseKey(in)
		require.NoError(t, err)
		mods, vk := windowsKey(k)
		assert.Equal(t, want, [2]uint32{mods, vk}, in)
	}
}

// Carbon's key codes are a US keyboard's places, and macOS needs Control or Command on any key but a
// function key, and has none past F20.
func TestMacOSIsAskedForTheKeyItKnows(t *testing.T) {
	for in, want := range map[string][2]uint32{
		"ctrl+shift+space": {macControl | macShift, 0x31},
		"alt+super+p":      {macOption | macCmd, 0x23},
		"ctrl+a":           {macControl, 0x00},
		"f13":              {0, 0x69},
	} {
		k, err := ParseKey(in)
		require.NoError(t, err)
		mods, code, err := macKey(k)
		require.NoError(t, err, in)
		assert.Equal(t, want, [2]uint32{mods, code}, in)
	}
	for _, in := range []string{"alt+p", "alt+shift+space", "ctrl+f21"} {
		k, err := ParseKey(in)
		require.NoError(t, err)
		_, _, err = macKey(k)
		assert.Error(t, err, in)
	}
	// Every letter and digit a key may be has its place.
	for c := 'a'; c <= 'z'; c++ {
		assert.Contains(t, macCodes, string(c))
	}
	for c := '0'; c <= '9'; c++ {
		assert.Contains(t, macCodes, string(c))
	}
	seen := map[uint32]string{}
	for name, code := range macCodes {
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s share the code %#x", name, other, code)
		}
		seen[code] = name
	}
}
