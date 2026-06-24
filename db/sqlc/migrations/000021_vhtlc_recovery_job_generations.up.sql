DROP INDEX IF EXISTS idx_vhtlc_recovery_jobs_unroll_target;
DROP INDEX IF EXISTS idx_vhtlc_recovery_jobs_swap_action;
DROP INDEX IF EXISTS idx_vhtlc_recovery_jobs_state_updated;

ALTER TABLE vhtlc_recovery_jobs RENAME TO vhtlc_recovery_jobs_old;

CREATE TABLE vhtlc_recovery_jobs (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    swap_id BLOB NOT NULL,
    direction TEXT NOT NULL CHECK (
        direction IN ('pay', 'receive', 'server_in', 'server_out')
    ),
    action TEXT NOT NULL CHECK (
        action IN ('claim', 'refund_without_receiver')
    ),
    state TEXT NOT NULL CHECK (state IN (
        'armed',
        'unroll_started',
        'waiting_for_target',
        'waiting_for_csv',
        'building_exit_spend',
        'exit_spend_built',
        'submitting_exit_spend',
        'exit_spend_pending_confirmation',
        'completed',
        'cancelled',
        'failed'
    )),
    vtxo_txid BLOB NOT NULL,
    vtxo_vout INTEGER NOT NULL CHECK (vtxo_vout >= 0),
    vtxo_amount_sat BIGINT NOT NULL CHECK (vtxo_amount_sat > 0),
    sender_pubkey BLOB NOT NULL,
    receiver_pubkey BLOB NOT NULL,
    server_pubkey BLOB NOT NULL,
    refund_locktime INTEGER NOT NULL CHECK (refund_locktime > 0),
    unilateral_claim_delay INTEGER NOT NULL CHECK (
        unilateral_claim_delay > 0
    ),
    unilateral_refund_delay INTEGER NOT NULL CHECK (
        unilateral_refund_delay > 0
    ),
    unilateral_refund_without_receiver_delay INTEGER NOT NULL CHECK (
        unilateral_refund_without_receiver_delay > 0
    ),
    preimage_hash BLOB NOT NULL,
    claim_preimage BLOB,
    signer_key_family INTEGER NOT NULL,
    signer_key_index INTEGER NOT NULL,
    destination_script BLOB NOT NULL,
    max_fee_rate_sat_per_kw INTEGER NOT NULL CHECK (
        max_fee_rate_sat_per_kw > 0
    ),
    unroll_target_outpoint_hash BLOB,
    unroll_target_outpoint_index INTEGER CHECK (
        unroll_target_outpoint_index IS NULL OR
        unroll_target_outpoint_index >= 0
    ),
    exit_policy_kind TEXT NOT NULL CHECK (exit_policy_kind IN (
        'vhtlc_claim',
        'vhtlc_refund_without_receiver'
    )),
    exit_tx BLOB,
    exit_txid BLOB,
    cooperative_txid BLOB,
    last_error TEXT,
    cancel_reason TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    armed_at BIGINT,
    escalated_at BIGINT,
    target_detected_at BIGINT,
    exit_tx_built_at BIGINT,
    exit_tx_broadcast_at BIGINT,
    terminal_at BIGINT,
    UNIQUE(swap_id, action, vtxo_txid, vtxo_vout)
);

INSERT INTO vhtlc_recovery_jobs (
    id, request_id, swap_id, direction, action, state, vtxo_txid,
    vtxo_vout, vtxo_amount_sat, sender_pubkey, receiver_pubkey,
    server_pubkey, refund_locktime, unilateral_claim_delay,
    unilateral_refund_delay, unilateral_refund_without_receiver_delay,
    preimage_hash, claim_preimage, signer_key_family, signer_key_index,
    destination_script, max_fee_rate_sat_per_kw,
    unroll_target_outpoint_hash, unroll_target_outpoint_index,
    exit_policy_kind, exit_tx, exit_txid, cooperative_txid, last_error,
    cancel_reason, created_at, updated_at, armed_at, escalated_at,
    target_detected_at, exit_tx_built_at, exit_tx_broadcast_at, terminal_at
)
SELECT
    id, request_id, swap_id, direction, action, state, vtxo_txid,
    vtxo_vout, vtxo_amount_sat, sender_pubkey, receiver_pubkey,
    server_pubkey, refund_locktime, unilateral_claim_delay,
    unilateral_refund_delay, unilateral_refund_without_receiver_delay,
    preimage_hash, claim_preimage, signer_key_family, signer_key_index,
    destination_script, max_fee_rate_sat_per_kw,
    unroll_target_outpoint_hash, unroll_target_outpoint_index,
    exit_policy_kind, exit_tx, exit_txid, cooperative_txid, last_error,
    cancel_reason, created_at, updated_at, armed_at, escalated_at,
    target_detected_at, exit_tx_built_at, exit_tx_broadcast_at, terminal_at
FROM vhtlc_recovery_jobs_old;

DROP TABLE vhtlc_recovery_jobs_old;

CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_state_updated
    ON vhtlc_recovery_jobs(state, updated_at DESC);

CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_swap_action
    ON vhtlc_recovery_jobs(swap_id, action);

CREATE INDEX IF NOT EXISTS idx_vhtlc_recovery_jobs_unroll_target
    ON vhtlc_recovery_jobs(
        unroll_target_outpoint_hash,
        unroll_target_outpoint_index
    )
    WHERE unroll_target_outpoint_hash IS NOT NULL;
