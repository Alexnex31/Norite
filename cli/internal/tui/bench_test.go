// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"strings"
	"testing"
)

func fullPane(contentLen int) *paneModel {
	p := newPane("10", "20")
	p.resize(100)
	p.loaded = true
	for i := range maxHeld {
		p.put(message(fmt.Sprint(1000+i), "20", "2", "Bob", strings.Repeat("word ", contentLen/5)), 100)
	}
	return p
}

func BenchmarkPaneView(b *testing.B) {
	for _, n := range []int{40, 400, 4000} {
		p := fullPane(n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				_ = p.view(100, 30)
			}
		})
	}
}

func BenchmarkPanePutScrolled(b *testing.B) {
	p := fullPane(400)
	p.scroll = 10
	i := 0
	for b.Loop() {
		p.put(message(fmt.Sprint(5000+i), "20", "2", "Bob", "new"), 100)
		i++
	}
}
