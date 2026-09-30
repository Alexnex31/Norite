// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package guilds

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// TestMemberRoleReadsCannotScanTheWholeGuild pins the plan of the two statements that read members' roles,
// under the plan that bit: the generic one.
//
// pgx runs cached prepared statements, and after a few executions Postgres may plan one without its
// parameters. For `guild_id = $1` that plan assumes an average guild, and on an instance of many small
// guilds the average is a handful of grants — so the `user_id = ANY($2)` shape these replaced scanned a
// large guild's every grant and filtered, 3,382 us a call under load against about 100 us (the M13a
// optimization review, measured end to end). No test of results can see it: the rows are right, only
// slow. So this reads the plan the server would use, for the statements pgx actually prepared, and
// requires that no node discards rows through a filter.
//
// The fixture is what makes the generic plan wrong: one guild of 4,000 grants among 300 of a few each.
func TestMemberRoleReadsCannotScanTheWholeGuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	owner := f.newUser(ctx, "owner")
	big := f.newGuild(ctx, owner, roles.PermViewChannel)
	base := int64(f.next())

	// Ids are offsets from one snowflake so each table's are unique; nothing reads them as timestamps.
	f.exec(ctx, `INSERT INTO users (id, username, email, display_name)
	             SELECT $1::bigint + i, 'm' || i, 'm' || i || '@example.test', 'm' || i FROM generate_series(1, 2000) i`, base)
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id) SELECT $1::bigint, $2::bigint + i FROM generate_series(1, 2000) i`,
		int64(big), base)
	f.exec(ctx, `INSERT INTO roles (id, guild_id, name, position) VALUES ($1::bigint + 3001, $2, 'a', 1), ($1::bigint + 3002, $2, 'b', 2)`,
		base, int64(big))
	f.exec(ctx, `INSERT INTO guild_member_roles (guild_id, user_id, role_id)
	             SELECT $1::bigint, $2::bigint + i, $2::bigint + 3000 + k FROM generate_series(1, 2000) i, generate_series(1, 2) k`,
		int64(big), base)

	f.exec(ctx, `INSERT INTO guilds (id, name, owner_id) SELECT $1::bigint + 10000 + g, 'small', $2 FROM generate_series(1, 300) g`,
		base, int64(owner))
	f.exec(ctx, `INSERT INTO roles (id, guild_id, name, position) SELECT $1::bigint + 20000 + g, $1::bigint + 10000 + g, 'r', 1
	             FROM generate_series(1, 300) g`, base)
	f.exec(ctx, `INSERT INTO guild_members (guild_id, user_id)
	             SELECT $1::bigint + 10000 + g, $1::bigint + ((g * 7 + k) % 2000) + 1 FROM generate_series(1, 300) g, generate_series(1, 4) k
	             ON CONFLICT DO NOTHING`, base)
	f.exec(ctx, `INSERT INTO guild_member_roles (guild_id, user_id, role_id)
	             SELECT guild_id, user_id, guild_id + 10000 FROM guild_members WHERE guild_id > $1::bigint + 10000`, base)
	f.exec(ctx, `ANALYZE`)

	// One connection of its own, told to plan every statement generically from the first execution.
	pooled, err := f.pool.Acquire(ctx)
	require.NoError(t, err)
	conn := pooled.Hijack()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	_, err = conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`)
	require.NoError(t, err)
	q := db.New(conn)

	// The results first, so a plan assertion on a statement returning nothing cannot pass by accident.
	page, err := q.ListGuildMembers(ctx, db.ListGuildMembersParams{GuildID: int64(big), UserID: 0, Limit: 100})
	require.NoError(t, err)
	require.Len(t, page, 100)
	for _, m := range page {
		if m.UserID == int64(owner) {
			require.Empty(t, m.RoleIds, "the owner was granted nothing")
			continue
		}
		require.Equal(t, []int64{base + 3001, base + 3002}, m.RoleIds, "each member's two roles, in id order")
	}
	one, err := q.ListRoleIDsOfMember(ctx, db.ListRoleIDsOfMemberParams{GuildID: int64(big), UserID: base + 1})
	require.NoError(t, err)
	require.Equal(t, []int64{base + 3001, base + 3002}, one)

	for _, c := range []struct {
		name string
		args string
	}{
		{"ListGuildMembers", fmt.Sprintf("%d, 0, 100", int64(big))},
		{"ListRoleIDsOfMember", fmt.Sprintf("%d, %d", int64(big), base+1)},
	} {
		var stmt string
		require.NoError(t, conn.QueryRow(ctx,
			`SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`, "%-- name: "+c.name+" :%",
		).Scan(&stmt), "pgx prepared %s on this connection", c.name)

		var raw []byte
		require.NoError(t, conn.QueryRow(ctx,
			`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+pgx.Identifier{stmt}.Sanitize()+`(`+c.args+`)`,
		).Scan(&raw))

		var plans []struct {
			Plan planNode `json:"Plan"`
		}
		require.NoError(t, json.Unmarshal(raw, &plans))
		require.Len(t, plans, 1)

		// Confirm the confirmation: a custom plan would name the literal values and prove nothing here.
		require.Contains(t, string(raw), "$1", "%s: the plan examined is the generic one", c.name)

		removed, filters := plans[0].Plan.filtered()
		require.Zero(t, removed,
			"%s's generic plan discards rows through a filter %v — it is scanning the guild, not the members",
			c.name, filters)
	}
}

// planNode is the part of EXPLAIN's JSON a filter shows up in.
type planNode struct {
	Filter         string     `json:"Filter"`
	RowsRemoved    float64    `json:"Rows Removed by Filter"`
	RecheckRemoved float64    `json:"Rows Removed by Index Recheck"`
	Plans          []planNode `json:"Plans"`
}

// filtered totals the rows every node below this one discarded, and names the filters that did it.
func (n planNode) filtered() (float64, []string) {
	removed := n.RowsRemoved + n.RecheckRemoved
	var filters []string
	if n.RowsRemoved > 0 {
		filters = append(filters, strings.TrimSpace(n.Filter))
	}
	for _, child := range n.Plans {
		r, fs := child.filtered()
		removed += r
		filters = append(filters, fs...)
	}
	return removed, filters
}
