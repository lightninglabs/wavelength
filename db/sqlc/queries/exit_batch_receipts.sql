-- name: BeginExitBatchRequest :execrows
INSERT INTO exit_batch_receipts (request_id, receipt) VALUES ($1, NULL)
ON CONFLICT (request_id) DO NOTHING;

-- name: AdmitExitBatchRequest :execrows
UPDATE exit_batch_receipts SET receipt = $2
WHERE request_id = $1 AND receipt IS NULL;

-- name: ResolveExitBatchRequest :one
INSERT INTO exit_batch_receipts (request_id, receipt) VALUES ($1, $2)
ON CONFLICT (request_id) DO UPDATE SET receipt =
    COALESCE(exit_batch_receipts.receipt, excluded.receipt)
RETURNING receipt;
