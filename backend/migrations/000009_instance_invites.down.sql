-- Drops instance invites.
--
-- Destructive in a way the short-lived tables are not: an outstanding invite is a thing somebody was given
-- and expects to work, and rolling this back makes every one of them stop. Worse, it does so silently from
-- the holder's side — registration answers the same refusal it gives for a code that never existed, which
-- is deliberate there and unhelpful here.
--
-- An instance running registration_mode = "invite" that rolls this back refuses every registration
-- outright, which is M4's behaviour and is at least safe: nobody gets in who should not.
DROP TABLE instance_invites;
