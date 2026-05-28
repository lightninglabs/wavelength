-- 000002_swap_recovery_and_fee_columns reintroduces three columns that were
-- appended to the already-applied 000001 migration by PRs #486 and #521.
-- golang-migrate never re-runs a version already recorded in
-- swap_client_schema_migrations, so swap stores created before those PRs booted
-- without the columns and failed swap listing with "no such column". Moving the
-- additions into this forward-only migration upgrades every existing swap store
-- on first boot; the backward-compatible defaults backfill legacy rows.

-- payer_fee_msat is the payer-paid route fee quoted by the swap server. The fee
-- is not deducted from amount_sat. Legacy receive rows backfill to 0.
ALTER TABLE receive_swaps
    ADD COLUMN payer_fee_msat BIGINT NOT NULL DEFAULT 0;

-- claim_recovery_id is the daemon-owned vHTLC recovery row armed for the
-- receive-side unilateral claim fallback. Empty means recovery has not been
-- armed yet.
ALTER TABLE receive_swaps
    ADD COLUMN claim_recovery_id TEXT NOT NULL DEFAULT '';

-- refund_recovery_id is the daemon-owned vHTLC recovery row armed for the
-- pay-side unilateral refund-without-receiver fallback. Empty means recovery
-- has not been armed yet.
ALTER TABLE pay_swaps
    ADD COLUMN refund_recovery_id TEXT NOT NULL DEFAULT '';
