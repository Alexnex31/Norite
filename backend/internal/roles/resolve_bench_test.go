// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles

import (
	"fmt"
	"testing"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
)

// A guild at the channel ceiling with three role overwrites on each channel — the shape the channel
// listing actually meets, and the one 000015's own numbers are taken at.
func benchGuild(channels, perChannel int) (ids []snowflake.ID, rows []db.PermissionOverwrite) {
	for c := 1; c <= channels; c++ {
		id := snowflake.ID(1000 + c)
		ids = append(ids, id)
		for o := 0; o < perChannel; o++ {
			rows = append(rows, db.PermissionOverwrite{
				ChannelID: int64(id), TargetType: OverwriteTargetRole,
				TargetID: int64(500 + o), Allow: 1, Deny: 2,
			})
		}
	}
	return ids, rows
}

func benchResolution() Resolution {
	return Resolution{
		UserID: 42, OwnerID: 7,
		Permissions: PermViewChannel, base: PermViewChannel,
		heldRoleIDs: map[int64]struct{}{500: {}, 501: {}}, everyoneRoleID: 500,
	}
}

// BenchmarkListingWholeSlice is the shape the channel listing had before an optimization review: hand
// every channel the whole guild's rows and let InChannel skip the ones that do not match.
//
// Correct, and quadratic in the guild. Kept as a benchmark rather than deleted because the correct-looking
// call is still the one the type system permits — InChannel takes a slice, and nothing stops a future
// caller passing the wrong one. This is what that costs.
func BenchmarkListingWholeSlice(b *testing.B) {
	for _, n := range []int{50, 500} {
		ids, rows := benchGuild(n, 3)
		res := benchResolution()
		b.Run(fmt.Sprintf("channels=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				for _, id := range ids {
					_ = res.InChannel(id, rows)
				}
			}
		})
	}
}

// BenchmarkListingGrouped is what it does now: group by channel once, hand each channel only its own.
//
// Slower below roughly a hundred channels, where building the map costs more than the scan it saves, and
// several times faster at the ceiling. The listing pays the grouping either way — it needs the rows
// per-channel to embed them in the response — so the map is not a cost this shape adds.
func BenchmarkListingGrouped(b *testing.B) {
	for _, n := range []int{50, 500} {
		ids, rows := benchGuild(n, 3)
		res := benchResolution()
		b.Run(fmt.Sprintf("channels=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				byChannel := make(map[snowflake.ID][]db.PermissionOverwrite, len(ids))
				for _, ow := range rows {
					id := snowflake.ID(ow.ChannelID)
					byChannel[id] = append(byChannel[id], ow)
				}
				for _, id := range ids {
					_ = res.InChannel(id, byChannel[id])
				}
			}
		})
	}
}
