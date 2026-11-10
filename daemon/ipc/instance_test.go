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
	for _, pair := range [][2]string{
		{"https://chat.example", "http://chat.example"},
		{"https://chat.example", "https://chat.example:8443"},
		{"https://chat.example", "https://chat.example.evil.test"},
		{"https://chat.example/a", "https://chat.example/b"},
		{"https://chat.example", ""},
		{"", ""},
		{"chat.example", "chat.example"},
	} {
		assert.False(t, SameInstance(pair[0], pair[1]), "%q and %q", pair[0], pair[1])
	}
}
