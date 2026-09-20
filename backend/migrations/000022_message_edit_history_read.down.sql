-- Restore 000020's index exactly. The down is a real reversal rather than a drop: the cascade this table
-- lives under is 190x slower with no index at all, so leaving the column unindexed on the way down would
-- turn a rollback into a message-deletion outage.
DROP INDEX message_edit_history_message_id_id_idx;

CREATE INDEX message_edit_history_message_id_edited_at_idx
  ON message_edit_history (message_id, edited_at DESC);
