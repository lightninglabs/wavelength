DROP INDEX IF EXISTS idx_utxo_log_source_id;
ALTER TABLE wallet_utxo_log DROP COLUMN source_id;
DELETE FROM utxo_classifications WHERE classification IN (
    'withdrawal', 'sweep_consumption', 'pending', 'round_change'
);
