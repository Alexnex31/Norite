-- Back to an instance with accounts and nothing to put them in.
--
-- The order is not cosmetic. guilds and channels reference each other — channels.guild_id points at
-- guilds, and guilds.system_channel_id points back at channels through the constraint the up migration
-- adds last. Neither table can be dropped while the other stands, so the constraint comes off first and
-- breaks the cycle. Everything else drops in reverse dependency order.
--
-- Indexes go with their tables; only the constraint needs naming, because it is the one object created
-- outside the table that owns it.
DROP TABLE permission_overwrites;
DROP TABLE guild_member_roles;
DROP TABLE roles;
DROP TABLE guild_members;

ALTER TABLE guilds DROP CONSTRAINT fk_system_channel;

DROP TABLE channels;
DROP TABLE guilds;
