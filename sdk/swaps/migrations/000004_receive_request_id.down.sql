DROP INDEX IF EXISTS receive_swaps_request_id;
ALTER TABLE receive_swaps DROP COLUMN memo;
ALTER TABLE receive_swaps DROP COLUMN request_id;
