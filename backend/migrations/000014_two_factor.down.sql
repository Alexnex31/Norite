-- Back to an instance with one factor.
--
-- The index goes with its table rather than separately: both tables are new here, so there is no
-- pre-existing shape to restore the way 000012's down had to.
DROP TABLE user_recovery_codes;
DROP TABLE user_totp;
