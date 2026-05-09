-- Register the boarding-sweep ledger event type so FeePaidMsg with
-- FeeType=FeeTypeOnchainSweep (event_type="boarding_sweep_fee_paid")
-- satisfies the ledger_entries.event_type FK constraint.
INSERT INTO ledger_event_types (event_type) VALUES
    ('boarding_sweep_fee_paid');

-- Register the per-input / per-destination UTXO audit classifications
-- emitted by the boarding-sweep flow alongside the ledger fee event.
-- These satisfy the wallet_utxo_log.classification FK constraint.
INSERT INTO utxo_classifications (classification) VALUES
    ('boarding_sweep_input'),
    ('boarding_sweep_return');
