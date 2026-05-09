-- Reverse 000012: remove the boarding-sweep ledger / classification rows.
-- We only delete the seeded rows; ledger_entries / wallet_utxo_log rows
-- referencing them are not silently rewritten because doing so would
-- violate the append-only invariant of those tables. Operators with
-- pre-existing boarding-sweep history must therefore manually reconcile
-- before downgrading.
DELETE FROM utxo_classifications
WHERE classification IN ('boarding_sweep_input', 'boarding_sweep_return');

DELETE FROM ledger_event_types
WHERE event_type = 'boarding_sweep_fee_paid';
