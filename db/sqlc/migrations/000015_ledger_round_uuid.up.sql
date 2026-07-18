-- round_uuid is the canonical lowercase UUID string form of round_id. The
-- ledger stores round_id as a raw 16-byte BLOB while every round-adjacent
-- table (rounds.round_id, vtxos.forfeit_round_id) stores the TEXT UUID, and
-- no BLOB<->TEXT conversion exists in the SQL dialect subset shared by
-- SQLite and Postgres. Materializing the TEXT form as its own column makes
-- ledger rows joinable against those tables in plain SQL (e.g. attributing a
-- round's operator fee to the VTXOs it forfeited).
--
-- The column is nullable: rows without a round linkage stay NULL, mirroring
-- round_id. Existing rows are backfilled by the Go post-migration step
-- registered for this version (see db/post_migration_checks.go), since the
-- BLOB-to-UUID-string conversion is not expressible in portable SQL.
ALTER TABLE ledger_entries ADD COLUMN round_uuid TEXT;

-- The composite (round_uuid, event_type) key fully covers the settlement fee
-- lookup (a correlated per-round SUM filtered by fee event types) without a
-- residual filter step.
CREATE INDEX IF NOT EXISTS idx_client_ledger_round_uuid
    ON ledger_entries(round_uuid, event_type)
    WHERE round_uuid IS NOT NULL;
