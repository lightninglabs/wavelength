// ingress_deferral.p - Ingress dispatch deferral and redrive specification.
//
// ingress_fold.p pins down the cursor half of the connection actor's ingress
// loop: the persisted cursor may never cover an envelope whose local enqueue
// did not commit. This model pins down the delivery half, which the fold
// model deliberately abstracted away by treating every dispatch as a durable
// enqueue that lives and dies with the transaction.
//
// It does not. The dispatch table has four kinds of destination, and only one
// of them is transactional:
//
//   * A DURABLE target is enqueued inside the write transaction, so its
//     delivery commits with the cursor and rolls back with it.
//
//   * An IN-MEMORY target is a fixed-capacity actor mailbox. It is delivered
//     to with TryTell, so the send never parks, but the delivery is NOT in
//     the transaction: the target may have processed the message before the
//     transaction even tries to commit, and a rollback does not take it back.
//     A full mailbox is refused, which is a deferral rather than a failure.
//
//   * A nonTx REQUEST is answered over the network BEFORE the transaction
//     opens, because a round trip must never run under the writer lock. The
//     response is externally visible and cannot be recalled.
//
//   * A WAITER RESPONSE is handed to a live in-memory unary waiter, also
//     before the transaction and for the same reason.
//
// The contract this model states:
//
//   * No acked loss. The committed cursor may only cover envelopes that were
//     actually handled by one of the four paths. A deferral therefore commits
//     the prefix and stops AT the undelivered envelope, so the redrive
//     re-pulls it.
//
//   * No transaction-scoped duplicate. The store may run the transaction body
//     more than once before it commits (a retryable SQLITE_BUSY or Postgres
//     40001). The durable enqueues replay harmlessly because the rollback
//     undid them; the TryTells do not, so a per-invocation record of what was
//     already handed over suppresses the repeat.
//
//   * Bounded at-least-once. Across redrives and crashes an in-memory target
//     may see an envelope again, because redelivery is the documented
//     recovery, but at most once per redrive/crash epoch. Any more than that
//     is duplicate work the loop created for itself.
//
//   * A nonTx request is served at most once per process incarnation. The
//     serve happens ahead of the commit, so a crash in between genuinely can
//     repeat it: that residual is accepted and encoded, not asserted away.
//     What is NOT accepted is one serve per backoff cycle for as long as an
//     unrelated actor stays wedged, which is what the loop does without the
//     clamp and the served watermark.
//
//   * The writer never parks. A full in-memory mailbox must produce a
//     deferral, never a blocking send, and above all never a blocking send
//     from inside the write transaction.
//
//   * Progress. If the in-memory target eventually keeps up, the cursor
//     eventually reaches the end of the stream.
//
// The pre-fix implementation is reachable through IngressPipelineConfig, so
// the model can be shown to catch the bugs it exists for rather than merely
// agreeing with the fixed code. Three independent knobs, three failures:
// ParkingBlockingSend wedges the writer, a cleared track_tx_deliveries
// duplicates in-memory deliveries across store retries, and a cleared
// served_watermark re-answers a request once per backoff cycle.
//
// Two things are deliberately out of scope. The straggler re-fold (a waiter
// that vanishes between the split peek and the delivery, so its response
// folds back into the durable batch) is a cursor-ordering property, which is
// exactly what ingress_fold.p already covers. And a park is terminal here:
// the model exists to show the park is reachable at all, so it stops at the
// first one rather than modeling what would release it.

// IngressDispatchMode selects how a delivery to a FULL bounded in-memory
// mailbox behaves.
//
//   * NonParkingDeferral (production): the send is refused, the envelope is
//     left undelivered and unacked, and the loop commits the prefix before it
//     and re-pulls from the deferred envelope after a backoff.
//
//   * ParkingBlockingSend (counterexample): the send blocks until the target
//     drains, which on the folded path means the process's only mailbox
//     puller is parked with the database's single writer held. This is the
//     shipped behavior that made a deployed client go permanently deaf with
//     nothing logged.
enum IngressDispatchMode {
    NonParkingDeferral,
    ParkingBlockingSend
}

// IngressEnvelopeKind is the destination class of an envelope, which is what
// decides whether its delivery is inside the transaction, survives a
// rollback, or is visible outside the process entirely.
enum IngressEnvelopeKind {
    IngressDurableTarget,
    IngressMemoryTarget,
    IngressNonTxRequest,
    IngressWaiterResponse
}

// IngressStepResult reports what one pull-dispatch-commit cycle did, so the
// driver can decide what to do next without reaching into the pipeline's
// state.
enum IngressStepResult {
    // IngressStepCommitted: the whole clamped batch was handled and the
    // cursor advanced past it.
    IngressStepCommitted,

    // IngressStepDeferred: a full in-memory mailbox stopped the batch. The
    // prefix committed and the cursor sits on the undelivered envelope.
    IngressStepDeferred,

    // IngressStepRolledBack: the transaction failed outright, so nothing
    // committed and the cursor did not move.
    IngressStepRolledBack,

    // IngressStepParked: a blocking send met a full mailbox and the ingress
    // goroutine is wedged. Only reachable under ParkingBlockingSend.
    IngressStepParked,

    // IngressStepDrained: the cursor already passed the end of the stream.
    IngressStepDrained
}

// IngressPipelineConfig selects the implementation profile under test. The
// production profile is (NonParkingDeferral, true, true); each of the three
// counterexamples flips exactly one field.
type IngressPipelineConfig = (
    // mode selects the deferral or the pre-fix blocking send.
    mode: IngressDispatchMode,

    // track_tx_deliveries is the per-invocation record of what was already
    // handed to a bounded mailbox (deliveredOutsideTx). Cleared, a store that
    // replays its transaction body hands the same envelope over once per
    // attempt.
    track_tx_deliveries: bool,

    // served_watermark is the highest already-answered nonTx request
    // (redriveState.servedNonTxSeq). Cleared, a request that sits in the pull
    // window while a deferral is outstanding is answered again on every
    // redrive.
    served_watermark: bool,

    // capacity is the bounded in-memory mailbox's size.
    capacity: int
);

// IngressStepReq drives one pull-dispatch-commit cycle.
type IngressStepReq = (
    reply_to: machine,

    // batch is how many envelopes the pull asks for, before the redrive
    // clamp narrows it.
    batch: int,

    // attempts is how many times the store runs the transaction body. Every
    // attempt but the last rolls back, modeling a retryable store error.
    attempts: int,

    // commits is whether the final attempt commits. False is the fold that
    // fails outright: nothing durable lands and the cursor does not move,
    // while any TryTell the body already did stands.
    commits: bool
);

// IngressDrainReq models the target actor catching up on its own mailbox.
type IngressDrainReq = (
    reply_to: machine,
    count: int
);

event eIngressStep: IngressStepReq;
event eIngressDrain: IngressDrainReq;
// eIngressCrash models the whole client process going down and coming back.
// Its payload is the reply target, unwrapped: a one-field request record
// would carry no more information than the machine reference itself.
event eIngressCrash: machine;

// eIngressStepResp answers every pipeline request with the outcome and the
// committed cursor.
event eIngressStepResp: (IngressStepResult, int);

// eIngressEnvelopeHandled announces that an envelope reached its destination
// by whichever of the four paths owns it: a committed durable enqueue, a
// TryTell a bounded mailbox accepted, a nonTx request answered over the
// network, or a response handed to a live waiter. It carries (pipeline, seq).
// A rolled-back durable enqueue is NOT handled, which is the whole point of
// announcing on commit rather than on dispatch.
event eIngressEnvelopeHandled: (machine, int);

// eIngressCursorCommitted announces a durably committed exclusive cursor,
// carrying (pipeline, cursor).
event eIngressCursorCommitted: (machine, int);

// eIngressMemoryDelivered announces a TryTell that a bounded in-memory
// mailbox accepted, carrying (pipeline, seq, fold, epoch). fold is the
// invocation of the folded dispatch it happened in, which is the scope the
// per-invocation record covers; epoch is the redrive/crash cycle, which is
// the scope the at-least-once bound is stated over.
event eIngressMemoryDelivered: (machine, int, int, int);

// eIngressNonTxServed announces a hoisted request answered over the network,
// carrying (pipeline, seq, incarnation). The incarnation is the process
// lifetime: a repeat within one incarnation is the bug, a repeat across a
// restart is the accepted residual of serving ahead of the commit.
event eIngressNonTxServed: (machine, int, int);

// eIngressWriterParked announces a blocking send that met a full mailbox,
// carrying (pipeline, seq). Under the production profile it is unreachable.
event eIngressWriterParked: (machine, int);

// eIngressBacklogArrived and eIngressBacklogDrained feed the progress
// monitor: one arrival per envelope in the stream, one drain per envelope the
// committed cursor passes. They are kept distinct from the delivery events so
// the safety cases, which intentionally end with the stream unfinished, leave
// the liveness monitor inert in its cold start state.
event eIngressBacklogArrived;
event eIngressBacklogDrained;

// IngressStreamTotal is the number of envelopes on the remote mailbox. The
// stream is small on purpose: every property here is about the ORDER of a
// deferral relative to the other three delivery paths, not about volume, and
// a short stream keeps the schedule count CI can afford meaningful.
fun IngressStreamTotal(): int {
    return 5;
}

// IngressStreamKind is the remote mailbox's ordered stream. The layout is
// chosen so that one deferral exercises every interaction that matters:
//
//   1: in-memory target  - fills the bounded mailbox
//   2: durable target    - the prefix that must still commit under a deferral
//   3: in-memory target  - the deferral point, behind a full mailbox
//   4: nonTx request     - PAST the deferral point, so it is answered
//                          optimistically while the cursor stops short of it
//                          and it stays in the pull window across redrives
//   5: waiter response   - delivered in-memory ahead of the transaction
//
// Envelope 4 sitting after envelope 3 is the load-bearing part: it is the
// only arrangement in which the redrive clamp and the served watermark are
// both needed, and dropping either one repeats a network answer.
fun IngressStreamKind(eventSeq: int): IngressEnvelopeKind {
    if (eventSeq == 1 || eventSeq == 3) {
        return IngressMemoryTarget;
    }

    if (eventSeq == 2) {
        return IngressDurableTarget;
    }

    if (eventSeq == 4) {
        return IngressNonTxRequest;
    }

    return IngressWaiterResponse;
}

fun IngressMinInt(a: int, b: int): int {
    if (a < b) {
        return a;
    }

    return b;
}

// IngressDeferredCursor is the cursor to commit when a full mailbox stopped a
// batch partway. The deferred envelope's own sequence number is exactly where
// the exclusive cursor belongs: everything ahead of it has been handled by
// the time the deferral is raised, and the deferred envelope itself has not.
// The cursor never moves backwards, so a re-pull of already-committed
// envelopes cannot rewind it.
fun IngressDeferredCursor(deferredSeq: int, pullCursor: int): int {
    if (deferredSeq <= pullCursor) {
        return pullCursor;
    }

    return deferredSeq;
}

// IngressPipelineSpec is the idealized ingress dispatch pipeline: the remote
// mailbox's ordered stream, the loop's committed cursor and its loop-local
// redrive state, and the one bounded in-memory mailbox every deferral in this
// model comes from.
//
// It is a single machine rather than a puller plus a target actor because the
// properties under test are all about what ONE goroutine does in what order.
// The real ingress loop is a single dedicated goroutine with no pool, so
// interleaving a second puller would model a concurrency that does not exist.
// The target actor's independence is captured by eIngressDrain instead: the
// driver decides when it catches up, including never.
machine IngressPipelineSpec {
    var mode: IngressDispatchMode;
    var trackTxDeliveries: bool;
    var servedWatermark: bool;
    var capacity: int;
    var total: int;

    // cursor is the durable exclusive pull cursor: the only state that
    // survives a crash.
    var cursor: int;

    // occupancy is how full the bounded in-memory mailbox is.
    var occupancy: int;

    // deferredSeq and servedSeq are redriveState: loop-local, lost on
    // restart, and the two things that keep a redrive from repeating the
    // previous cycle's work.
    var deferredSeq: int;
    var servedSeq: int;

    // incarnation counts process lifetimes, epoch counts redrive-or-crash
    // cycles, and fold counts folded-dispatch invocations. Each is the scope
    // of one duplicate-suppression contract.
    var incarnation: int;
    var epoch: int;
    var fold: int;

    // parked records that a blocking send wedged the loop. It is sticky: the
    // model stops at the first park rather than modeling recovery.
    var parked: bool;

    // txDelivered is deliveredOutsideTx: the envelopes this fold invocation
    // already handed to the bounded mailbox. It is reset per invocation on
    // purpose. A record that spanned invocations would suppress the FIRST
    // delivery of an envelope whose earlier cycle rolled back after the
    // TryTell, trading a benign duplicate for a silent loss.
    var txDelivered: map[int, bool];

    // drainedSeq remembers which envelopes the progress monitor has already
    // been told about, so a cursor that advances twice does not announce the
    // same envelope twice.
    var drainedSeq: map[int, bool];

    start state Active {
        entry (cfg: IngressPipelineConfig) {
            var s: int;

            mode = cfg.mode;
            trackTxDeliveries = cfg.track_tx_deliveries;
            servedWatermark = cfg.served_watermark;
            capacity = cfg.capacity;
            total = IngressStreamTotal();

            cursor = 1;
            incarnation = 1;

            s = 1;
            while (s <= total) {
                announce eIngressBacklogArrived;
                s = s + 1;
            }
        }

        on eIngressStep do (req: IngressStepReq) {
            var result: IngressStepResult;

            result = RunStep(req.batch, req.attempts, req.commits);
            send req.reply_to, eIngressStepResp, (result, cursor);
        }

        on eIngressDrain do (req: IngressDrainReq) {
            var n: int;

            // The target actor works through its own mailbox. Nothing here
            // releases a park: a parked loop stays parked, because the model
            // stops at the first one.
            n = req.count;
            while (n > 0 && occupancy > 0) {
                occupancy = occupancy - 1;
                n = n - 1;
            }

            send req.reply_to, eIngressStepResp,
                (IngressStepDrained, cursor);
        }

        on eIngressCrash do (replyTo: machine) {
            // A restart rebuilds from the committed cursor and nothing else.
            // Every piece of loop-local state goes, and so does the target
            // actor's in-memory mailbox, because the whole process went down
            // with it. That is also why a crash is the one thing that clears
            // a wedged target: it is the operator's restart-fixes-it.
            incarnation = incarnation + 1;
            epoch = epoch + 1;
            deferredSeq = 0;
            servedSeq = 0;
            occupancy = 0;
            txDelivered = default(map[int, bool]);

            send replyTo, eIngressStepResp, (IngressStepDrained, cursor);
        }
    }

    // RunStep is one turn of the pull-dispatch-commit loop: pull a batch from
    // the cursor, narrow it to what a previous deferral left outstanding, do
    // the pre-transaction work, then run the transaction body until the store
    // stops retrying it.
    fun RunStep(batch: int, attempts: int, commits: bool): IngressStepResult {
        var pullEnd: int;
        var nextCursor: int;
        var clampEnd: int;
        var attempt: int;
        var deferredAt: int;
        var committing: bool;

        if (parked) {
            return IngressStepParked;
        }

        if (cursor > total) {
            return IngressStepDrained;
        }

        fold = fold + 1;

        pullEnd = IngressMinInt(cursor + batch - 1, total);
        nextCursor = pullEnd + 1;

        // Clamp a redriven batch to the envelopes at or before the one a full
        // mailbox turned away last cycle. Nothing past it can commit until it
        // is delivered, so handling the rest again is pure duplicate effort —
        // and for a nonTx request that duplicate effort is a second answer
        // sent to the operator. A batch with nothing at or before the
        // deferred envelope is left whole: that envelope is no longer on the
        // mailbox, so there is nothing left to protect, and clamping to an
        // empty batch would let the cursor advance over envelopes nobody
        // dispatched.
        //
        // The clamp and the served watermark overlap here, and the model says
        // so rather than flattering both: with the watermark in place,
        // removing the clamp breaks no property above, because the second
        // answer it would allow is the one the watermark suppresses. The
        // clamp earns its place as a bound on repeated WORK — a redrive that
        // re-walks the whole window every backoff cycle for as long as a
        // target stays wedged — and as the guard that still holds if a future
        // hoisted route needs per-envelope state the watermark cannot
        // summarize.
        if (deferredSeq != 0) {
            clampEnd = IngressMinInt(pullEnd, deferredSeq);
            if (clampEnd >= cursor && clampEnd < pullEnd) {
                pullEnd = clampEnd;
                nextCursor = deferredSeq + 1;
            }
        }

        RunPreTx(pullEnd);

        // The record is created out here, OUTSIDE the transaction, so a
        // replayed body can see what the previous attempt already handed to
        // the bounded mailbox and skip it.
        txDelivered = default(map[int, bool]);

        deferredAt = 0;
        attempt = 1;
        while (attempt <= attempts) {
            committing = commits && attempt == attempts;
            deferredAt = RunTxBody(pullEnd, nextCursor, committing);

            if (parked) {
                return IngressStepParked;
            }

            attempt = attempt + 1;
        }

        if (!commits) {
            // The fold failed outright. The cursor did not move and the
            // loop-local deferral state is untouched, so the next cycle
            // re-pulls the same window.
            epoch = epoch + 1;

            return IngressStepRolledBack;
        }

        if (deferredAt != 0) {
            deferredSeq = deferredAt;
            epoch = epoch + 1;

            return IngressStepDeferred;
        }

        deferredSeq = 0;

        return IngressStepCommitted;
    }

    // RunPreTx does the half of the fold that happens with no transaction
    // open, because neither part of it may run under the writer lock: a
    // waiter is an RPC caller sitting on a deadline, and a hoisted request is
    // a full network round trip.
    //
    // Both are irrevocable. A response has left the process by the time the
    // transaction opens, which is why the cursor commit that follows can only
    // ever repeat them, never take them back.
    fun RunPreTx(pullEnd: int) {
        var s: int;
        var kind: IngressEnvelopeKind;

        s = cursor;
        while (s <= pullEnd) {
            kind = IngressStreamKind(s);

            if (kind == IngressWaiterResponse) {
                announce eIngressEnvelopeHandled, (this, s);
            } else if (kind == IngressNonTxRequest) {
                // Already answered by an earlier cycle of this episode. The
                // cursor has not passed it yet, so the mailbox keeps handing
                // it back; answering again would send the operator a second
                // response to a request it has already had answered.
                if (!(servedWatermark && s <= servedSeq)) {
                    announce eIngressNonTxServed, (this, s, incarnation);
                    announce eIngressEnvelopeHandled, (this, s);
                }

                if (s > servedSeq) {
                    servedSeq = s;
                }
            }

            s = s + 1;
        }
    }

    // RunTxBody is one attempt at the write transaction: the durable prefix,
    // the TryTells into the bounded mailbox, and the cursor commit. It
    // returns the sequence number a full mailbox turned away, or zero.
    //
    // The asymmetry between the two target kinds is the whole model. A
    // durable enqueue is only observable when the attempt commits, because a
    // rolled-back attempt undid it. A TryTell is observable the moment it is
    // accepted, because it was never in the transaction to begin with.
    fun RunTxBody(pullEnd: int, nextCursor: int, commit: bool): int {
        var s: int;
        var kind: IngressEnvelopeKind;
        var cursorForCommit: int;
        var deferredAt: int;

        cursorForCommit = nextCursor;
        deferredAt = 0;

        s = cursor;
        while (s <= pullEnd && deferredAt == 0) {
            kind = IngressStreamKind(s);

            if (kind == IngressDurableTarget) {
                if (commit) {
                    announce eIngressEnvelopeHandled, (this, s);
                }
            } else if (kind == IngressMemoryTarget) {
                if (!(trackTxDeliveries && (s in txDelivered))) {
                    if (occupancy < capacity) {
                        occupancy = occupancy + 1;
                        txDelivered[s] = true;

                        announce eIngressMemoryDelivered,
                            (this, s, fold, epoch);
                        announce eIngressEnvelopeHandled, (this, s);
                    } else if (mode == ParkingBlockingSend) {
                        // The pre-fix path: a blocking send into a full
                        // fixed-capacity mailbox, from inside the write
                        // transaction. The only ingress goroutine in the
                        // process is now wedged with the single writer held,
                        // and nothing logs.
                        parked = true;
                        announce eIngressWriterParked, (this, s);

                        return 0;
                    } else {
                        // Backpressure, not a failed batch: the envelope is
                        // intact on the remote mailbox and the loop re-pulls
                        // it. Commit up to it and stop, because acking past
                        // an event that never reached its actor is how
                        // backpressure turns into a lost round event.
                        deferredAt = s;
                        cursorForCommit = IngressDeferredCursor(s, cursor);
                    }
                }
            }

            s = s + 1;
        }

        if (commit) {
            cursor = cursorForCommit;

            AnnounceDrainedBelow(cursor);
            announce eIngressCursorCommitted, (this, cursor);
        }

        return deferredAt;
    }

    // AnnounceDrainedBelow tells the progress monitor about every envelope
    // the committed cursor has now passed, once each.
    fun AnnounceDrainedBelow(c: int) {
        var s: int;

        s = 1;
        while (s < c) {
            if (!(s in drainedSeq)) {
                drainedSeq[s] = true;
                announce eIngressBacklogDrained;
            }

            s = s + 1;
        }
    }
}

// IngressCursorCoversOnlyHandledEnvelopes is the no-acked-loss safety
// property, and the reason the deferral commits the prefix instead of the
// batch. Whenever a cursor c is committed, every envelope below it must have
// been handled by one of the four delivery paths. A violation means the loop
// acked an envelope nobody received, and the remote mailbox is then free to
// garbage-collect it: the round event is gone for good.
//
// This is the delivery-side companion to
// IngressCursorCoversOnlyCommittedEnvelopes in ingress_fold.p, which states
// the same shape for the transactional half alone. The strengthening is that
// "handled" here also covers the three paths that are NOT in the transaction,
// so the monitor rejects a cursor that ran past an envelope a full mailbox
// refused — the way backpressure turns into loss.
spec IngressCursorCoversOnlyHandledEnvelopes observes
    eIngressEnvelopeHandled, eIngressCursorCommitted {

    var handled: map[(machine, int), bool];

    start state Monitoring {
        on eIngressEnvelopeHandled do (h: (machine, int)) {
            handled[h] = true;
        }

        on eIngressCursorCommitted do (c: (machine, int)) {
            var s: int;

            s = 1;
            while (s < c.1) {
                assert (c.0, s) in handled,
                    "committed cursor covers an envelope that no target "+
                    "ever received (acked loss)";
                s = s + 1;
            }
        }
    }
}

// IngressMemoryTargetNoTxScopedDuplicate is the safety contract for the
// per-invocation delivery record. A bounded in-memory target must not receive
// the same envelope twice within ONE folded dispatch, no matter how many
// times the store replays the transaction body.
//
// The failure it catches is invisible in production by construction: the
// final attempt commits, the loop reports success, and the target quietly
// processed the same round event once per retry. The store retries up to ten
// times, so a contended writer turns one event into ten deliveries with
// nothing logged.
spec IngressMemoryTargetNoTxScopedDuplicate observes eIngressMemoryDelivered {
    var delivered: map[(machine, int, int), bool];

    start state Monitoring {
        on eIngressMemoryDelivered do (d: (machine, int, int, int)) {
            var key: (machine, int, int);

            key = (d.0, d.1, d.2);

            assert !(key in delivered),
                "a bounded in-memory target received the same envelope "+
                "twice within one folded dispatch (a replayed transaction "+
                "body re-sent it)";

            delivered[key] = true;
        }
    }
}

// IngressMemoryTargetAtLeastOnceBounded states the delivery bound the design
// actually promises. Redelivery to an in-memory target is expected: the
// cursor has not moved, the remote mailbox keeps handing the envelope back,
// and the target has to absorb it. What is promised is that it happens at
// most once per redrive-or-crash epoch, so the duplicate count is bounded by
// how many times the loop went around rather than by how many internal steps
// each cycle took.
//
// The monitor states that as a strictly increasing epoch per envelope, which
// is stronger than counting: it rejects a second delivery in the SAME cycle
// however the cycle produced it, whether from a replayed transaction body or
// from a batch dispatched twice within one turn.
spec IngressMemoryTargetAtLeastOnceBounded observes eIngressMemoryDelivered {
    var lastEpoch: map[(machine, int), int];

    start state Monitoring {
        on eIngressMemoryDelivered do (d: (machine, int, int, int)) {
            var key: (machine, int);

            key = (d.0, d.1);

            if (key in lastEpoch) {
                assert d.3 > lastEpoch[key],
                    "a bounded in-memory target received the same envelope "+
                    "twice in one redrive epoch (at-least-once became "+
                    "unbounded)";
            }

            lastEpoch[key] = d.3;
        }
    }
}

// IngressNonTxRequestServedOncePerIncarnation is the bound on the one thing
// on this path that leaves the process before the commit. Hoisting a request
// out of the transaction moves its answer ahead of the cursor, so a crash in
// between leaves the cursor unmoved and the request is answered a second time
// on the re-pull. That residual is REAL and is encoded here rather than
// asserted away: the monitor keys on the process incarnation, so a repeat
// after a restart is permitted by construction.
//
// What the monitor does reject is a repeat WITHIN one incarnation. Without
// the redrive clamp and the served watermark, a request that sits in the pull
// window while an unrelated actor stays wedged is answered once per backoff
// cycle, indefinitely — a crash-bounded exposure turned into an unbounded
// one. Everything hoisted today is a read, so the duplicate is merely waste;
// the routes queued to be hoisted next are state-changing ones where it is
// not.
spec IngressNonTxRequestServedOncePerIncarnation observes eIngressNonTxServed {
    var served: map[(machine, int, int), bool];

    start state Monitoring {
        on eIngressNonTxServed do (r: (machine, int, int)) {
            var key: (machine, int, int);

            key = (r.0, r.1, r.2);

            assert !(key in served),
                "a hoisted nonTx request was answered twice in one process "+
                "incarnation (the redrive re-served it)";

            served[key] = true;
        }
    }
}

// IngressWriterNeverParks is the headline safety property, and the one the
// whole path was rebuilt for. Delivery into a bounded in-memory mailbox must
// never block the ingress goroutine, because that goroutine is the process's
// only consumer of the remote mailbox and, on the folded path, it holds the
// database's single writer while it works.
//
// A park is therefore not a slowdown. It is a permanently deaf client that
// still answers read RPCs, still reports healthy, and logs nothing — the
// exact shape of two production incidents. The production profile cannot
// reach the announcement at all; the ParkingBlockingSend profile reaches it
// on the first full mailbox.
spec IngressWriterNeverParks observes eIngressWriterParked {
    start state Monitoring {
        on eIngressWriterParked do (p: (machine, int)) {
            assert false,
                "ingress dispatch parked on a full in-memory mailbox: the "+
                "only mailbox puller is wedged, holding the write "+
                "transaction";
        }
    }
}

// IngressBacklogEventuallyDrains is the progress half of the trade-off.
// Deferring rather than blocking is only the right answer if backpressure
// DELAYS the stream instead of stopping it: a target that eventually keeps up
// must let the cursor eventually reach the end.
//
// The pipeline announces one arrival per envelope at startup and one drain
// per envelope the committed cursor passes, so a run that leaves the cursor
// short leaves the monitor hot. A parked writer never commits again and never
// drains anything, which is how the same wedge shows up here as a liveness
// failure rather than a safety one.
spec IngressBacklogEventuallyDrains observes
    eIngressBacklogArrived, eIngressBacklogDrained {

    var outstanding: int;

    start cold state Idle {
        ignore eIngressBacklogDrained;

        on eIngressBacklogArrived do {
            outstanding = outstanding + 1;
            goto Draining;
        }
    }

    hot state Draining {
        on eIngressBacklogArrived do {
            outstanding = outstanding + 1;
        }

        on eIngressBacklogDrained do {
            outstanding = outstanding - 1;
            if (outstanding == 0) {
                goto Idle;
            }
        }
    }
}
