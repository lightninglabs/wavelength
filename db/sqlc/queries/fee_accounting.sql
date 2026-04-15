-- name: InsertClientLedgerEntry :exec
INSERT INTO ledger_entries (
    debit_account, credit_account, amount_sat,
    round_id, session_id, event_type, description, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListClientLedgerEntries :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, event_type, description, created_at
FROM ledger_entries
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: ListClientLedgerEntriesByType :many
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, event_type, description, created_at
FROM ledger_entries
WHERE event_type = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetClientAccountBalance :one
SELECT CAST(COALESCE(
    (SELECT SUM(le1.amount_sat) FROM ledger_entries le1
     WHERE le1.debit_account = sqlc.arg(account_id)), 0
) - COALESCE(
    (SELECT SUM(le2.amount_sat) FROM ledger_entries le2
     WHERE le2.credit_account = sqlc.arg(account_id)), 0
) AS BIGINT) AS balance;

-- name: GetTotalOperatorFeesPaid :one
-- Returns cumulative Ark protocol fees paid to the operator (fees_paid
-- account only). Does not include L1 chain/miner fees (onchain_fees).
SELECT CAST(COALESCE(SUM(amount_sat), 0) AS BIGINT) AS total_fees
FROM ledger_entries
WHERE debit_account = 'fees_paid';

-- name: CountClientLedgerEntries :one
SELECT COUNT(*) FROM ledger_entries;

-- name: ListClientAccounts :many
SELECT account_id, account_name, account_type
FROM accounts
ORDER BY account_id;
