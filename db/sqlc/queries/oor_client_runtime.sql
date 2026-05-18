-- name: UpsertOORClientSession :exec
INSERT INTO oor_client_sessions (
    session_id, direction, state, idempotency_key, retry_after,
    retry_reason, fail_reason, created_at, updated_at, completed_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10
)
ON CONFLICT (session_id) DO UPDATE SET
    direction = excluded.direction,
    state = excluded.state,
    idempotency_key = excluded.idempotency_key,
    retry_after = excluded.retry_after,
    retry_reason = excluded.retry_reason,
    fail_reason = excluded.fail_reason,
    updated_at = excluded.updated_at,
    completed_at = excluded.completed_at;

-- name: ListActiveOORClientSessions :many
SELECT session_id, direction, state, idempotency_key, retry_after,
    retry_reason, fail_reason, created_at, updated_at, completed_at
FROM oor_client_sessions
WHERE completed_at IS NULL
ORDER BY updated_at, session_id;

-- name: ListOORClientSessions :many
SELECT session_id, direction, state, idempotency_key, retry_after,
    retry_reason, fail_reason, created_at, updated_at, completed_at
FROM oor_client_sessions
ORDER BY updated_at, session_id;

-- name: GetOORClientSession :one
SELECT session_id, direction, state, idempotency_key, retry_after,
    retry_reason, fail_reason, created_at, updated_at, completed_at
FROM oor_client_sessions
WHERE session_id = $1;

-- name: FindOORClientOutgoingSessionByIdempotencyKey :one
SELECT session_id, direction, state, idempotency_key, retry_after,
    retry_reason, fail_reason, created_at, updated_at, completed_at
FROM oor_client_sessions
WHERE direction = 'outgoing'
  AND idempotency_key = $1
LIMIT 1;

-- name: UpsertOORClientInput :exec
INSERT INTO oor_client_inputs (
    session_id, input_index, outpoint_hash, outpoint_index, amount_sat,
    pk_script, client_key_family, client_key_index, client_pub_key,
    operator_pub_key, exit_delay, vtxo_policy_template, owner_leaf_script,
    owner_leaf_policy, spend_witness_script, spend_control_block,
    condition_witness, required_sequence, required_locktime
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13,
    $14, $15, $16,
    $17, $18, $19
)
ON CONFLICT (session_id, input_index) DO UPDATE SET
    outpoint_hash = excluded.outpoint_hash,
    outpoint_index = excluded.outpoint_index,
    amount_sat = excluded.amount_sat,
    pk_script = excluded.pk_script,
    client_key_family = excluded.client_key_family,
    client_key_index = excluded.client_key_index,
    client_pub_key = excluded.client_pub_key,
    operator_pub_key = excluded.operator_pub_key,
    exit_delay = excluded.exit_delay,
    vtxo_policy_template = excluded.vtxo_policy_template,
    owner_leaf_script = excluded.owner_leaf_script,
    owner_leaf_policy = excluded.owner_leaf_policy,
    spend_witness_script = excluded.spend_witness_script,
    spend_control_block = excluded.spend_control_block,
    condition_witness = excluded.condition_witness,
    required_sequence = excluded.required_sequence,
    required_locktime = excluded.required_locktime;

-- name: ListOORClientInputs :many
SELECT session_id, input_index, outpoint_hash, outpoint_index, amount_sat,
    pk_script, client_key_family, client_key_index, client_pub_key,
    operator_pub_key, exit_delay, vtxo_policy_template, owner_leaf_script,
    owner_leaf_policy, spend_witness_script, spend_control_block,
    condition_witness, required_sequence, required_locktime
FROM oor_client_inputs
WHERE session_id = $1
ORDER BY input_index;

-- name: UpsertOORClientRecipient :exec
INSERT INTO oor_client_recipients (
    session_id, output_index, pk_script, value_sat, vtxo_policy_template
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (session_id, output_index) DO UPDATE SET
    pk_script = excluded.pk_script,
    value_sat = excluded.value_sat,
    vtxo_policy_template = excluded.vtxo_policy_template;

-- name: ListOORClientRecipients :many
SELECT session_id, output_index, pk_script, value_sat, vtxo_policy_template
FROM oor_client_recipients
WHERE session_id = $1
ORDER BY output_index;

-- name: UpsertOORClientCheckpoint :exec
INSERT INTO oor_client_checkpoints (
    session_id, checkpoint_index, phase, checkpoint_psbt, created_at,
    updated_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6
)
ON CONFLICT (session_id, checkpoint_index, phase) DO UPDATE SET
    checkpoint_psbt = excluded.checkpoint_psbt,
    updated_at = excluded.updated_at;

-- name: GetOORClientCheckpoint :one
SELECT session_id, checkpoint_index, phase, checkpoint_psbt, created_at,
    updated_at
FROM oor_client_checkpoints
WHERE session_id = $1
  AND checkpoint_index = $2
  AND phase = $3;

-- name: ListOORClientCheckpointsByPhase :many
SELECT session_id, checkpoint_index, phase, checkpoint_psbt, created_at,
    updated_at
FROM oor_client_checkpoints
WHERE session_id = $1
  AND phase = $2
ORDER BY checkpoint_index;

-- name: ListOORClientCheckpoints :many
SELECT session_id, checkpoint_index, phase, checkpoint_psbt, created_at,
    updated_at
FROM oor_client_checkpoints
WHERE session_id = $1
ORDER BY checkpoint_index, phase;

-- name: UpsertOORClientArkArtifact :exec
INSERT INTO oor_client_ark_artifacts (
    session_id, phase, ark_psbt, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (session_id, phase) DO UPDATE SET
    ark_psbt = excluded.ark_psbt,
    updated_at = excluded.updated_at;

-- name: GetOORClientArkArtifact :one
SELECT session_id, phase, ark_psbt, created_at, updated_at
FROM oor_client_ark_artifacts
WHERE session_id = $1
  AND phase = $2;

-- name: ListOORClientArkArtifacts :many
SELECT session_id, phase, ark_psbt, created_at, updated_at
FROM oor_client_ark_artifacts
WHERE session_id = $1
ORDER BY phase;

-- name: UpsertOORClientIncomingHint :exec
INSERT INTO oor_client_incoming_hints (
    session_id, recipient_pk_script, recipient_event_id, created_at,
    updated_at
) VALUES (
    $1, $2, $3, $4,
    $5
)
ON CONFLICT (recipient_pk_script, recipient_event_id) DO UPDATE SET
    session_id = excluded.session_id,
    updated_at = excluded.updated_at;

-- name: GetOORClientIncomingHint :one
SELECT session_id, recipient_pk_script, recipient_event_id, created_at,
    updated_at
FROM oor_client_incoming_hints
WHERE session_id = $1;

-- name: UpsertOORClientIncomingMetadata :exec
INSERT INTO oor_client_incoming_metadata (
    session_id, output_index, round_id, chain_depth, batch_expiry,
    operator_pubkey, ancestry_blob, metadata_blob, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10
)
ON CONFLICT (session_id, output_index) DO UPDATE SET
    round_id = excluded.round_id,
    chain_depth = excluded.chain_depth,
    batch_expiry = excluded.batch_expiry,
    operator_pubkey = excluded.operator_pubkey,
    ancestry_blob = excluded.ancestry_blob,
    metadata_blob = excluded.metadata_blob,
    updated_at = excluded.updated_at;

-- name: ListOORClientIncomingMetadata :many
SELECT session_id, output_index, round_id, chain_depth, batch_expiry,
    operator_pubkey, ancestry_blob, metadata_blob, created_at, updated_at
FROM oor_client_incoming_metadata
WHERE session_id = $1
ORDER BY output_index;

-- name: InsertOORClientEffect :exec
INSERT INTO oor_client_effects (
    id, session_id, direction, effect_type, status, idempotency_key,
    attempts, max_attempts, next_attempt_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, 'pending', $5,
    0, $6, $7, $8, $8
)
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: ListDueOORClientEffectIDs :many
SELECT id
FROM oor_client_effects
WHERE next_attempt_at <= $1
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $1)
  )
ORDER BY next_attempt_at, created_at, id
LIMIT $2;

-- name: ClaimOORClientEffect :one
UPDATE oor_client_effects
SET status = 'claimed',
    claim_owner = $2,
    claim_token = $3,
    claim_until = $4,
    attempts = attempts + 1,
    updated_at = $5
WHERE id = $1
  AND next_attempt_at <= $5
  AND attempts < max_attempts
  AND (
    status = 'pending' OR
    (status = 'claimed' AND claim_until <= $5)
  )
RETURNING id, session_id, direction, effect_type, status, idempotency_key,
    attempts, max_attempts, next_attempt_at, claim_owner, claim_token,
    claim_until, last_error, created_at, updated_at, done_at;

-- name: MarkOORClientEffectDone :exec
UPDATE oor_client_effects
SET status = 'done',
    done_at = $3,
    updated_at = $3,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = NULL
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseOORClientEffectForRetry :exec
UPDATE oor_client_effects
SET status = CASE
        WHEN attempts >= max_attempts THEN 'dead'
        ELSE 'pending'
    END,
    next_attempt_at = $3,
    updated_at = $4,
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    last_error = $5
WHERE id = $1
  AND claim_token = $2
  AND status = 'claimed';

-- name: ReleaseExpiredOORClientEffectClaims :exec
UPDATE oor_client_effects
SET status = 'pending',
    claim_owner = NULL,
    claim_token = NULL,
    claim_until = NULL,
    updated_at = $2
WHERE status = 'claimed'
  AND claim_until <= $1;
