-- name: InsertAssetDeposit :execrows
-- InsertAssetDeposit records a deposit request before tapd funds it. A
-- request ID already recorded inserts nothing.
INSERT INTO asset_deposits (
    request_id, status, asset_ref, asset_amount, carrier_value_sat,
    fee_rate_sat_per_vbyte, target_conf, max_fee_sat, operator_key,
    exit_delay, funding_proofs, created_at, updated_at
) VALUES (
    $1, 'funding', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11
)
ON CONFLICT (request_id) DO NOTHING;

-- name: GetAssetDeposit :one
SELECT * FROM asset_deposits WHERE request_id = $1;

-- name: ListAssetDeposits :many
SELECT * FROM asset_deposits ORDER BY created_at, request_id;

-- name: ListAssetDepositsByStatus :many
SELECT * FROM asset_deposits WHERE status = $1
ORDER BY created_at, request_id;

-- name: MarkAssetDepositPublished :execrows
-- MarkAssetDepositPublished stores the funded output and its boarding claim.
-- Only a deposit still funding moves.
UPDATE asset_deposits SET
    status = 'published',
    outpoint_hash = $2,
    outpoint_index = $3,
    value_sat = $4,
    fee_sat = $5,
    pk_script = $6,
    policy_template = $7,
    owner_key_family = $8,
    owner_key_index = $9,
    owner_pubkey = $10,
    digest = $11,
    script_key = $12,
    taproot_asset_root = $13,
    op_true_witness = $14,
    last_error = '',
    updated_at = $15
WHERE request_id = $1 AND status = 'funding';

-- name: MarkAssetDepositConfirmed :execrows
-- MarkAssetDepositConfirmed stores the output's confirmed proof. Only a
-- published deposit moves.
UPDATE asset_deposits SET
    status = 'confirmed',
    proof_file = $2,
    confirmed_height = $3,
    last_error = '',
    updated_at = $4
WHERE request_id = $1 AND status = 'published';

-- name: SetAssetDepositError :exec
-- SetAssetDepositError records the latest failure of a deposit that has not
-- confirmed.
UPDATE asset_deposits SET
    last_error = $2,
    updated_at = $3
WHERE request_id = $1 AND status != 'confirmed';

-- name: GetAssetDepositByOutpoint :one
SELECT * FROM asset_deposits
WHERE outpoint_hash = $1 AND outpoint_index = $2;

-- name: InsertRoundAssetDeposit :exec
INSERT INTO round_asset_deposits (
    round_id, request_id, input_index, input_signature
) VALUES ($1, $2, $3, $4)
ON CONFLICT (round_id, request_id) DO UPDATE SET
    input_index = excluded.input_index,
    input_signature = excluded.input_signature;

-- name: GetRoundAssetDeposits :many
SELECT * FROM round_asset_deposits
WHERE round_id = $1
ORDER BY input_index;

-- name: GetAssetDepositRound :one
-- GetAssetDepositRound returns the round that spends a deposit. A failed round
-- never broadcast its commitment, so it leaves the deposit free to board again.
SELECT rad.round_id, r.status
FROM round_asset_deposits AS rad
JOIN rounds AS r ON r.round_id = rad.round_id
WHERE rad.request_id = $1 AND r.status != 'failed'
ORDER BY r.creation_time DESC
LIMIT 1;

-- name: InsertAssetDepositClaim :execrows
-- InsertAssetDepositClaim records a signed claim. A deposit that already has
-- a claim inserts nothing.
INSERT INTO asset_deposit_claims (
    request_id, claim_txid, status, created_at, updated_at
) VALUES (
    $1, $2, 'claiming', $3, $3
)
ON CONFLICT (request_id) DO NOTHING;

-- name: GetAssetDepositClaim :one
SELECT * FROM asset_deposit_claims WHERE request_id = $1;

-- name: ListAssetDepositClaimsByStatus :many
SELECT * FROM asset_deposit_claims WHERE status = $1
ORDER BY created_at, request_id;

-- name: MarkAssetDepositClaimed :execrows
-- MarkAssetDepositClaimed records that tapd confirmed the claim.
UPDATE asset_deposit_claims SET
    status = 'claimed',
    updated_at = $2
WHERE request_id = $1 AND status = 'claiming';
