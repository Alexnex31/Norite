// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package voice

import (
	"fmt"
	"runtime/debug"
)

// contain is deferred at the top of every goroutine and every Pion callback the package runs for a
// connection. The self-hosted SFU shares norite-server's process, so a panic in code that parses a
// stranger's packets would otherwise end REST, the gateway and messaging with it; contained, it ends that
// one connection. Pion's own goroutines are beyond a recover here, which the security ledger records.
func (c *conn) contain(where string) {
	v := recover()
	if v == nil {
		return
	}
	c.log.Error().
		Str("in", where).
		Str("panic", fmt.Sprint(v)).
		Bytes("stack", debug.Stack()).
		Msg("a voice connection panicked; closing it")
	go c.close("it panicked in " + where)
}
