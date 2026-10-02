// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package attach

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/daemon/internal/state"
)

// BenchmarkFanOut measures one gateway event delivered to n watching clients, without the socket: what
// the gateway's read loop pays per event before the writers take over (M20 /optimization-review). Each
// client's queue is drained after every event, standing in for its writer, so nothing is dropped.
func BenchmarkFanOut(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		for _, size := range []int{200, 4000} {
			b.Run(fmt.Sprintf("clients=%d/content=%d", n, size), func(b *testing.B) {
				srv := New(Options{
					Session: &fakeSession{}, State: state.New(zerolog.Nop(), state.DefaultLimits),
					Log: zerolog.Nop(),
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var clients []*conn
				for range n {
					c := &conn{srv: srv, watching: true, out: make(chan net.Buffers, queueFrames),
						gone: make(chan struct{}), log: zerolog.Nop()}
					c.ctx, c.cancel = context.WithCancel(ctx)
					srv.clients[c] = struct{}{}
					clients = append(clients, c)
				}
				payload := messagePayload("100", "30", strings.Repeat("x", size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					srv.Dispatch("MESSAGE_CREATE", payload)
					// What each writer does before its write: take the frame and release its bytes. Inline,
					// so no client falls behind and is dropped, which would measure a race instead.
					for _, c := range clients {
						bufs := <-c.out
						total := 0
						for _, p := range bufs {
							total += len(p)
						}
						c.queued.Add(-int64(total))
					}
				}
			})
		}
	}
}
