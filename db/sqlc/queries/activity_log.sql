-- Canonical activity log queries. activity_entries is the current-state
-- projection read by List; activity_events is the append-only transition log
-- read by a resumable SubscribeWallet. See docs/canonical_activity_log_design.md.

-- name: UpsertActivityEntry :exec
-- UpsertActivityEntry inserts the activity row or advances it in place. On
-- conflict the mutable lifecycle columns are overwritten with the new
-- projection and updated_at_unix is bumped, but created_at_unix is preserved so
-- the row keeps its position in the created-ordered feed. The settlement and
-- correlation handles are COALESCEd so an early projection that does not yet
-- know a txid never clobbers one a later projection already recorded.
INSERT INTO activity_entries (
    canonical_id, kind, status, amount_sat, fee_sat, counterparty, note,
    phase, phase_label, failure_code, failure_reason,
    payment_hash, txid, confirmation_height, vtxo_outpoint,
    swap_session_id, ledger_txid, boarding_addr, request_json,
    created_at_unix, updated_at_unix
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14, $15,
    $16, $17, $18, $19,
    $20, $21
)
ON CONFLICT (canonical_id) DO UPDATE SET
    kind           = EXCLUDED.kind,
    status         = EXCLUDED.status,
    amount_sat     = EXCLUDED.amount_sat,
    fee_sat        = EXCLUDED.fee_sat,
    counterparty   = EXCLUDED.counterparty,
    note           = EXCLUDED.note,
    phase          = EXCLUDED.phase,
    phase_label    = EXCLUDED.phase_label,
    failure_code   = EXCLUDED.failure_code,
    failure_reason = EXCLUDED.failure_reason,
    payment_hash        = COALESCE(EXCLUDED.payment_hash, activity_entries.payment_hash),
    txid                = COALESCE(EXCLUDED.txid, activity_entries.txid),
    confirmation_height = COALESCE(EXCLUDED.confirmation_height, activity_entries.confirmation_height),
    vtxo_outpoint       = EXCLUDED.vtxo_outpoint,
    swap_session_id = COALESCE(EXCLUDED.swap_session_id, activity_entries.swap_session_id),
    ledger_txid     = COALESCE(EXCLUDED.ledger_txid, activity_entries.ledger_txid),
    boarding_addr   = COALESCE(EXCLUDED.boarding_addr, activity_entries.boarding_addr),
    request_json    = EXCLUDED.request_json,
    updated_at_unix = EXCLUDED.updated_at_unix;

-- name: AppendActivityEvent :one
-- AppendActivityEvent records one immutable lifecycle-transition row and
-- returns the event_seq the database assigned (monotonic, not necessarily
-- contiguous). Callers use it as the resumable-subscribe cursor for the update.
INSERT INTO activity_events (
    canonical_id, status, phase, entry_json, created_at_unix
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING event_seq;

-- name: GetActivityEntry :one
-- GetActivityEntry returns one entry by its canonical id.
SELECT * FROM activity_entries WHERE canonical_id = $1;

-- name: CountActivityEntriesByStatus :one
-- CountActivityEntriesByStatus returns the number of current-state rows in the
-- given status. It backs the wallet status summary's pending count, which must
-- reflect the whole feed rather than a single paginated page.
SELECT COUNT(*) FROM activity_entries WHERE status = sqlc.arg(status);

-- name: ListActivityEntries :many
-- ListActivityEntries returns entries newest-first, paged by the immutable
-- (created_at_unix, canonical_id) cursor so a row that transitions in place
-- keeps its position. An empty cursor (created = 0) starts from the newest.
-- Callers pass limit_count + 1 and trim the extra row to compute has_more.
SELECT * FROM activity_entries
WHERE (
    CAST(sqlc.arg(cursor_created) AS BIGINT) = 0
    OR created_at_unix < CAST(sqlc.arg(cursor_created) AS BIGINT)
    OR (
        created_at_unix = CAST(sqlc.arg(cursor_created) AS BIGINT)
        AND canonical_id > sqlc.arg(cursor_id)
    )
)
ORDER BY created_at_unix DESC, canonical_id ASC
LIMIT sqlc.arg(limit_count);

-- name: ListEntriesByKindStatus :many
-- ListEntriesByKindStatus returns entries of the given kind and status, paged
-- by the unique canonical_id ascending. It backs the startup rehydration of
-- the wallet-local pending map: filtering in SQL keeps that scan O(matching
-- rows) instead of decoding the whole activity feed, and the canonical_id
-- cursor is strictly monotonic (a full page always advances it).
SELECT * FROM activity_entries
WHERE kind = sqlc.arg(kind)
    AND status = sqlc.arg(status)
    AND canonical_id > sqlc.arg(cursor_id)
ORDER BY canonical_id ASC
LIMIT sqlc.arg(limit_count);

-- name: PullActivityEvents :many
-- PullActivityEvents returns transition rows strictly after the cursor in
-- event_seq order, the resumable-subscribe replay primitive.
SELECT * FROM activity_events
WHERE event_seq > sqlc.arg(cursor)
ORDER BY event_seq ASC
LIMIT sqlc.arg(limit_count);
