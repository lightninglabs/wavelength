-- Persist the per-round sweep delay alongside the round checkpoint.
--
-- A round is checkpointed at input_sig_sent and can confirm after a daemon
-- restart. The confirmation handler derives each new VTXO's absolute batch
-- expiry as confirmation_height + sweep_delay, but the delay lived only in
-- the in-memory FSM state, so a resumed round rebuilt it as zero and stamped
-- BatchExpiry == CreatedHeight. That VTXO reads back as already expired the
-- moment it is created.
--
-- Zero remains the default for rows written before this migration. Those
-- rounds have no recorded delay, so the confirmation path treats zero as
-- "unknown" and refuses to stamp an expiry rather than stamping a wrong one.
ALTER TABLE rounds
    ADD COLUMN sweep_delay INTEGER NOT NULL DEFAULT 0;
