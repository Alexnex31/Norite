-- Drops the device-code flow. Rows in device_codes live twenty minutes and carry nothing durable: an
-- authorization in progress at the moment of a rollback fails at its next poll and is started again.
--
-- The column goes first. Dropping the table it references would otherwise fail, and doing it in this
-- order means a rollback that stops half way leaves oauth_states in the shape 000006 left it, which the
-- code from that milestone can still read.
ALTER TABLE oauth_states DROP COLUMN device_code_id;
DROP TABLE device_codes;
