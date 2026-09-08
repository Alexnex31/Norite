-- Back to an instance that mutates guilds without recording it.
--
-- Both indexes go with the table. Nothing references audit_log_entries, so there is no order to get right
-- here — unlike 000015, whose cycle between guilds and channels does have one.
DROP TABLE audit_log_entries;
