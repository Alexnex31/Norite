-- Milestone M1 — initial migration, intentionally empty.
--
-- It exists so the migration machinery is exercised end to end from the first milestone: golang-migrate
-- creates its schema_migrations bookkeeping table, the advisory-lock guard runs, and startup blocks on
-- completion, all before any real table depends on it working. The first real schema lands at Milestone
-- M4 (users, auth), which is the earliest point the roadmap defines any table.
--
-- Postgres needs a statement here, so this is a no-op one rather than an empty file.
SELECT 1;
