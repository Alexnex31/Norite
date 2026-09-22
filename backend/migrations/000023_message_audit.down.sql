-- Dropping the table takes every recorded message with it, which is the honest reversal: the rows exist
-- only because this migration created somewhere to put them, and there is nowhere else they belong.
--
-- The indexes go with the table. The column is dropped last so that nothing can be written between the
-- table disappearing and the switch that fills it becoming unreachable.
DROP TABLE message_audit_entries;

ALTER TABLE guilds DROP COLUMN message_audit_enabled;
