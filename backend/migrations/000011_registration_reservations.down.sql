-- Drops registration reservations.
--
-- Rolling back reopens the account-existence oracle 000011 closed: with no reservations, a registration
-- against a taken address leaves the username free while one against a fresh address does not, and two
-- requests read the difference. The code from before this migration does not consult the table, so a
-- rollback is safe in the mechanical sense and costs the security property.
DROP TABLE registration_reservations;
