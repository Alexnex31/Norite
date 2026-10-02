// SPDX-FileCopyrightText: 2026 Alexandre Duffez
// SPDX-License-Identifier: AGPL-3.0-or-later

package messages

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/Alexnex31/Norite/backend/internal/db"
)

// TestTheBacklogNamesItsAuthorsByPrimaryKeyOnly pins the plan of the channel listing once it joins `users`
// (M20a), under the plan that has bitten twice: the generic one.
//
// The listing is one of rule 7's hot paths, and the join is the first time it reads a second table. pgx
// runs cached prepared statements, and a generic plan does not know the page is fifty rows — it costs the
// LIMIT from a parameter — so it could prefer hashing the whole `users` table to fifty index lookups, and
// every page would read every account on the instance. No test of results can see that; the rows are right.
// So this reads the plan the server would use, for the statement pgx actually prepared, on a fixture where
// that choice would be expensive: 20,000 accounts, one channel of 60,000 messages among 500 of three.
//
// Measured on this fixture (M20a): the page alone reads 4 buffers in about 0.02 ms, and with its authors
// 154 in about 0.06 ms — fifty primary-key lookups.
func TestTheBacklogNamesItsAuthorsByPrimaryKeyOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	// Offsets from one snowflake, so each table's ids are unique; nothing reads them as timestamps.
	base := int64(f.ids.MustNext())
	f.exec(t, `INSERT INTO users (id, username, email, display_name)
	           SELECT $1::bigint + i, 'u' || i, 'u' || i || '@example.test', 'U ' || i
	           FROM generate_series(1, 20000) i`, base)
	f.exec(t, `INSERT INTO channels (id, guild_id, name, type, position)
	           SELECT $1::bigint + 100000 + c, $2, 'c', 0, 1 FROM generate_series(1, 500) c`, base, int64(f.guildID))
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content)
	           SELECT $1::bigint + 1000000 + i, $2, $1::bigint + 1 + (i % 20000), 'x'
	           FROM generate_series(1, 60000) i`, base, int64(f.channelID))
	f.exec(t, `INSERT INTO messages (id, channel_id, author_id, content)
	           SELECT $1::bigint + 2000000 + c * 10 + k, $1::bigint + 100000 + c, $1::bigint + 1 + c, 'y'
	           FROM generate_series(1, 500) c, generate_series(1, 3) k`, base)
	f.exec(t, `ANALYZE`)

	// One connection of its own, told to plan every statement generically from the first execution.
	pooled, err := f.pool.Acquire(ctx)
	require.NoError(t, err)
	conn := pooled.Hijack()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	_, err = conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`)
	require.NoError(t, err)

	// The results first, so a plan assertion on a statement returning nothing cannot pass by accident.
	page, err := db.New(conn).ListChannelMessages(ctx, db.ListChannelMessagesParams{
		ChannelID: int64(f.channelID), Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page, 50)
	for _, row := range page {
		require.NotNil(t, row.AuthorUsername, "message %d is named", row.ID)
		require.Equal(t, fmt.Sprintf("u%d", *row.AuthorID-base), *row.AuthorUsername)
	}

	var stmt string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT name FROM pg_prepared_statements WHERE statement LIKE $1`, "%-- name: ListChannelMessages :%",
	).Scan(&stmt), "pgx prepared ListChannelMessages on this connection")

	var raw []byte
	require.NoError(t, conn.QueryRow(ctx,
		`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE `+pgx.Identifier{stmt}.Sanitize()+
			fmt.Sprintf(`(%d, 50, NULL, NULL)`, int64(f.channelID)),
	).Scan(&raw))
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)

	// Confirm the confirmation: a custom plan would name the literal values and prove nothing here.
	require.Contains(t, string(raw), "$1", "the plan examined is the generic one")

	var reads []planNode
	plans[0].Plan.collect("users", &reads)
	require.NotEmpty(t, reads, "the plan reads users at all")
	var rows float64
	for _, n := range reads {
		require.Equal(t, "users_pkey", n.Index, "users is read by %s, not by its primary key", n.Type)
		rows += n.Rows * n.Loops
	}
	require.LessOrEqual(t, rows, float64(len(page)), "users is read once per message on the page, at most")
}

// planNode is the part of EXPLAIN's JSON that says how a relation was read.
type planNode struct {
	Type     string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Index    string     `json:"Index Name"`
	Rows     float64    `json:"Actual Rows"`
	Loops    float64    `json:"Actual Loops"`
	Plans    []planNode `json:"Plans"`
}

// collect appends every node below this one that reads relation.
func (n planNode) collect(relation string, into *[]planNode) {
	if n.Relation == relation {
		*into = append(*into, n)
	}
	for _, child := range n.Plans {
		child.collect(relation, into)
	}
}
