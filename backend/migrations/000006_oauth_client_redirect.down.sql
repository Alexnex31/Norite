-- Drops the client's return URI. Rows in this table live fifteen minutes, so nothing durable is lost:
-- any flow in progress at the moment of a rollback fails at its callback and is started again.
ALTER TABLE oauth_states DROP COLUMN client_redirect_uri;
