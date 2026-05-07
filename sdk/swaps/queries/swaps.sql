-- name: UpsertReceiveSwap :exec
INSERT INTO receive_swaps (
    payment_hash,
    amount_sat,
    state,
    invoice,
    preimage,
    deadline_unix,
    client_pubkey,
    payment_addr,
    operator_pubkey,
    swap_server_pubkey,
    refund_locktime,
    unilateral_claim_delay,
    unilateral_refund_delay,
    unilateral_refund_without_receiver_delay,
    vhtlc_pkscript,
    vhtlc_policy_template,
    vhtlc_outpoint,
    vhtlc_amount,
    pending_htlc_ack_cursor,
    claim_receive_pubkey,
    claim_receive_pkscript,
    claim_session_id,
    intervention_reason,
    created_at_unix,
    updated_at_unix
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20, $21, $22, $23, $24, $25
)
ON CONFLICT (payment_hash) DO UPDATE SET
    amount_sat = EXCLUDED.amount_sat,
    state = EXCLUDED.state,
    invoice = EXCLUDED.invoice,
    preimage = EXCLUDED.preimage,
    deadline_unix = EXCLUDED.deadline_unix,
    client_pubkey = EXCLUDED.client_pubkey,
    payment_addr = EXCLUDED.payment_addr,
    operator_pubkey = EXCLUDED.operator_pubkey,
    swap_server_pubkey = EXCLUDED.swap_server_pubkey,
    refund_locktime = EXCLUDED.refund_locktime,
    unilateral_claim_delay = EXCLUDED.unilateral_claim_delay,
    unilateral_refund_delay = EXCLUDED.unilateral_refund_delay,
    unilateral_refund_without_receiver_delay =
        EXCLUDED.unilateral_refund_without_receiver_delay,
    vhtlc_pkscript = EXCLUDED.vhtlc_pkscript,
    vhtlc_policy_template = EXCLUDED.vhtlc_policy_template,
    vhtlc_outpoint = EXCLUDED.vhtlc_outpoint,
    vhtlc_amount = EXCLUDED.vhtlc_amount,
    pending_htlc_ack_cursor = EXCLUDED.pending_htlc_ack_cursor,
    claim_receive_pubkey = EXCLUDED.claim_receive_pubkey,
    claim_receive_pkscript = EXCLUDED.claim_receive_pkscript,
    claim_session_id = EXCLUDED.claim_session_id,
    intervention_reason = EXCLUDED.intervention_reason,
    updated_at_unix = EXCLUDED.updated_at_unix;

-- name: GetReceiveSwap :one
SELECT * FROM receive_swaps
WHERE payment_hash = $1
LIMIT 1;

-- name: ListReceiveSwaps :many
SELECT * FROM receive_swaps
ORDER BY created_at_unix ASC;

-- name: ListPendingReceiveSwaps :many
SELECT * FROM receive_swaps
WHERE state NOT IN ('Completed', 'Expired', 'NeedsIntervention', 'Failed')
ORDER BY created_at_unix ASC;

-- name: UpsertPaySwap :exec
INSERT INTO pay_swaps (
    payment_hash,
    invoice,
    max_fee_sat,
    state,
    amount_sat,
    fee_sat,
    expiry_unix,
    client_pubkey,
    operator_pubkey,
    server_pubkey,
    refund_locktime,
    unilateral_claim_delay,
    unilateral_refund_delay,
    unilateral_refund_without_receiver_delay,
    vhtlc_pkscript,
    vhtlc_policy_template,
    vhtlc_outpoint,
    vhtlc_amount,
    funding_session_id,
    refund_receive_pubkey,
    refund_receive_pkscript,
    refund_session_id,
    preimage,
    intervention_reason,
    created_at_unix,
    updated_at_unix
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20, $21, $22, $23, $24, $25, $26
)
ON CONFLICT (payment_hash) DO UPDATE SET
    invoice = EXCLUDED.invoice,
    max_fee_sat = EXCLUDED.max_fee_sat,
    state = EXCLUDED.state,
    amount_sat = EXCLUDED.amount_sat,
    fee_sat = EXCLUDED.fee_sat,
    expiry_unix = EXCLUDED.expiry_unix,
    client_pubkey = EXCLUDED.client_pubkey,
    operator_pubkey = EXCLUDED.operator_pubkey,
    server_pubkey = EXCLUDED.server_pubkey,
    refund_locktime = EXCLUDED.refund_locktime,
    unilateral_claim_delay = EXCLUDED.unilateral_claim_delay,
    unilateral_refund_delay = EXCLUDED.unilateral_refund_delay,
    unilateral_refund_without_receiver_delay =
        EXCLUDED.unilateral_refund_without_receiver_delay,
    vhtlc_pkscript = EXCLUDED.vhtlc_pkscript,
    vhtlc_policy_template = EXCLUDED.vhtlc_policy_template,
    vhtlc_outpoint = EXCLUDED.vhtlc_outpoint,
    vhtlc_amount = EXCLUDED.vhtlc_amount,
    funding_session_id = EXCLUDED.funding_session_id,
    refund_receive_pubkey = EXCLUDED.refund_receive_pubkey,
    refund_receive_pkscript = EXCLUDED.refund_receive_pkscript,
    refund_session_id = EXCLUDED.refund_session_id,
    preimage = EXCLUDED.preimage,
    intervention_reason = EXCLUDED.intervention_reason,
    updated_at_unix = EXCLUDED.updated_at_unix;

-- name: GetPaySwap :one
SELECT * FROM pay_swaps
WHERE payment_hash = $1
LIMIT 1;

-- name: ListPaySwaps :many
SELECT * FROM pay_swaps
ORDER BY created_at_unix ASC;

-- name: ListPendingPaySwaps :many
SELECT * FROM pay_swaps
WHERE state NOT IN (
    'Completed', 'Expired', 'Refunded', 'NeedsIntervention', 'Failed'
)
ORDER BY created_at_unix ASC;
