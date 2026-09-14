DROP INDEX audit_log_entries_guild_id_id_idx;
CREATE INDEX audit_log_entries_guild_id_created_at_idx ON audit_log_entries (guild_id, created_at DESC);
