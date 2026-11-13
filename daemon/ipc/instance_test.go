// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package ipc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWhichURLsNameOneInstance(t *testing.T) {
	assert.True(t, SameInstance("https://chat.example", "https://chat.example/"))
	assert.True(t, SameInstance("https://Chat.Example/norite", "https://chat.example/norite/"))
	// The port a connection uses, and the host without the dot of a fully qualified name.
	for _, pair := range [][2]string{
		{"https://chat.example:443", "https://chat.example"},
		{"https://chat.example:", "https://chat.example"},
		{"http://chat.example:80/norite", "http://chat.example/norite"},
		{"https://chat.example.", "https://chat.example"},
		{"http://[FD00::1]:80", "http://[fd00::1]"},
		{"http://192.168.1.20:8080", "http://192.168.1.20:8080/"},
	} {
		assert.True(t, SameInstance(pair[0], pair[1]), "%q and %q", pair[0], pair[1])
		assert.True(t, SameInstance(pair[1], pair[0]), "%q and %q", pair[1], pair[0])
	}
	for _, pair := range [][2]string{
		{"https://chat.example", "http://chat.example"},
		{"https://chat.example", "https://chat.example:8443"},
		{"http://chat.example:443", "http://chat.example"},
		{"https://chat.example:80", "https://chat.example"},
		{"http://[fd00::1]:8080", "http://[fd00::1]:8081"},
		{"https://chat.example..", "https://chat.example"},
		{"https://chat.example", "https://chat.example.evil.test"},
		{"https://chat.example/a", "https://chat.example/b"},
		{"https://chat.example", ""},
		{"", ""},
		{"chat.example", "chat.example"},
	} {
		assert.False(t, SameInstance(pair[0], pair[1]), "%q and %q", pair[0], pair[1])
	}
}
