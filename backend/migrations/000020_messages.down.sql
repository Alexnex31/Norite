-- Back to an instance whose channels hold nothing.
--
-- Order matters here, unlike 000016's: message_edit_history references messages, so it goes first. Both
-- tables take their indexes with them.
--
-- messages' self-reference does not need untangling — dropping the table drops the constraint with it —
-- but channels.last_message_id does *not* come back to NULL, because it is a denormalized pointer with no
-- FK (000015 says why) and nothing here can see which rows hold a now-dangling id. Re-applying 000020
-- onto that state gives a channel a last_message_id naming a message that no longer exists. Harmless for
-- a dev reset, which is the only thing a down migration is for here, and stated because the alternative
-- reading is that this file is a supported rollback for a populated instance. It is not.
DROP TABLE message_edit_history;
DROP TABLE messages;
