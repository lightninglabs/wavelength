DROP INDEX IF EXISTS idx_client_ledger_round_uuid;
ALTER TABLE ledger_entries DROP COLUMN round_uuid;
