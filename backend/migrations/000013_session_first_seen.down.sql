-- Back to deriving first_seen from the rows that survive, which is what 000013 exists to stop doing.
DROP INDEX sessions_live_by_device_idx;
ALTER TABLE sessions DROP COLUMN first_seen;
