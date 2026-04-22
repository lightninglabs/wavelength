-- UTXO diff classifier: extend the wallet_utxo_log audit trail
-- so the ledger actor can distinguish round / sweep-attributable
-- outpoint movements from genuinely external ones and only book
-- external_deposit / external_withdrawal legs for the latter.
--
-- Rather than add a separate attribution table, piggyback on
-- the existing audit log:
--
--   - source_id carries the round_id or batch_id a pre-insert
--     attributes the outpoint to. NULL for rows the diff loop
--     produced itself (genuine or still-pending external
--     movements).
--   - Three new classification values fill gaps the initial
--     enum left:
--       * withdrawal -- the spent-side analogue of deposit; the
--         diff loop books RecordExternalWithdrawal for these.
--       * sweep_consumption -- the spent-side analogue of
--         sweep_return; emitted when the diff sees an outpoint
--         that was consumed as a sweep input and matches a
--         pre-insert from handleSweepCompleted.
--       * pending -- the diff loop's two-phase default. A
--         concurrent round / sweep handler may pre-insert an
--         attributed audit row in the window between the block
--         epoch landing in the mailbox and the producer's
--         RoundConfirmedMsg / SweepCompletedMsg being drained;
--         the diff loop writes 'pending' first, and a
--         reconciliation pass at the next block epoch promotes
--         still-unattributed rows to deposit / withdrawal and
--         books the corresponding external_* ledger leg.

INSERT INTO utxo_classifications (classification) VALUES
    ('withdrawal'),
    ('sweep_consumption'),
    ('pending'),
    ('round_change')
ON CONFLICT DO NOTHING;

-- source_id is the 16-byte round_id or batch_id that attributes
-- the outpoint to a specific round or sweep. NULL means the
-- audit row was produced by the diff loop itself, not pre-
-- inserted by a round / sweep handler.
ALTER TABLE wallet_utxo_log ADD COLUMN source_id BLOB;

-- Index on source_id to speed up "all outpoints attributed to
-- round X" reconciliation queries the classifier will use when
-- the handler's pre-insert is replayed.
CREATE INDEX IF NOT EXISTS idx_utxo_log_source_id
    ON wallet_utxo_log(source_id)
    WHERE source_id IS NOT NULL;
