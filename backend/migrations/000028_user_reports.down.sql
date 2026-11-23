-- Back to message reports only. A user report open beside another of the same target in a second guild
-- would make the narrower index fail to build, so open user reports go first; this is a dev reset, as
-- every down migration here is.
DELETE FROM reports WHERE target_type = 3;
DROP INDEX reports_open_target_per_reporter_idx;
CREATE UNIQUE INDEX reports_open_target_per_reporter_idx
  ON reports (reporter_id, target_type, target_id) WHERE status = 0;
DROP INDEX IF EXISTS reports_channel_id_idx;
ALTER TABLE reports DROP COLUMN IF EXISTS channel_id;
