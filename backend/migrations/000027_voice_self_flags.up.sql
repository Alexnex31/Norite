-- Milestone M34 — what a caller has set on themselves.
--
-- A person muting or deafening themselves is the same two switches a moderator's mute and deafen are in
-- the client (M33's D6), set by the person instead. The channel is told, so a participant list can say who
-- will not hear it, and a client connecting later reads the same thing. Distinct from mute and deaf, which
-- the re-check writes and the person cannot clear.
ALTER TABLE voice_states
  ADD COLUMN self_mute boolean NOT NULL DEFAULT false,
  ADD COLUMN self_deaf boolean NOT NULL DEFAULT false;
