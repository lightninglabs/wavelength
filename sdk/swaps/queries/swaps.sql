-- name: ClaimSwapHashOwner :one
INSERT INTO swap_hash_owners (payment_hash, direction)
VALUES ($1, $2)
ON CONFLICT (payment_hash) DO UPDATE SET
    direction = EXCLUDED.direction
WHERE swap_hash_owners.direction = EXCLUDED.direction
RETURNING direction;

-- name: UpsertReceiveSwap :exec
INSERT INTO receive_swaps (
    payment_hash,
    amount_sat,
    payer_fee_msat,
    state,
    invoice,
    preimage,
    deadline_unix,
    client_pubkey,
    payment_addr,
    operator_pubkey,
    swap_server_pubkey,
    settlement_type,
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
	claim_recovery_id,
	intervention_reason,
	requested_amount_sat,
	available_credit_sat,
	attached_credit_sat,
	dust_limit_sat,
	created_at_unix,
	updated_at_unix
) VALUES (
	$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
	$16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28,
	$29, $30, $31, $32
)
ON CONFLICT (payment_hash) DO UPDATE SET
    amount_sat = EXCLUDED.amount_sat,
    payer_fee_msat = EXCLUDED.payer_fee_msat,
    state = EXCLUDED.state,
    invoice = EXCLUDED.invoice,
    preimage = EXCLUDED.preimage,
    deadline_unix = EXCLUDED.deadline_unix,
    client_pubkey = EXCLUDED.client_pubkey,
    payment_addr = EXCLUDED.payment_addr,
    operator_pubkey = EXCLUDED.operator_pubkey,
    swap_server_pubkey = EXCLUDED.swap_server_pubkey,
    settlement_type = EXCLUDED.settlement_type,
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
	claim_recovery_id = EXCLUDED.claim_recovery_id,
	intervention_reason = EXCLUDED.intervention_reason,
	requested_amount_sat = EXCLUDED.requested_amount_sat,
	available_credit_sat = EXCLUDED.available_credit_sat,
	attached_credit_sat = EXCLUDED.attached_credit_sat,
	dust_limit_sat = EXCLUDED.dust_limit_sat,
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
    settlement_type,
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
    refund_recovery_id,
    preimage,
    intervention_reason,
    created_at_unix,
    updated_at_unix
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28
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
    settlement_type = EXCLUDED.settlement_type,
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
    refund_recovery_id = EXCLUDED.refund_recovery_id,
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

-- name: UpsertRefreshSwap :one
INSERT INTO refresh_swaps (
    payment_hash,
    preimage,
    amount_sat,
    source_outpoint,
    max_vtxo_age_blocks,
    state,
    expiry_unix,
    client_pubkey,
    operator_pubkey,
    server_pubkey,
    settlement_type,
    input_refund_locktime,
    input_unilateral_claim_delay,
    input_unilateral_refund_delay,
    input_unilateral_refund_without_receiver_delay,
    input_vhtlc_pkscript,
    input_vhtlc_policy_template,
    input_vhtlc_outpoint,
    input_vhtlc_amount,
    funding_session_id,
    input_refund_receive_pubkey,
    input_refund_receive_pkscript,
    input_refund_session_id,
    input_refund_recovery_id,
    output_sender_pubkey,
    output_refund_locktime,
    output_unilateral_claim_delay,
    output_unilateral_refund_delay,
    output_unilateral_refund_without_receiver_delay,
    output_vhtlc_pkscript,
    output_vhtlc_policy_template,
    output_vhtlc_outpoint,
    output_vhtlc_amount,
    output_observed_height,
    output_created_height,
    output_batch_expiry,
    pending_htlc_ack_cursor,
    output_claim_receive_pubkey,
    output_claim_receive_pkscript,
    output_claim_session_id,
    output_claim_recovery_id,
    input_claim_txid,
    intervention_reason,
    created_at_unix,
    updated_at_unix,
    state_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
    $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33,
    $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44,
    $45, $46
)
ON CONFLICT (payment_hash) DO UPDATE SET
    preimage = EXCLUDED.preimage,
    amount_sat = EXCLUDED.amount_sat,
    source_outpoint = EXCLUDED.source_outpoint,
    max_vtxo_age_blocks = EXCLUDED.max_vtxo_age_blocks,
    state = EXCLUDED.state,
    expiry_unix = EXCLUDED.expiry_unix,
    client_pubkey = EXCLUDED.client_pubkey,
    operator_pubkey = EXCLUDED.operator_pubkey,
    server_pubkey = EXCLUDED.server_pubkey,
    settlement_type = EXCLUDED.settlement_type,
    input_refund_locktime = EXCLUDED.input_refund_locktime,
    input_unilateral_claim_delay = EXCLUDED.input_unilateral_claim_delay,
    input_unilateral_refund_delay = EXCLUDED.input_unilateral_refund_delay,
    input_unilateral_refund_without_receiver_delay =
        EXCLUDED.input_unilateral_refund_without_receiver_delay,
    input_vhtlc_pkscript = EXCLUDED.input_vhtlc_pkscript,
    input_vhtlc_policy_template = EXCLUDED.input_vhtlc_policy_template,
    input_vhtlc_outpoint = EXCLUDED.input_vhtlc_outpoint,
    input_vhtlc_amount = EXCLUDED.input_vhtlc_amount,
    funding_session_id = EXCLUDED.funding_session_id,
    input_refund_receive_pubkey = EXCLUDED.input_refund_receive_pubkey,
    input_refund_receive_pkscript = EXCLUDED.input_refund_receive_pkscript,
    input_refund_session_id = EXCLUDED.input_refund_session_id,
    input_refund_recovery_id = EXCLUDED.input_refund_recovery_id,
    output_sender_pubkey = EXCLUDED.output_sender_pubkey,
    output_refund_locktime = EXCLUDED.output_refund_locktime,
    output_unilateral_claim_delay = EXCLUDED.output_unilateral_claim_delay,
    output_unilateral_refund_delay = EXCLUDED.output_unilateral_refund_delay,
    output_unilateral_refund_without_receiver_delay =
        EXCLUDED.output_unilateral_refund_without_receiver_delay,
    output_vhtlc_pkscript = EXCLUDED.output_vhtlc_pkscript,
    output_vhtlc_policy_template = EXCLUDED.output_vhtlc_policy_template,
    output_vhtlc_outpoint = EXCLUDED.output_vhtlc_outpoint,
    output_vhtlc_amount = EXCLUDED.output_vhtlc_amount,
    output_observed_height = EXCLUDED.output_observed_height,
    output_created_height = EXCLUDED.output_created_height,
    output_batch_expiry = EXCLUDED.output_batch_expiry,
    pending_htlc_ack_cursor = EXCLUDED.pending_htlc_ack_cursor,
    output_claim_receive_pubkey = EXCLUDED.output_claim_receive_pubkey,
    output_claim_receive_pkscript = EXCLUDED.output_claim_receive_pkscript,
    output_claim_session_id = EXCLUDED.output_claim_session_id,
    output_claim_recovery_id = EXCLUDED.output_claim_recovery_id,
    input_claim_txid = EXCLUDED.input_claim_txid,
    intervention_reason = EXCLUDED.intervention_reason,
    updated_at_unix = EXCLUDED.updated_at_unix,
    state_version = EXCLUDED.state_version
WHERE refresh_swaps.state_version = EXCLUDED.state_version - 1
RETURNING state_version;

-- name: GetRefreshSwap :one
SELECT * FROM refresh_swaps
WHERE payment_hash = $1
LIMIT 1;

-- name: ListRefreshSwaps :many
SELECT * FROM refresh_swaps
ORDER BY created_at_unix ASC;

-- name: ListPendingRefreshSwaps :many
SELECT * FROM refresh_swaps
WHERE state NOT IN (
    'Completed', 'Expired', 'Refunded', 'NeedsIntervention', 'Failed'
)
ORDER BY created_at_unix ASC;
