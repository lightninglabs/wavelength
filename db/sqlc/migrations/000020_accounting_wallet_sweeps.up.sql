-- Add the clearing account and event types needed to account for
-- wallet-level sweeps without double-counting internal return outputs.
INSERT INTO accounts (account_id, account_name, account_type) VALUES
    ('wallet_clearing', 'Wallet Sweep Clearing', 'asset')
ON CONFLICT DO NOTHING;

INSERT INTO ledger_event_types (event_type) VALUES
    ('wallet_utxo_spent'),
    ('wallet_sweep_transfer')
ON CONFLICT DO NOTHING;

-- Round-scoped events without a specific key remain deduped by
-- (round_id, event_type, accounts). Events that carry an explicit
-- idempotency_key, such as per-recipient round sends, are instead
-- deduped by idx_client_ledger_idempotent_key so multiple sends in
-- the same round can coexist.
DROP INDEX IF EXISTS idx_client_ledger_idempotent_round;
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent_round
    ON ledger_entries(round_id, event_type, debit_account, credit_account)
    WHERE round_id IS NOT NULL
      AND idempotency_key IS NULL;
