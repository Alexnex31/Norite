-- Make the sessions table sweepable.
--
-- Nothing has ever deleted from it. Every refresh *inserts* a successor and revokes its predecessor, so a
-- client on a fifteen-minute access token writes about ninety-six rows a day per device and none of them
-- ever go. auth.RunSweeper, which prunes six other tables, did not know this one existed.
--
-- Both indexes below were measured with EXPLAIN (ANALYZE, BUFFERS) on Postgres 16, against a table seeded
-- with one account's rotations. Neither number is a guess, and the two are worth quite different amounts.

-- The sweep filters on expires_at and ignores revoked_at, so a partial index predicated on
-- `revoked_at IS NULL` cannot serve it.
--
-- This is migration 000005's lesson recurring, and it lands harder here. There, the partial predicate
-- excluded rows that were merely consumed. Here it excludes every row that has been rotated away — which,
-- on a table where rotation is the dominant write, is very nearly the whole table. The index existed and
-- described almost nothing the sweep would ever look at.
--
-- What replacing it buys is that the sweep stops scaling with the table rather than with its own work.
-- Sweeping the same 500 expired rows:
--
--            partial (Seq Scan)          non-partial (Index Scan)
--    50,004    4.5 ms, 1,818 buffers      3.7 ms, 1,186 buffers
--   250,004   15.3 ms, 4,904 buffers      1.0 ms,   504 buffers
--
-- At fifty thousand rows it is a wash, and saying otherwise would be inventing a win. The point is the
-- second line: the scan cost grows with the table while the index cost grows with the rows actually
-- expiring, and a sweep every ten minutes deletes a small slice of a table meant to grow for years.
DROP INDEX sessions_expires_at_idx;
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- replaced_by_id is a self-referencing foreign key declared ON DELETE SET NULL, and it had no index.
--
-- That means Postgres runs a referential-integrity trigger for every deleted row, and without an index the
-- trigger scans the table looking for children. This is the one that mattered:
--
--   deleting 2,000 expired rows out of 50,004
--     the DELETE itself                    9.4 ms  ->   9.0 ms
--     the sessions_replaced_by_id_fkey trigger   3,757 ms  ->  21.1 ms
--
-- Four hundred times the cost of the work it was checking, and quadratic in the table, so a year of one
-- account's rotations would have spent about a minute and a half inside that trigger — holding a
-- connection from a pool that is deliberately small (docs/architecture.md §15.3).
--
-- The same unindexed-FK shape M9 measured at 279 ms -> 2.7 ms on another table. Worth checking for on
-- every foreign key this schema adds, because nothing about the declaration hints at it and the cost only
-- appears when something finally deletes.
CREATE INDEX sessions_replaced_by_id_idx ON sessions (replaced_by_id);
