// ingress_fold.p - Connection-actor ingress cursor specification.
//
// Models the pull-dispatch-checkpoint loop run by the client's
// ServerConnectionActor and the operator's ClientConnectionActor against a
// remote mailbox. The loop pulls a batch of envelopes (identified by
// monotonically increasing event sequence numbers), enqueues each into a
// local durable mailbox, and persists the advanced PullCursor (exclusive
// next-pull position) in an AckState checkpoint.
//
// Distributed-systems contract:
//
//   * The persisted cursor must never cover an envelope whose local enqueue
//     did not durably commit: a cursor that runs ahead of the enqueues
//     silently skips those envelopes forever (message loss).
//   * The transactional fold makes the batch enqueues and the cursor
//     advance ONE atomic commit, so a dispatch failure or crash rolls back
//     both and the batch is re-pulled intact.
//   * Redelivery after rollback or crash is safe (at-least-once): the
//     rollback erased the partial enqueues, so the retry starts clean.
//
// The counterexample drivers in the test file demonstrate the two ways an
// implementation can break the contract: advancing the in-memory cursor
// past a rolled-back commit, and checkpointing the cursor in a separate
// commit issued before the enqueues land.
//
// Only WAITER-BACKED response envelopes are out of scope: a KIND_RESPONSE
// with a live in-memory unary waiter delivers to that waiter at most once,
// before and outside the dispatch transaction (it cannot roll back, and
// gating it on the writer lock starves RPC callers), so the cursor may
// cover it without a durable enqueue. The implementation classifies these
// at split time via the waiter registry.
//
// A response with NO live waiter is NOT out of scope: it falls back to
// durable route dispatch and folds into the SAME transaction as requests
// and events, so its enqueue commits atomically with the cursor exactly
// like any other durable envelope. This model therefore drives all such
// durable dispatches uniformly — a waiterless response is indistinguishable
// from a request/event here, and the no-loss property below guards it the
// same way. (Were the implementation to instead enqueue a waiterless
// response OUTSIDE the fold, ahead of the cursor commit, that would be the
// eager-cursor counterexample shape; the split keeps it inside the fold.)

// eIngressEnvelopeCommitted announces that the local enqueue of the given
// envelope sequence number durably committed, namespaced by the driver
// machine so concurrent test machines do not interfere.
event eIngressEnvelopeCommitted: (machine, int);

// eIngressCursorPersisted announces that an AckState checkpoint carrying
// the given exclusive PullCursor durably committed.
event eIngressCursorPersisted: (machine, int);

// IngressCursorCoversOnlyCommittedEnvelopes is the no-message-loss safety
// property: whenever a checkpoint persists cursor c, every envelope with
// sequence number below c must have a committed local enqueue. A violation
// means the loop would resume past envelopes that were never delivered.
spec IngressCursorCoversOnlyCommittedEnvelopes observes
    eIngressEnvelopeCommitted, eIngressCursorPersisted {

    var committed: map[(machine, int), bool];

    start state Monitoring {
        on eIngressEnvelopeCommitted do (enq: (machine, int)) {
            committed[(enq.0, enq.1)] = true;
        }

        on eIngressCursorPersisted do (cp: (machine, int)) {
            var s: int;

            s = 1;
            while (s < cp.1) {
                assert (cp.0, s) in committed,
                    "persisted cursor covers an envelope whose local "+
                    "enqueue never committed (message loss)";
                s = s + 1;
            }
        }
    }
}
