-- Credit operations control-plane queries.

-- name: UpsertCreditOperation :exec
INSERT INTO credit_operations (
    op_id, op_key, kind, state, status, server_op_id, payment_hash,
    destination_pubkey, oor_session_id, invoice, amount_sat, topup_sat,
    max_credit_sat, max_fee_sat, routing_fee_budget_sat, last_error,
    snapshot_data, snapshot_version, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20
)
ON CONFLICT (op_id) DO UPDATE SET
    op_key = EXCLUDED.op_key,
    kind = EXCLUDED.kind,
    state = EXCLUDED.state,
    status = EXCLUDED.status,
    server_op_id = EXCLUDED.server_op_id,
    payment_hash = EXCLUDED.payment_hash,
    destination_pubkey = EXCLUDED.destination_pubkey,
    oor_session_id = EXCLUDED.oor_session_id,
    invoice = EXCLUDED.invoice,
    amount_sat = EXCLUDED.amount_sat,
    topup_sat = EXCLUDED.topup_sat,
    max_credit_sat = EXCLUDED.max_credit_sat,
    max_fee_sat = EXCLUDED.max_fee_sat,
    routing_fee_budget_sat = EXCLUDED.routing_fee_budget_sat,
    last_error = EXCLUDED.last_error,
    snapshot_data = EXCLUDED.snapshot_data,
    snapshot_version = EXCLUDED.snapshot_version,
    updated_at = EXCLUDED.updated_at
;

-- name: GetCreditOperation :one
SELECT * FROM credit_operations
WHERE op_id = $1
;

-- name: LookupActiveCreditOperationByKey :one
-- Status 2 = Failed (anchored to Go iota in db/credit_operation_store.go
-- CreditOpStatus). Failed operations never dedup a keyed retry, so the lookup
-- skips them: only a pending or completed operation answers for an op_key.
SELECT * FROM credit_operations
WHERE op_key = $1 AND status != 2
;

-- name: ListNonTerminalCreditOperations :many
-- Status 1 = Completed, 2 = Failed (anchored to Go iota in
-- db/credit_operation_store.go CreditOpStatus).
SELECT * FROM credit_operations
WHERE status NOT IN (1, 2)
ORDER BY created_at ASC
;

-- name: ListAllCreditOperations :many
SELECT * FROM credit_operations
ORDER BY created_at ASC
;
