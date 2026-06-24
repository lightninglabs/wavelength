DROP INDEX IF EXISTS idx_client_ledger_idempotent_round;
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent_round
    ON ledger_entries(round_id, event_type, debit_account, credit_account)
    WHERE round_id IS NOT NULL;

DELETE FROM ledger_event_types
WHERE event_type IN ('wallet_utxo_spent', 'wallet_sweep_transfer');

DELETE FROM accounts
WHERE account_id = 'wallet_clearing';
