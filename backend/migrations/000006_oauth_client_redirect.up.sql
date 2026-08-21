-- Milestone M8 — record where a client asked to be returned to once the provider has come back.

-- A command-line client cannot read the exchange code off the page the callback renders, so it names a
-- listener on its own machine when the flow starts and the callback returns the code there instead. That
-- destination has to survive the round trip through the provider, which is what this column is for: it is
-- fixed when the flow starts and read back out of the row the callback consumes, never re-supplied by
-- whoever presents the callback. Same discipline as code_verifier beside it.
--
-- NOT NULL DEFAULT '' rather than nullable, deliberately. sqlc is configured with
-- emit_pointers_for_null_types, so a nullable column arrives in Go as *string and every read site grows a
-- nil check for a distinction that does not exist — '' is not a valid redirect, so absent and empty are
-- the same fact. oauth_identities.email is the precedent.
--
-- No index, and that is a decision rather than an omission (CLAUDE.md rule 7). This column is never a
-- predicate: it is read out of a row ConsumeOAuthState has already found by state_hash, which
-- oauth_states_state_hash_idx serves, and the sweep filters expires_at, which oauth_states_expires_at_idx
-- serves. No query shape changes here at all — CreateOAuthState gains a column and ConsumeOAuthState gains
-- nothing, since it already returns every column.
ALTER TABLE oauth_states ADD COLUMN client_redirect_uri text NOT NULL DEFAULT '';
