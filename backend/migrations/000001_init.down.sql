-- Down migration for the intentionally-empty initial migration (see 000001_init.up.sql).
--
-- There is nothing to undo, but the file must exist: golang-migrate requires a matching down file for
-- every version, and a missing one would only be discovered during an actual rollback.
SELECT 1;
