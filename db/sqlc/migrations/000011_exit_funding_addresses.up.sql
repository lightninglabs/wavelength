-- exit_funding_addresses stores the backing-wallet funding address handed
-- out by an exit plan for one target VTXO outpoint. It is persisted so the
-- same outpoint always maps to the same funding address across daemon
-- restarts: without it a restart derives a brand-new HD receive address and
-- demands a second deposit for the same VTXO (darepo-client#893).
--
-- Shipped as its own additive migration (rather than folded into
-- 000007_unilateral_exit) because it only CREATEs a new standalone table, so
-- existing deployments gain it without a schema reset.
CREATE TABLE IF NOT EXISTS exit_funding_addresses (
    -- target_outpoint_hash identifies the target VTXO transaction.
    target_outpoint_hash BLOB NOT NULL,

    -- target_outpoint_index identifies the target VTXO output index.
    target_outpoint_index INTEGER NOT NULL CHECK (
        target_outpoint_index >= 0
    ),

    -- funding_address is the backing-wallet receive address a user funds to
    -- clear the exit shortfall for this outpoint.
    funding_address TEXT NOT NULL,

    -- created_at is the unix timestamp when the address was first derived.
    created_at BIGINT NOT NULL,

    PRIMARY KEY (target_outpoint_hash, target_outpoint_index)
);
