-- Inherited batch-expiry persistence for OOR-materialized VTXOs.
--
-- Round-created VTXOs derive their batch expiry from the owning round's
-- confirmation_height + csv_delay via the vtxos.round_id FK. OOR-created
-- VTXOs, however, are materialized with round_id=NULL (they never flow
-- through a round), so the round-join returns no match and BatchExpiry=0
-- silently. Seal-time forfeit fee computation in
-- rounds/seal_time_fee_builder.go then sees remaining=0 for any OOR-derived
-- refresh input and undercharges the operator fee.
--
-- The fix threads the inherited expiry -- min(parent.batch_expiry) across
-- the OOR session's consumed inputs -- onto the materialized output row at
-- finalize time. Round-created VTXOs leave this column NULL and continue
-- to use the round-join fallback via COALESCE in GetVTXOWithRoundExpiry.

ALTER TABLE vtxos
	ADD COLUMN batch_expiry INTEGER;
