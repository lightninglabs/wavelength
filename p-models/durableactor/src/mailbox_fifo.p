// mailbox_fifo.p - Durable actor mailbox specification model.
//
// Distributed-systems contract, not just a regression:
//
//   * Enqueue is durable and idempotent by message id.
//   * Lease grants one temporary processing owner identified by a token.
//   * Ack/Nack/Extend require the current token; stale owners cannot commit.
//   * Lease expiry returns the same row to the available set for at-least-once
//     delivery.
//   * Nack does not remove the row; it schedules retry by moving available_at.
//   * Exhausted rows are no longer live predecessors for FIFO blocking.
//   * Per-correlation-key FIFO is a liveness-preserving compromise: only live
//     same-key predecessors block, so a stuck round/session does not stop the
//     entire actor mailbox.
//
// The key value 0 represents SQL NULL / unkeyed.

enum MailboxClaimMode {
    LegacyAvailableAtOrder,
    PerCorrelationKeyFIFO
}

enum DurableMailboxOpResult {
    MailboxOpOk,
    MailboxOpDuplicate,
    MailboxOpTokenMismatch,
    MailboxOpNotFound
}

type MailboxRow = (
    id: int,
    mailbox_id: int,
    correlation_key: int,
    priority: int,
    available_at: int,
    lease_until: int,
    lease_token: int,
    attempts: int,
    max_attempts: int,
    created_at: int
);

type DurableMailboxEnqueueReq = (
    reply_to: machine,
    row: MailboxRow
);

type DurableMailboxLeaseNextReq = (
    reply_to: machine,
    mailbox_id: int,
    lease_token: int,
    now: int,
    lease_duration: int
);

type DurableMailboxTokenReq = (
    reply_to: machine,
    id: int,
    lease_token: int
);

type DurableMailboxNackReq = (
    reply_to: machine,
    id: int,
    lease_token: int,
    available_at: int
);

// DurableMailboxDeadLetterReq carries no lease token: production
// MoveToDeadLetter(ctx, id, reason) addresses the row by id alone. The
// canonical dead-letter target is a retry-exhausted row, and a row only
// becomes exhausted by being leased/nacked, which clears its lease token.
// Gating dead-letter on the token would therefore make it unreachable for
// exactly the rows it exists to remove.
type DurableMailboxDeadLetterReq = (
    reply_to: machine,
    id: int
);

event eDurableMailboxEnqueue: DurableMailboxEnqueueReq;
event eDurableMailboxLeaseNext: DurableMailboxLeaseNextReq;
event eDurableMailboxAck: DurableMailboxTokenReq;
event eDurableMailboxNack: DurableMailboxNackReq;
event eDurableMailboxExpireLeasesAt: int;
event eDurableMailboxDeadLetter: DurableMailboxDeadLetterReq;

event eDurableMailboxOpResp: (int, DurableMailboxOpResult);
event eDurableMailboxLeaseResp: (int, DurableMailboxOpResult);

// eDurableMailboxClaimed is announced on every successful lease.
// eDurableMailboxRowEnqueued and eDurableMailboxRowRemoved let the FIFO monitor
// reconstruct the live per-lane row set (and each row's remaining retry budget)
// so it can check the per-correlation-key ordering contract independently of
// any in-machine assertion. Every event carries the announcing mailbox machine
// (mbox) as its first field: a single test case can instantiate many
// DurableMailboxSpec machines that reuse the same row ids, so the monitor must
// namespace its state by the originating mailbox instance. The payloads are:
//
//   eDurableMailboxClaimed:     (mbox, id, mailbox_id, correlation_key)
//   eDurableMailboxRowEnqueued: (mbox, id, mailbox_id, correlation_key,
//                                attempts, max_attempts)
//   eDurableMailboxRowRemoved:  (mbox, id)
event eDurableMailboxClaimed: (machine, int, int, int);
event eDurableMailboxRowEnqueued: (machine, int, int, int, int, int);
event eDurableMailboxRowRemoved: (machine, int);

// eMailboxWorkArrived and eMailboxWorkDrained are emitted only by the liveness
// driver to feed the non-starvation liveness monitor. Keeping them distinct
// from the row lifecycle events means the liveness monitor stays inert in the
// safety test cases that intentionally leave rows outstanding.
event eMailboxWorkArrived;
event eMailboxWorkDrained;

fun NoMailboxRow(): int {
    return 0;
}

fun NoLease(): int {
    return -1;
}

fun NoLeaseToken(): int {
    return 0;
}

fun NullCorrelationKey(): int {
    return 0;
}

fun NewMailboxRow(id: int, mailbox_id: int, correlation_key: int,
    priority: int, available_at: int, lease_until: int, lease_token: int,
    attempts: int, max_attempts: int, created_at: int): MailboxRow {

    return (
        id = id,
        mailbox_id = mailbox_id,
        correlation_key = correlation_key,
        priority = priority,
        available_at = available_at,
        lease_until = lease_until,
        lease_token = lease_token,
        attempts = attempts,
        max_attempts = max_attempts,
        created_at = created_at
    );
}

fun RowHasRetryBudget(row: MailboxRow): bool {
    return row.attempts < row.max_attempts;
}

fun RowLeaseIsAvailable(row: MailboxRow, now: int): bool {
    return row.lease_until == NoLease() || row.lease_until < now;
}

fun RowIsClaimVisible(row: MailboxRow, mailbox_id: int, now: int): bool {
    return row.mailbox_id == mailbox_id &&
           row.available_at <= now &&
           RowLeaseIsAvailable(row, now) &&
           RowHasRetryBudget(row);
}

fun HasEarlierLiveSameKey(rows: seq[MailboxRow], row: MailboxRow): bool {
    var i: int;
    var pred: MailboxRow;

    if (row.correlation_key == NullCorrelationKey()) {
        return false;
    }

    i = 0;
    while (i < sizeof(rows)) {
        pred = rows[i];

        if (pred.mailbox_id == row.mailbox_id &&
            pred.correlation_key == row.correlation_key &&
            pred.id < row.id &&
            RowHasRetryBudget(pred)) {

            return true;
        }

        i = i + 1;
    }

    return false;
}

fun RowEligibleUnderMode(rows: seq[MailboxRow], row: MailboxRow,
    mailbox_id: int, now: int, mode: MailboxClaimMode): bool {

    if (!RowIsClaimVisible(row, mailbox_id, now)) {
        return false;
    }

    if (mode == PerCorrelationKeyFIFO &&
        HasEarlierLiveSameKey(rows, row)) {

        return false;
    }

    return true;
}

// RowOrdersBefore mirrors the claim query's tie-breaking. The production SQL
// orders eligible rows by:
//
//   ORDER BY m.priority DESC, m.available_at ASC, m.created_at ASC
//
// (see db/actordelivery/queries/mailbox.sql). Those three axes are reproduced
// exactly here. The trailing id comparison is a model-only final tie-breaker:
// the SQL leaves rows that tie on (priority, available_at, created_at)
// unordered under LIMIT 1, whereas P requires a deterministic choice. The
// bridge traces keep created_at unique among same-(priority, available_at)
// rows, so the id fallback never disagrees with the SQL in practice.
fun RowOrdersBefore(row: MailboxRow, candidate: MailboxRow): bool {
    if (row.priority > candidate.priority) {
        return true;
    }

    if (row.priority < candidate.priority) {
        return false;
    }

    if (row.available_at < candidate.available_at) {
        return true;
    }

    if (row.available_at > candidate.available_at) {
        return false;
    }

    if (row.created_at < candidate.created_at) {
        return true;
    }

    if (row.created_at > candidate.created_at) {
        return false;
    }

    return row.id < candidate.id;
}

fun ClaimNextMailboxRow(rows: seq[MailboxRow], mailbox_id: int, now: int,
    mode: MailboxClaimMode): int {

    var i: int;
    var row: MailboxRow;
    var candidate: MailboxRow;
    var found: bool;

    i = 0;
    while (i < sizeof(rows)) {
        row = rows[i];

        if (RowEligibleUnderMode(rows, row, mailbox_id, now, mode)) {
            if (!found || RowOrdersBefore(row, candidate)) {
                found = true;
                candidate = row;
            }
        }

        i = i + 1;
    }

    if (!found) {
        return NoMailboxRow();
    }

    return candidate.id;
}

fun RowExists(rows: seq[MailboxRow], id: int): bool {
    var i: int;

    i = 0;
    while (i < sizeof(rows)) {
        if (rows[i].id == id) {
            return true;
        }

        i = i + 1;
    }

    return false;
}

fun RowByID(rows: seq[MailboxRow], id: int): MailboxRow {
    var i: int;

    i = 0;
    while (i < sizeof(rows)) {
        if (rows[i].id == id) {
            return rows[i];
        }

        i = i + 1;
    }

    // Callers must guard RowByID with RowExists. The sentinel is only here
    // to satisfy P's total-function requirement.
    return NewMailboxRow(
        NoMailboxRow(), 0, NullCorrelationKey(), 0, 0, NoLease(),
        NoLeaseToken(), 0, 0, 0
    );
}

fun RemoveMailboxRow(rows: seq[MailboxRow], id: int): seq[MailboxRow] {
    var result: seq[MailboxRow];
    var i: int;

    i = 0;
    while (i < sizeof(rows)) {
        if (rows[i].id != id) {
            result += (sizeof(result), rows[i]);
        }

        i = i + 1;
    }

    return result;
}

fun ReplaceMailboxRow(rows: seq[MailboxRow], row: MailboxRow):
    seq[MailboxRow] {

    var result: seq[MailboxRow];
    var i: int;

    i = 0;
    while (i < sizeof(rows)) {
        if (rows[i].id == row.id) {
            result += (sizeof(result), row);
        } else {
            result += (sizeof(result), rows[i]);
        }

        i = i + 1;
    }

    return result;
}

// LeaseMailboxRow grants a lease and increments attempts. attempts counts
// lease grants, not processing failures: the production LeaseNextMessage does
// attempts = attempts + 1 in the same UPDATE that sets the lease token (see
// db/actordelivery/queries/mailbox.sql). A row that is leased max_attempts
// times is therefore exhausted even if every lease was followed by an honest
// nack, which is why exhaustion is keyed on lease count here.
fun LeaseMailboxRow(rows: seq[MailboxRow], id: int, lease_token: int,
    lease_until: int): seq[MailboxRow] {

    var row: MailboxRow;

    row = RowByID(rows, id);
    row = NewMailboxRow(
        row.id, row.mailbox_id, row.correlation_key, row.priority,
        row.available_at, lease_until, lease_token, row.attempts + 1,
        row.max_attempts, row.created_at
    );

    return ReplaceMailboxRow(rows, row);
}

fun NackMailboxRow(rows: seq[MailboxRow], id: int, available_at: int):
    seq[MailboxRow] {

    var row: MailboxRow;

    row = RowByID(rows, id);
    row = NewMailboxRow(
        row.id, row.mailbox_id, row.correlation_key, row.priority,
        available_at, NoLease(), NoLeaseToken(), row.attempts,
        row.max_attempts, row.created_at
    );

    return ReplaceMailboxRow(rows, row);
}

fun ExpireMailboxLeases(rows: seq[MailboxRow], now: int): seq[MailboxRow] {
    var result: seq[MailboxRow];
    var i: int;
    var row: MailboxRow;

    i = 0;
    while (i < sizeof(rows)) {
        row = rows[i];
        if (row.lease_until != NoLease() && row.lease_until < now) {
            row = NewMailboxRow(
                row.id, row.mailbox_id, row.correlation_key,
                row.priority, row.available_at, NoLease(),
                NoLeaseToken(), row.attempts, row.max_attempts,
                row.created_at
            );
        }

        result += (sizeof(result), row);
        i = i + 1;
    }

    return result;
}

fun TokenMatches(row: MailboxRow, lease_token: int): bool {
    return row.lease_token != NoLeaseToken() &&
           row.lease_token == lease_token;
}

// DurableMailboxSpec is the idealized persistent mailbox. It is deliberately
// single-mailbox-table rather than single-actor: scoping by mailbox_id is part
// of the invariant under test.
machine DurableMailboxSpec {
    var rows: seq[MailboxRow];
    var mode: MailboxClaimMode;

    start state Active {
        entry (claim_mode: MailboxClaimMode) {
            mode = claim_mode;
        }

        on eDurableMailboxEnqueue do (req: DurableMailboxEnqueueReq) {
            if (RowExists(rows, req.row.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.row.id, MailboxOpDuplicate);
                return;
            }

            rows += (sizeof(rows), req.row);
            announce eDurableMailboxRowEnqueued, (
                this, req.row.id, req.row.mailbox_id,
                req.row.correlation_key, req.row.attempts,
                req.row.max_attempts
            );
            send req.reply_to, eDurableMailboxOpResp,
                (req.row.id, MailboxOpOk);
        }

        on eDurableMailboxLeaseNext do (
            req: DurableMailboxLeaseNextReq
        ) {
            var id: int;
            var row: MailboxRow;

            id = ClaimNextMailboxRow(
                rows, req.mailbox_id, req.now, mode
            );
            if (id == NoMailboxRow()) {
                send req.reply_to, eDurableMailboxLeaseResp,
                    (NoMailboxRow(), MailboxOpNotFound);
                return;
            }

            row = RowByID(rows, id);
            rows = LeaseMailboxRow(
                rows, id, req.lease_token,
                req.now + req.lease_duration
            );

            announce eDurableMailboxClaimed,
                (this, row.id, row.mailbox_id, row.correlation_key);
            send req.reply_to, eDurableMailboxLeaseResp,
                (id, MailboxOpOk);
        }

        on eDurableMailboxAck do (req: DurableMailboxTokenReq) {
            var row: MailboxRow;

            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            row = RowByID(rows, req.id);
            if (!TokenMatches(row, req.lease_token)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpTokenMismatch);
                return;
            }

            rows = RemoveMailboxRow(rows, req.id);
            announce eDurableMailboxRowRemoved, (this, req.id);
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpOk);
        }

        on eDurableMailboxNack do (req: DurableMailboxNackReq) {
            var row: MailboxRow;

            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            row = RowByID(rows, req.id);
            if (!TokenMatches(row, req.lease_token)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpTokenMismatch);
                return;
            }

            rows = NackMailboxRow(rows, req.id, req.available_at);
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpOk);
        }

        on eDurableMailboxExpireLeasesAt do (now: int) {
            rows = ExpireMailboxLeases(rows, now);
        }

        // Dead-letter removes a row by id, with no lease-token check, matching
        // production MoveToDeadLetter(ctx, id, reason). This is the path the
        // durable actor takes for a retry-exhausted row, whose lease token has
        // already been cleared by the preceding nack.
        on eDurableMailboxDeadLetter do (req: DurableMailboxDeadLetterReq) {
            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            rows = RemoveMailboxRow(rows, req.id);
            announce eDurableMailboxRowRemoved, (this, req.id);
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpOk);
        }
    }
}

// SameKeyFIFOClaimsRespectLiveHead is the global safety contract for the
// per-correlation-key FIFO rule. It reconstructs the live per-lane row set from
// the enqueue/claim/removal stream and, on every keyed claim, asserts that no
// earlier-id row in the same (mailbox_id, correlation_key) lane is still live
// (present and with retry budget remaining).
//
// This is strictly stronger than asserting claim ids are merely non-decreasing
// per lane. The production failure mode is a successor claimed while an
// earlier same-key row sits in nack/backoff: the claim ids 1 then 2 are
// non-decreasing, so a backwards-only check passes on the exact bug. Tracking
// liveness lets the monitor reject that claim directly, independent of any
// in-machine assertion. attempts are tracked the way LeaseMailboxRow updates
// them (one increment per claim), so an exhausted predecessor correctly stops
// blocking, matching the SQL anti-join's m2.attempts < m2.max_attempts.
spec SameKeyFIFOClaimsRespectLiveHead observes
    eDurableMailboxRowEnqueued, eDurableMailboxClaimed,
    eDurableMailboxRowRemoved {

    // State is namespaced by (mbox, id): the originating mailbox machine plus
    // the row id. rowLane maps that key to its (mailbox_id, correlation_key).
    var rowPresent: map[(machine, int), bool];
    var rowLane: map[(machine, int), (int, int)];
    var rowClaims: map[(machine, int), int];
    var rowMax: map[(machine, int), int];

    start state Monitoring {
        on eDurableMailboxRowEnqueued do (
            row: (machine, int, int, int, int, int)
        ) {
            var key: (machine, int);

            key = (row.0, row.1);
            rowPresent[key] = true;
            rowLane[key] = (row.2, row.3);
            rowClaims[key] = row.4;
            rowMax[key] = row.5;
        }

        on eDurableMailboxClaimed do (claim: (machine, int, int, int)) {
            var lane: (int, int);
            var key: (machine, int);
            var pred: (machine, int);

            key = (claim.0, claim.1);

            if (claim.3 != NullCorrelationKey()) {
                lane = (claim.2, claim.3);
                foreach (pred in keys(rowPresent)) {
                    if (pred.0 == claim.0 && rowPresent[pred] &&
                        pred.1 < claim.1 && rowLane[pred] == lane &&
                        rowClaims[pred] < rowMax[pred]) {

                        assert false,
                            "same-key claim overtook a live earlier "+
                            "head-of-line row";
                    }
                }
            }

            // Mirror LeaseMailboxRow: a claim consumes one retry attempt.
            if (key in rowClaims) {
                rowClaims[key] = rowClaims[key] + 1;
            }
        }

        on eDurableMailboxRowRemoved do (removed: (machine, int)) {
            if (removed in rowPresent) {
                rowPresent[removed] = false;
            }
        }
    }
}

// MailboxKeyedWorkEventuallyDrains is the liveness half of the per-key FIFO
// trade-off: per-correlation-key blocking must delay, never permanently starve.
// The liveness driver announces eMailboxWorkArrived for each enqueued row and
// eMailboxWorkDrained for each row it leases-and-acks. While any announced row
// is still outstanding the monitor sits in the hot Draining state, so a model
// in which an eligible row could never be claimed would leave the monitor hot
// forever and fail the liveness check. Only the liveness driver emits these
// events, so safety test cases that intentionally leave rows outstanding keep
// this monitor inert in its cold start state.
spec MailboxKeyedWorkEventuallyDrains observes
    eMailboxWorkArrived, eMailboxWorkDrained {

    var outstanding: int;

    start cold state Idle {
        ignore eMailboxWorkDrained;

        on eMailboxWorkArrived do {
            outstanding = outstanding + 1;
            goto Draining;
        }
    }

    hot state Draining {
        on eMailboxWorkArrived do {
            outstanding = outstanding + 1;
        }

        on eMailboxWorkDrained do {
            outstanding = outstanding - 1;
            if (outstanding == 0) {
                goto Idle;
            }
        }
    }
}
