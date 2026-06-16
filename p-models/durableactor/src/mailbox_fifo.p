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

type DurableMailboxPeekNextReq = (
    reply_to: machine,
    mailbox_id: int,
    now: int
);

type DurableMailboxTokenReq = (
    reply_to: machine,
    id: int,
    lease_token: int
);

type DurableMailboxByIDReq = (
    reply_to: machine,
    id: int
);

type DurableMailboxNackReq = (
    reply_to: machine,
    id: int,
    lease_token: int,
    available_at: int
);

type DurableMailboxNackByIDReq = (
    reply_to: machine,
    id: int,
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

// DurableMailboxCommitReq models the durable actor's Read/Commit consume step.
// A behavior does its side-effect IO outside the writer transaction (the gap
// between leasing the row and committing here), then Commit folds the behavior
// effect, the dedup mark, and the lease-fenced ack into one atomic unit.
//
// `fenced` distinguishes the two designs:
//
//   * LeaseFencedCommit (fenced = true): the production Read/Commit path. The
//     effect is applied only when the lease token still matches, atomically
//     with the ack. A consumer whose lease expired and whose row was reclaimed
//     applies nothing (ErrLeaseLost is a no-op).
//
//   * Unfenced (fenced = false): the counterexample. The effect is applied
//     regardless of the lease token -- modeling a behavior whose domain write
//     is not bound to the fenced ack -- so a stale consumer double-applies the
//     effect under reclaim.
type DurableMailboxCommitReq = (
    reply_to: machine,
    id: int,
    lease_token: int,
    fenced: bool
);

// DurableMailboxStageReq models the durable actor's early-durable-write (Stage)
// primitive: a short, non-fenced writer transaction that advances behavior
// state BEFORE the side-effect IO, while the message is only consumed later by
// Commit. A behavior that must persist-before-broadcast (e.g. the unroll actor
// persisting a sweep transaction before handing it to txconfirm) stages here.
//
//   * effect_seq is the monotone checkpoint level the stage advances to (the
//     FSM transition index). A replay of an already-staged level is a no-op.
//
//   * sweep_seq identifies the downstream broadcast (e.g. the sweep txid) the
//     behavior will emit after this stage. `stable` selects the two designs:
//
//       - stable = true (production): the broadcast id is derived from the
//         staged checkpoint and persisted with it, so a replay reuses the same
//         id and the downstream (txconfirm) dedups it. At most one distinct
//         broadcast occurs per row.
//
//       - stable = false (counterexample): the behavior derives a fresh
//         broadcast id on every attempt (e.g. a sweep rebuilt with a new wallet
//         address), so a replay broadcasts a new distinct id -- the
//         double-broadcast the persist-before-broadcast / sweep-reuse rule
//         exists to prevent.
//
//   * lease_token / fenced select whether the stage validates the lease before
//     writing, mirroring the production Stage that fences on ExtendLease:
//
//       - fenced = true (production): the checkpoint write happens only when the
//         lease token still matches. A consumer whose lease was reclaimed
//         applies nothing, so it cannot regress a newer owner's checkpoint.
//
//       - fenced = false (counterexample): the write happens regardless of the
//         token, modeling the original unfenced Stage where a stale consumer
//         overwrites the checkpoint with its older state -- the lost-update /
//         checkpoint regression the fence prevents.
type DurableMailboxStageReq = (
    reply_to: machine,
    id: int,
    lease_token: int,
    effect_seq: int,
    sweep_seq: int,
    stable: bool,
    fenced: bool
);

event eDurableMailboxEnqueue: DurableMailboxEnqueueReq;
event eDurableMailboxLeaseNext: DurableMailboxLeaseNextReq;
event eDurableMailboxPeekNext: DurableMailboxPeekNextReq;
event eDurableMailboxAck: DurableMailboxTokenReq;
event eDurableMailboxAckByID: DurableMailboxByIDReq;
event eDurableMailboxNack: DurableMailboxNackReq;
event eDurableMailboxNackByID: DurableMailboxNackByIDReq;
event eDurableMailboxExpireLeasesAt: int;
event eDurableMailboxDeadLetter: DurableMailboxDeadLetterReq;
event eDurableMailboxCommit: DurableMailboxCommitReq;
event eDurableMailboxStage: DurableMailboxStageReq;

event eDurableMailboxOpResp: (int, DurableMailboxOpResult);
event eDurableMailboxLeaseResp: (int, DurableMailboxOpResult);
event eDurableMailboxPeekResp: (int, int, int, DurableMailboxOpResult);

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

// eDurableMailboxEffectApplied is announced when a Read/Commit consumer's
// behavior effect is durably applied during Commit. It carries the announcing
// mailbox machine and the row id: (mbox, id). The
// LeaseFencedCommitAppliesEffectAtMostOnce monitor uses it to assert the effect
// for any one row is applied at most once across the run, even when the row's
// lease expires mid-IO and the row is reclaimed and reprocessed.
event eDurableMailboxEffectApplied: (machine, int);

// eDurableMailboxCheckpointStaged is announced whenever a Stage advances a
// row's durable checkpoint to a strictly higher level. It carries
// (mbox, id, effect_seq). CheckpointAdvancesMonotonically uses it to assert a
// staged checkpoint never moves backward and a replay never re-stages a stale
// level as if it were new.
event eDurableMailboxCheckpointStaged: (machine, int, int);

// eDurableMailboxBroadcast is announced when a behavior performs the downstream
// broadcast IO that follows a Stage (e.g. handing the sweep tx to txconfirm).
// It carries (mbox, id, sweep_id) where sweep_id is the broadcast identity
// (e.g. the sweep txid). StagedEffectAppliedAtMostOnceUnderReplay asserts that
// across a Stage'd-but-unacked crash and replay, a row never broadcasts two
// DISTINCT ids -- the production design reuses the persisted id, so the replay
// re-broadcasts the same id and the downstream dedups it.
event eDurableMailboxBroadcast: (machine, int, int);

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

// NackMailboxRowByID is the leaseless failure path. Peek does not write a
// lease or pre-increment attempts, so the by-ID nack increments attempts here.
// It also clears any stale expired lease metadata left by an older leased
// claim; otherwise the persisted row no longer matches the empty-token
// leaseless state machine the actor layer observed.
fun NackMailboxRowByID(rows: seq[MailboxRow], id: int, available_at: int):
    seq[MailboxRow] {

    var row: MailboxRow;

    row = RowByID(rows, id);
    row = NewMailboxRow(
        row.id, row.mailbox_id, row.correlation_key, row.priority,
        available_at, NoLease(), NoLeaseToken(), row.attempts + 1,
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

    // checkpoint and sweepId model the behavior's durable, non-fenced staged
    // state, written by Stage ahead of the side-effect IO. checkpoint[id] is
    // the monotone FSM level; sweepId[id] is the persisted broadcast identity.
    // Both are spec-machine state, so a failed (lease-lost) Commit -- which only
    // touches rows -- never rolls them back: a staged write survives the
    // rolled-back ack, exactly as a Stage transaction commits independently of
    // the later fenced Commit.
    var checkpoint: map[int, int];
    var sweepId: map[int, int];

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

        on eDurableMailboxPeekNext do (
            req: DurableMailboxPeekNextReq
        ) {
            var id: int;
            var row: MailboxRow;

            id = ClaimNextMailboxRow(
                rows, req.mailbox_id, req.now, mode
            );
            if (id == NoMailboxRow()) {
                send req.reply_to, eDurableMailboxPeekResp,
                    (
                        NoMailboxRow(), NoLeaseToken(), 0,
                        MailboxOpNotFound
                    );
                return;
            }

            // Peek is read-only: it selects the same eligible row as LeaseNext
            // but does not mutate lease metadata or attempts. The actor-layer
            // contract is nevertheless an empty token, even if the persisted
            // row still has stale expired lease metadata from an older leased
            // claim.
            row = RowByID(rows, id);
            send req.reply_to, eDurableMailboxPeekResp,
                (id, NoLeaseToken(), row.attempts, MailboxOpOk);
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

        on eDurableMailboxAckByID do (req: DurableMailboxByIDReq) {
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

        on eDurableMailboxNackByID do (req: DurableMailboxNackByIDReq) {
            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            rows = NackMailboxRowByID(rows, req.id, req.available_at);
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpOk);
        }

        on eDurableMailboxExpireLeasesAt do (now: int) {
            rows = ExpireMailboxLeases(rows, now);
        }

        // Stage models the durable actor's early-durable-write: a short,
        // lease-fenced writer transaction that advances the behavior checkpoint
        // and records the upcoming broadcast identity BEFORE the side-effect IO,
        // without acking the message. A staged row stays claimable until a later
        // Commit consumes it.
        //
        // The fence (production: fenced = true) validates the lease token before
        // writing, mirroring the production Stage that fences on ExtendLease. A
        // checkpoint write is an OVERWRITE (SaveCheckpoint replaces the row), so
        // a stale consumer that wrote its older state would regress the
        // checkpoint. The fence is what keeps the write monotone: only the live
        // lease holder, which holds the newest state, writes. The unfenced
        // profile (fenced = false) writes regardless of the token, so a stale
        // consumer overwrites a newer checkpoint with an older level -- the
        // lost-update regression the fence prevents.
        on eDurableMailboxStage do (req: DurableMailboxStageReq) {
            var row: MailboxRow;
            var bcast: int;

            // Stage advances state only for a row that still exists. Once a
            // fenced Commit has removed the row, a stale consumer cannot stage
            // or broadcast against it.
            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            // Fence: under the production design the write happens only when the
            // lease token still matches. A reclaimed consumer applies nothing,
            // so it cannot regress the checkpoint. The unfenced profile skips
            // this gate to expose the regression.
            row = RowByID(rows, req.id);
            if (req.fenced && !TokenMatches(row, req.lease_token)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpTokenMismatch);
                return;
            }

            // The checkpoint write is an overwrite. Under the fence only the
            // live owner reaches here, so the level is non-decreasing; an
            // unfenced stale writer can lower it, which the monotonicity monitor
            // catches.
            checkpoint[req.id] = req.effect_seq;
            announce eDurableMailboxCheckpointStaged,
                (this, req.id, req.effect_seq);

            // Record the broadcast identity. The production design persists it
            // with the checkpoint and reuses it on replay (stable); the
            // counterexample re-derives a fresh id every attempt.
            if (req.stable) {
                if (!(req.id in sweepId)) {
                    sweepId[req.id] = req.sweep_seq;
                }
            } else {
                sweepId[req.id] = req.sweep_seq;
            }

            // Emit the downstream broadcast. The receiver dedups by id, so a
            // replay that reuses the persisted id is collapsed; a replay that
            // derives a fresh id is a distinct, second broadcast.
            bcast = sweepId[req.id];
            announce eDurableMailboxBroadcast, (this, req.id, bcast);
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpOk);
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

        // Commit models the durable actor's Read/Commit consume step: the
        // behavior effect, the dedup mark, and the lease-fenced ack land as one
        // atomic unit. The effect is announced (eDurableMailboxEffectApplied)
        // and the row is consumed (removed) per the design selected by
        // req.fenced.
        //
        // The ack itself ALWAYS validates the lease token, mirroring the
        // production ack `DELETE ... WHERE id AND lease_token`: a stale owner
        // can never consume the row. Under the fenced design the effect is gated
        // on the same token check, so it commits atomically with the ack and a
        // stale owner applies nothing. Under the unfenced (counterexample)
        // design the effect is applied even when the token is stale, so a
        // reclaimed-and-reprocessed row gets its effect applied twice -- the bug
        // the lease fence exists to prevent.
        on eDurableMailboxCommit do (req: DurableMailboxCommitReq) {
            var row: MailboxRow;
            var tokenOk: bool;

            // A missing row means it was already consumed (e.g. a reclaimer
            // committed first). The model deliberately reports this as
            // MailboxOpNotFound rather than the MailboxOpTokenMismatch of the
            // stale-but-present case below; the production ack collapses both
            // into a single 0-row DELETE -> ErrLeaseLost. The distinction is
            // cosmetic for safety: both are effect no-ops (neither reaches the
            // effect announce), so the at-most-once contract holds identically.
            if (!RowExists(rows, req.id)) {
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            row = RowByID(rows, req.id);
            tokenOk = TokenMatches(row, req.lease_token);

            if (tokenOk || !req.fenced) {
                announce eDurableMailboxEffectApplied, (this, req.id);
            }

            if (tokenOk) {
                rows = RemoveMailboxRow(rows, req.id);
                announce eDurableMailboxRowRemoved, (this, req.id);
                send req.reply_to, eDurableMailboxOpResp,
                    (req.id, MailboxOpOk);
                return;
            }

            // Stale token: the ack fenced the consume out. Under the fenced
            // design no effect was applied above; this is the ErrLeaseLost
            // no-op.
            send req.reply_to, eDurableMailboxOpResp,
                (req.id, MailboxOpTokenMismatch);
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

// LeaseFencedCommitAppliesEffectAtMostOnce is the safety contract for the
// durable actor's Read/Commit consume step. The Read/Commit path lets a
// behavior do side-effect IO outside the writer transaction, so the row's lease
// can expire mid-IO and the row can be reclaimed and reprocessed by a second
// consumer. The lease fence (the token-validated ack that the behavior effect
// commits atomically with) must ensure the effect is applied at most once
// across the whole run, regardless of how many consumers processed the row.
//
// The monitor namespaces state by (mbox, id) because one test case can spin up
// several DurableMailboxSpec machines that reuse row ids. It catches the
// unfenced design directly: a stale consumer that applies its effect after the
// row was reclaimed announces a second eDurableMailboxEffectApplied for the same
// (mbox, id), tripping the assertion with no in-machine check required.
spec LeaseFencedCommitAppliesEffectAtMostOnce observes
    eDurableMailboxEffectApplied {

    var applied: map[(machine, int), bool];

    start state Monitoring {
        on eDurableMailboxEffectApplied do (effect: (machine, int)) {
            assert !(effect in applied),
                "lease-fenced commit applied a message effect more than "+
                "once (a stale owner committed after the row was reclaimed)";

            applied[effect] = true;
        }
    }
}

// StagedEffectAppliedAtMostOnceUnderReplay is the headline safety contract for
// the early-durable-write (Stage) path. Because a Stage advances durable state
// ahead of the IO but the message is acked only later by Commit, a crash
// between the Stage and the Commit redelivers the message and replays the same
// event against the already-advanced state. The contract is that this replay
// must not produce a second DISTINCT downstream broadcast: the production design
// reuses the broadcast id persisted with the checkpoint, so the replay
// re-broadcasts the same id and the receiver (txconfirm) dedups it.
//
// The monitor namespaces state by (mbox, id). It catches the counterexample
// design directly: a behavior that re-derives a fresh broadcast id on replay
// announces a second, different eDurableMailboxBroadcast for the same (mbox,
// id), tripping the assertion with no in-machine check -- the double-broadcast
// the persist-before-broadcast / sweep-reuse rule prevents.
spec StagedEffectAppliedAtMostOnceUnderReplay observes
    eDurableMailboxBroadcast {

    var seen: map[(machine, int), bool];
    var broadcastId: map[(machine, int), int];

    start state Monitoring {
        on eDurableMailboxBroadcast do (b: (machine, int, int)) {
            var key: (machine, int);

            key = (b.0, b.1);

            if (key in seen) {
                assert broadcastId[key] == b.2,
                    "a row broadcast two distinct downstream effects across "+
                    "a Stage'd-but-unacked replay (double-broadcast)";
            } else {
                seen[key] = true;
                broadcastId[key] = b.2;
            }
        }
    }
}

// CheckpointAdvancesMonotonically guards the other half of the Stage contract:
// a staged checkpoint never moves backward. Every Stage write is an overwrite
// that announces its level, so a replayed (equal) level is fine but a lower
// level is a regression. Under the fence only the live lease holder writes, so
// the level is non-decreasing; an unfenced stale consumer that overwrites with
// its older level trips this directly -- the lost-update the fence prevents.
spec CheckpointAdvancesMonotonically observes
    eDurableMailboxCheckpointStaged {

    var high: map[(machine, int), int];

    start state Monitoring {
        on eDurableMailboxCheckpointStaged do (s: (machine, int, int)) {
            var key: (machine, int);

            key = (s.0, s.1);

            if (key in high) {
                assert s.2 >= high[key],
                    "a staged checkpoint regressed (a stale consumer "+
                    "overwrote a newer checkpoint with an older level)";
            }

            high[key] = s.2;
        }
    }
}

// =============================================================================
// Outbox fold model
// =============================================================================
//
// The CDC outbox publisher's per-message delivery step folds two writes -- the
// target-mailbox enqueue (the "Tell") and the outbox-row completion -- into a
// single write transaction (deliverMessage's ExecTx in baselib/actor). The
// contract this model pins down:
//
//   * Atomicity: the enqueue and the completion commit together or not at all.
//     A failure mid-fold rolls BOTH back, so the outbox row stays pending and
//     no orphan is left in the target mailbox; the row is redelivered after the
//     claim expires.
//
//   * Idempotent delivery: the target enqueue is keyed by the outbox id
//     (WithOutboxID -> ON CONFLICT (id) DO NOTHING), so a redelivery or a
//     concurrent-publisher reclaim never produces a second distinct delivery.
//
//   * Token-fenced completion: only the current claim owner completes the row.
//     A stale publisher's completion matches zero rows (it is a no-op, not a
//     failure), so the row stays pending for the live owner.
//
// The headline safety property is no lost messages: an outbox row must never be
// marked completed unless the target enqueue is durable. The two profiles:
//
//   * AtomicFold (production): one transaction. The completion can only be
//     observed together with a durable enqueue.
//
//   * SplitWrite (counterexample): the completion and the enqueue are
//     independent writes. A crash after the completion lands but before the
//     enqueue is durable leaves the outbox completed with the message never
//     delivered -- the lost message the fold's single transaction prevents.

enum OutboxFoldMode {
    AtomicFold,
    SplitWrite
}

type OutboxFoldIDReq = (
    reply_to: machine,
    id: int
);

type OutboxFoldClaimReq = (
    reply_to: machine,
    id: int,
    token: int
);

// OutboxFoldDeliverReq models the folded delivery transaction for one row.
// `token` is the publisher's claim token; completion is gated on it matching
// the row's current owner. `enqueue_ok` models whether the target enqueue step
// succeeds: false is the failure/crash that, under AtomicFold, must roll the
// whole transaction back.
type OutboxFoldDeliverReq = (
    reply_to: machine,
    id: int,
    token: int,
    enqueue_ok: bool
);

event eOutboxFoldEnqueue: OutboxFoldIDReq;
event eOutboxFoldClaim: OutboxFoldClaimReq;
event eOutboxFoldDeliver: OutboxFoldDeliverReq;
event eOutboxFoldResp: (int, DurableMailboxOpResult);

// eOutboxTargetEnqueued is announced when a delivery durably enqueues the
// message into the target mailbox; eOutboxCompleted when the outbox row is
// marked completed. Both carry (mbox, id). The monitors reconstruct the
// completed-implies-delivered and delivered-at-most-once contracts from this
// stream, namespaced by the announcing OutboxFoldSpec instance.
event eOutboxTargetEnqueued: (machine, int);
event eOutboxCompleted: (machine, int);

// OutboxFoldSpec is the idealized transactional outbox + target mailbox for the
// CDC delivery step. It is single-table per (outbox row, target enqueue) and
// scoped by id; the safety contract is independent of how many rows coexist.
machine OutboxFoldSpec {
    var mode: OutboxFoldMode;
    var present: map[int, bool];
    var completed: map[int, bool];
    var enqueued: map[int, bool];
    var token: map[int, int];

    start state Active {
        entry (fold_mode: OutboxFoldMode) {
            mode = fold_mode;
        }

        on eOutboxFoldEnqueue do (req: OutboxFoldIDReq) {
            if (req.id in present && present[req.id]) {
                send req.reply_to, eOutboxFoldResp,
                    (req.id, MailboxOpDuplicate);
                return;
            }

            present[req.id] = true;
            completed[req.id] = false;
            enqueued[req.id] = false;
            token[req.id] = NoLeaseToken();
            send req.reply_to, eOutboxFoldResp, (req.id, MailboxOpOk);
        }

        // Claim assigns the row's current owner token, mirroring
        // ClaimOutboxBatch. Only a pending row is claimable; a completed row is
        // terminal. A reclaim (after the prior claim expires) simply overwrites
        // the owner token, which is what fences a stale publisher out.
        on eOutboxFoldClaim do (req: OutboxFoldClaimReq) {
            if (!(req.id in present) || !present[req.id]) {
                send req.reply_to, eOutboxFoldResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            if (completed[req.id]) {
                send req.reply_to, eOutboxFoldResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            token[req.id] = req.token;
            send req.reply_to, eOutboxFoldResp, (req.id, MailboxOpOk);
        }

        on eOutboxFoldDeliver do (req: OutboxFoldDeliverReq) {
            var tokenOk: bool;

            if (!(req.id in present) || !present[req.id]) {
                send req.reply_to, eOutboxFoldResp,
                    (req.id, MailboxOpNotFound);
                return;
            }

            // Completion is token-fenced: it lands only for the current owner.
            tokenOk = token[req.id] == req.token &&
                req.token != NoLeaseToken();

            if (mode == AtomicFold) {
                // One transaction. If the enqueue step fails the whole tx
                // rolls back: nothing is applied and the row stays pending for
                // redelivery after the claim expires.
                if (!req.enqueue_ok) {
                    send req.reply_to, eOutboxFoldResp,
                        (req.id, MailboxOpOk);
                    return;
                }

                // The enqueue committed: durable and idempotent by id, so a
                // redelivery or reclaim re-run announces no second delivery.
                if (!enqueued[req.id]) {
                    enqueued[req.id] = true;
                    announce eOutboxTargetEnqueued, (this, req.id);
                }

                // The completion is folded into the same commit. A stale token
                // matches zero rows -- a no-op, not a failure -- so the row
                // stays pending, exactly like CompleteOutbox's token-gated
                // UPDATE.
                if (tokenOk && !completed[req.id]) {
                    completed[req.id] = true;
                    announce eOutboxCompleted, (this, req.id);
                }

                send req.reply_to, eOutboxFoldResp, (req.id, MailboxOpOk);
                return;
            }

            // SplitWrite (counterexample): the completion and the enqueue are
            // independent, unsynchronized writes. The dangerous interleaving
            // commits the completion even though the enqueue did not durably
            // land (enqueue_ok = false), leaving the row completed with the
            // message never delivered -- a lost message.
            if (tokenOk && !completed[req.id]) {
                completed[req.id] = true;
                announce eOutboxCompleted, (this, req.id);
            }

            if (req.enqueue_ok && !enqueued[req.id]) {
                enqueued[req.id] = true;
                announce eOutboxTargetEnqueued, (this, req.id);
            }

            send req.reply_to, eOutboxFoldResp, (req.id, MailboxOpOk);
        }
    }
}

// OutboxCompletionImpliesDelivery is the no-lost-messages safety contract for
// the outbox fold: an outbox row may be marked completed only if the target
// enqueue is already durable. It reconstructs the durable-enqueue set from the
// announcement stream and, on every completion, asserts the row was delivered.
// The AtomicFold profile upholds it by construction (a completion is only
// observable together with the same transaction's durable enqueue); the
// SplitWrite counterexample trips it directly when a completion lands without a
// delivery, with no in-machine assertion required.
spec OutboxCompletionImpliesDelivery observes
    eOutboxTargetEnqueued, eOutboxCompleted {

    var enqueued: map[(machine, int), bool];

    start state Monitoring {
        on eOutboxTargetEnqueued do (e: (machine, int)) {
            enqueued[e] = true;
        }

        on eOutboxCompleted do (c: (machine, int)) {
            assert c in enqueued && enqueued[c],
                "outbox row marked completed without a durable target "+
                "enqueue (lost message)";
        }
    }
}

// OutboxTargetDeliveredAtMostOnce is the exactly-once half of the contract: the
// idempotent (ON CONFLICT id) target enqueue means a row is delivered to the
// target mailbox at most once, no matter how many times it is redelivered or
// reclaimed by concurrent publishers. A second distinct delivery for the same
// (mbox, id) trips the assertion.
spec OutboxTargetDeliveredAtMostOnce observes eOutboxTargetEnqueued {
    var seen: map[(machine, int), bool];

    start state Monitoring {
        on eOutboxTargetEnqueued do (e: (machine, int)) {
            assert !(e in seen),
                "outbox fold delivered two distinct copies to the target "+
                "mailbox for one row";
            seen[e] = true;
        }
    }
}
