// ingress_fold_test.p - Ingress cursor specification tests.

// TestIngressFold_AtomicBatchDispatchNoLoss drives the transactional
// ingress design through nondeterministic batch sizes, injected commit
// failures (rollbacks), and crash-restarts. The fold's contract is that the
// batch enqueues and the cursor advance are announced as one atomic commit,
// a failed commit announces nothing and resets the in-memory cursor to the
// durable one, and a crash reloads the durable cursor. The run must drain
// every envelope with no loss.
machine TestIngressFold_AtomicBatchDispatchNoLoss {
    start state Init {
        entry {
            var total: int;
            var durableCursor: int;
            var memCursor: int;
            var batchEnd: int;
            var faultBudget: int;
            var delivered: map[int, bool];
            var s: int;

            total = 3;
            durableCursor = 1;
            memCursor = 1;
            faultBudget = 2;

            while (durableCursor <= total) {
                // Pull a batch of one or two envelopes starting at the
                // in-memory cursor (which always matches the durable one
                // at the top of a healthy iteration).
                memCursor = durableCursor;
                batchEnd = memCursor;
                if ($ && batchEnd < total) {
                    batchEnd = batchEnd + 1;
                }

                if (faultBudget > 0 && $) {
                    // Commit failure: the transaction rolls back, so
                    // neither the enqueues nor the cursor advance are
                    // announced, and the in-memory cursor resets to the
                    // durable position for the re-pull.
                    faultBudget = faultBudget - 1;
                    memCursor = durableCursor;
                } else if (faultBudget > 0 && $) {
                    // Crash before the commit: restart reloads the
                    // durable cursor; nothing was announced.
                    faultBudget = faultBudget - 1;
                    memCursor = durableCursor;
                } else {
                    // Atomic commit: every enqueue in the batch and the
                    // advanced cursor become durable together.
                    s = memCursor;
                    while (s <= batchEnd) {
                        announce eIngressEnvelopeCommitted, (this, s);
                        delivered[s] = true;
                        s = s + 1;
                    }

                    durableCursor = batchEnd + 1;
                    announce eIngressCursorPersisted,
                        (this, durableCursor);
                }
            }

            // Liveness-by-construction: the loop only exits once the
            // durable cursor passed every envelope, and the monitor has
            // checked that the cursor never covered an undelivered one.
            s = 1;
            while (s <= total) {
                assert s in delivered,
                    "drained ingress loop left an envelope undelivered";
                s = s + 1;
            }

            goto Done;
        }
    }

    state Done {}
}

// TestIngressFold_EagerCursorLossCounterexample reproduces the bug the fold
// must avoid: the loop mutates its cursor state inside the transaction
// closure and keeps the advanced value after the commit ROLLS BACK. The
// next successful batch then persists a cursor that covers the rolled-back
// envelopes, which the monitor flags as message loss.
machine TestIngressFold_EagerCursorLossCounterexample {
    start state Init {
        entry {
            // Batch [1,2] dispatches, but the commit fails and rolls the
            // enqueues back, so neither enqueue is announced. The buggy
            // loop advanced its in-memory cursor to 3 anyway.
            //
            // Batch [3] then commits fine, persisting cursor 4 — which
            // claims envelopes 1 and 2 were delivered. They never were.
            announce eIngressEnvelopeCommitted, (this, 3);
            announce eIngressCursorPersisted, (this, 4);

            goto Done;
        }
    }

    state Done {}
}

// TestIngressFold_CheckpointBeforeEnqueueCounterexample reproduces the
// other ordering bug: persisting the cursor checkpoint in its own commit
// BEFORE the batch enqueues land. A crash between the two commits resumes
// past envelopes that never reached the local mailbox.
machine TestIngressFold_CheckpointBeforeEnqueueCounterexample {
    start state Init {
        entry {
            // Cursor checkpoint for batch [1,2] commits first ...
            announce eIngressCursorPersisted, (this, 3);

            // ... and the process crashes before the enqueue commit; the
            // envelopes are never announced. The monitor flags the
            // persisted cursor at announce time.
            goto Done;
        }
    }

    state Done {}
}

test tcIngressFoldNoLoss [main=TestIngressFold_AtomicBatchDispatchNoLoss]:
  assert IngressCursorCoversOnlyCommittedEnvelopes in
  { TestIngressFold_AtomicBatchDispatchNoLoss };

// tcIngressEagerCursorCounterexample runs the rolled-back-commit scenario
// with the buggy eager in-memory cursor. It is expected to find a bug.
test tcIngressEagerCursorCounterexample
    [main=TestIngressFold_EagerCursorLossCounterexample]:
  assert IngressCursorCoversOnlyCommittedEnvelopes in
  { TestIngressFold_EagerCursorLossCounterexample };

// tcIngressCheckpointFirstCounterexample runs the split-commit scenario
// where the checkpoint lands before the enqueues. It is expected to find a
// bug.
test tcIngressCheckpointFirstCounterexample
    [main=TestIngressFold_CheckpointBeforeEnqueueCounterexample]:
  assert IngressCursorCoversOnlyCommittedEnvelopes in
  { TestIngressFold_CheckpointBeforeEnqueueCounterexample };
