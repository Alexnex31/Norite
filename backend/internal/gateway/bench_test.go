// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Alexnex31/Norite/backend/internal/dispatch"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// benchServer holds n READY sessions, each in five of n/20 guilds, so an event's guild has about a hundred
// members on the process whatever n is.
func benchServer(n int) *Server {
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer}}
	guilds := n / 20
	for i := range n {
		set := map[snowflake.ID]struct{}{}
		for k := range 5 {
			set[snowflake.ID((i*7+k*131)%guilds+1)] = struct{}{}
		}
		s := &session{srv: srv, id: fmt.Sprint(i), userID: snowflake.ID(i + 1)}
		srv.idx.register(s)
		s.becomeReady(ready{}, set)
	}
	return srv
}

// The candidate lookup, which scanned every session on the process before the index.
func BenchmarkCandidates(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			srv := benchServer(n)
			ev := dispatch.Event{Type: "MESSAGE_CREATE", Audience: dispatch.Guild, GuildID: 1}
			b.ReportAllocs()
			var got int
			for b.Loop() {
				got = len(srv.candidates(ev))
			}
			b.ReportMetric(float64(got), "candidates")
		})
	}
}

// BenchmarkDispatchFrame is one recipient's share of an event: numbering it and building its frame.
func BenchmarkDispatchFrame(b *testing.B) {
	payload, _ := json.Marshal(map[string]any{"id": "363829384819245057", "content": strings.Repeat("word ", 400)})
	srv := &Server{opts: Options{Logger: zerolog.Nop(), ResumeBuffer: DefaultResumeBuffer}}
	s := &session{srv: srv, ready: true, guilds: map[snowflake.ID]struct{}{}}
	ev := dispatch.Event{Type: "MESSAGE_CREATE", Data: payload}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		s.mu.Lock()
		s.applyLocked(ev)
		s.mu.Unlock()
	}
}

// slowAudience stands in for the database: every resolution takes what a channel event with a hundred
// candidates measured against a local Postgres.
type slowAudience struct{}

func (slowAudience) Allowed(_ context.Context, _ dispatch.Event, c []snowflake.ID) ([]snowflake.ID, error) {
	time.Sleep(770 * time.Microsecond)
	return c, nil
}

// How many events a process fans out a second, across 64 busy guilds, by lane count.
func BenchmarkFanoutLanes(b *testing.B) {
	for _, lanes := range []int{1, 4} {
		b.Run(fmt.Sprint(lanes), func(b *testing.B) {
			srv := benchServer(1_280)
			srv.opts.Audience = slowAudience{}
			srv.opts.FanoutLanes = lanes
			srv.startLanes()
			payloads := make([][]byte, 64)
			for g := range payloads {
				payloads[g], _ = json.Marshal(dispatch.Event{
					Type: "MESSAGE_CREATE", Audience: dispatch.Guild, GuildID: snowflake.ID(g + 1), Data: json.RawMessage(`{}`),
				})
			}
			start := time.Now()
			i := 0
			for b.Loop() {
				srv.onEvent(payloads[i%64])
				i++
			}
			srv.closeLanes()
			b.ReportMetric(float64(i)/time.Since(start).Seconds(), "events/s")
		})
	}
}
