// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package roles_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
	"github.com/Alexnex31/Norite/backend/internal/platform/snowflake"
	"github.com/Alexnex31/Norite/backend/internal/roles"
)

// ListMemberRolesForUsers runs on every gateway event, for every guild a connected member is in, so it is
// the fan-out's hot path (rule 7). Its predecessor's shape, guild_id = $1 AND user_id = ANY($2), is the one
// M13a found scanning a large guild's every role grant under the generic plan pgx caches. This reads that
// generic plan, on a skewed dataset, and fails on any row a filter discards: the method
// TestMemberRoleReadsCannotScanTheWholeGuild (guilds) established.
func TestTheFanOutsRoleReadCannotScanTheWholeGuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	// One guild of 2,000 members holding two roles each, among 300 small ones: many small guilds and a few
	// large ones is the ordinary shape of an instance, and the large ones are where fan-out matters.
	owner := f.newUser(ctx, "owner")
	big, _ := f.newGuild(ctx, owner, roles.PermViewChannel)
	base := int64(f.next())
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
	// And four hundred channels in the large guild, for the overwrite read every channel event makes.
	f.exec(ctx, `INSERT INTO channels (id, guild_id, type, name, position)
	             SELECT $1::bigint + 30000 + c, $2, 0, 'c' || c, c FROM generate_series(1, 400) c`, base, int64(big))
	f.exec(ctx, `ANALYZE`)

	// A connection of its own with the generic plan forced, so what is examined is the plan a cached
	// statement uses after its fifth execution rather than one planned for these particular values.
	pooled, err := f.pool.Acquire(ctx)
	require.NoError(t, err)
	conn := pooled.Hijack()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	_, err = conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`)
	require.NoError(t, err)
	q := db.New(conn)

	// The real generated function, so pgx prepares the statement exactly as production does, and a check
	// that it answers correctly on this dataset.
	recipients := make([]snowflake.ID, 100)
	ids := make([]string, 100)
	for i := range recipients {
		recipients[i] = snowflake.ID(base + int64(i) + 1)
		ids[i] = fmt.Sprint(base + int64(i) + 1)
	}
	perms, err := roles.ResolveMany(ctx, q, big, 0, recipients, nil)
	require.NoError(t, err)
	require.Len(t, perms, 100, "all hundred are members")

	var stmt string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`, "%-- name: ListMemberRolesForUsers :%",
	).Scan(&stmt), "pgx prepared ListMemberRolesForUsers on this connection")

	var raw []byte
	require.NoError(t, conn.QueryRow(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+pgx.Identifier{stmt}.Sanitize()+
			`('{`+strings.Join(ids, ",")+`}'::bigint[], `+fmt.Sprint(int64(big))+`)`,
	).Scan(&raw))

	var plans []struct {
		Plan fanOutPlanNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	require.Contains(t, string(raw), "$2", "the plan examined is the generic one")

	removed, filters := plans[0].Plan.filtered()
	require.Zero(t, removed,
		"the generic plan discards rows through a filter %v: it is scanning the guild, not the recipients", filters)

	// Every channel event reads its channel's overwrites, and whether the channel still exists. A guild
	// predicate in that statement gave the generic plan the guild's channel index to walk, filtering the
	// guild's channels down to one: up to the channel ceiling per event, where the primary key is one row.
	channel := snowflake.ID(base + 30000 + 200)
	_, err = roles.ResolveMany(ctx, q, big, channel, recipients, nil)
	require.NoError(t, err)
	var overwriteStmt string
	var params int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT name, cardinality(parameter_types) FROM pg_prepared_statements WHERE statement LIKE $1`,
		"%-- name: ListOverwritesOfExistingChannel :%",
	).Scan(&overwriteStmt, &params), "pgx prepared ListOverwritesOfExistingChannel on this connection")
	args := fmt.Sprint(int64(channel))
	if params == 2 {
		args += ", " + fmt.Sprint(int64(big))
	}
	require.NoError(t, conn.QueryRow(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+pgx.Identifier{overwriteStmt}.Sanitize()+`(`+args+`)`,
	).Scan(&raw))
	plans = nil
	require.NoError(t, json.Unmarshal(raw, &plans))
	removed, filters = plans[0].Plan.filtered()
	require.Zero(t, removed,
		"the overwrite read's generic plan discards rows through a filter %v: it is walking the guild's channels", filters)
}

type fanOutPlanNode struct {
	Filter         string           `json:"Filter"`
	RowsRemoved    float64          `json:"Rows Removed by Filter"`
	RecheckRemoved float64          `json:"Rows Removed by Index Recheck"`
	Plans          []fanOutPlanNode `json:"Plans"`
}

func (n fanOutPlanNode) filtered() (float64, []string) {
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
