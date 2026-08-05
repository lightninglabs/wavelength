// ingress_deferral_test.p - Ingress deferral and redrive specification tests.

// TestIngressDeferral_BoundedMailboxRedriveNoLoss drives the production
// profile through everything the ingress loop cannot control: how much of the
// stream each pull asks for, how many times the store replays the transaction
// body before it commits, whether the fold commits at all, whether the target
// actor drains its bounded mailbox, and whether the whole client crashes
// between two turns.
//
// The contract is that no interleaving of those may ack an envelope nobody
// received, hand a bounded target the same envelope twice in one cycle, or
// answer a hoisted request twice in one process lifetime — and that once the
// target keeps up, the cursor reaches the end of the stream.
//
// The fault budget is what makes the run finite. While it lasts the driver
// may skip a drain, crash, or fail a fold; once it is spent the target always
// keeps up, so the loop is guaranteed to finish and the closing assertion is
// a real progress check rather than a bound on patience.
machine TestIngressDeferral_BoundedMailboxRedriveNoLoss {
    var pipeline: IngressPipelineSpec;

    start state Init {
        entry {
            var resp: (IngressStepResult, int);
            var faultBudget: int;
            var cycles: int;
            var batch: int;
            var attempts: int;
            var commits: bool;
            var total: int;
            var capacity: int;

            total = IngressStreamTotal();

            // The target's mailbox is deliberately explored at both sizes the
            // stream can tell apart, because the two expose different halves
            // of the contract. At one it fills on the first in-memory
            // envelope, so every deferral, redrive, and recovery is reachable.
            // At two it only fills when something redelivers, so a replayed
            // transaction body producing a second copy shows up as a
            // duplicate rather than hiding behind a deferral.
            capacity = 1;
            if ($) {
                capacity = 2;
            }

            pipeline = new IngressPipelineSpec((
                mode = NonParkingDeferral,
                track_tx_deliveries = true,
                served_watermark = true,
                capacity = capacity
            ));

            resp = (IngressStepCommitted, 1);
            faultBudget = 2;
            cycles = 0;

            while (resp.1 <= total && cycles < 16) {
                cycles = cycles + 1;

                // The target actor either keeps up or does not. Not keeping
                // up is what fills the bounded mailbox and produces the
                // backpressure episode.
                if (faultBudget > 0 && $) {
                    faultBudget = faultBudget - 1;
                } else {
                    send pipeline, eIngressDrain, (
                        reply_to = this, count = 2
                    );
                    receive { case eIngressStepResp:
                        (r0: (IngressStepResult, int)) { resp = r0; }
                    }
                }

                // Crash the whole client between two turns. The committed
                // cursor is the only survivor: the redrive clamp, the served
                // watermark, and the target's mailbox all go.
                if (faultBudget > 0 && $) {
                    faultBudget = faultBudget - 1;

                    send pipeline, eIngressCrash, this;
                    receive { case eIngressStepResp:
                        (r1: (IngressStepResult, int)) { resp = r1; }
                    }
                }

                batch = 1;
                if ($) {
                    batch = 2;
                } else if ($) {
                    batch = 3;
                }

                // A second attempt is the store replaying its transaction
                // body after a retryable error. The durable half of the first
                // attempt rolled back; the TryTells it already did did not.
                attempts = 1;
                if ($) {
                    attempts = 2;
                }

                commits = true;
                if (faultBudget > 0 && $) {
                    faultBudget = faultBudget - 1;
                    commits = false;
                }

                send pipeline, eIngressStep, (
                    reply_to = this,
                    batch = batch,
                    attempts = attempts,
                    commits = commits
                );
                receive { case eIngressStepResp:
                    (r2: (IngressStepResult, int)) { resp = r2; }
                }
            }

            assert resp.1 > total,
                "ingress redrive never reached the end of the stream even "+
                "though the target caught up";

            goto Done;
        }
    }

    state Done {}
}

// TestIngressDeferral_ParkingSendWedgesWriterCounterexample reproduces the
// shipped bug. The batch fills the cap-one mailbox at envelope 1 and meets it
// full at envelope 3, and the pre-fix profile answers that with a blocking
// send from inside the write transaction. The only ingress goroutine in the
// process is wedged there forever, holding the single writer, with nothing
// logged.
//
// There is no in-machine assertion: the park is raised solely by
// IngressWriterNeverParks, and the same run leaves
// IngressBacklogEventuallyDrains hot because a parked loop never commits
// another cursor.
machine TestIngressDeferral_ParkingSendWedgesWriterCounterexample {
    var pipeline: IngressPipelineSpec;

    start state Init {
        entry {
            var resp: (IngressStepResult, int);

            pipeline = new IngressPipelineSpec((
                mode = ParkingBlockingSend,
                track_tx_deliveries = true,
                served_watermark = true,
                capacity = 1
            ));

            send pipeline, eIngressStep, (
                reply_to = this, batch = 3, attempts = 1, commits = true
            );
            receive { case eIngressStepResp:
                (r0: (IngressStepResult, int)) { resp = r0; }
            }

            goto Done;
        }
    }

    state Done {}
}

// TestIngressDeferral_UntrackedRetryDuplicatesCounterexample reproduces the
// duplicate the per-invocation delivery record prevents. The store replays
// its transaction body once before committing, and without the record the
// second attempt hands envelope 1 to the bounded target again. The durable
// half replays harmlessly because the rollback undid it; the TryTell does
// not, because it was never in the transaction.
//
// The mailbox is given room for both copies on purpose: the bug under test is
// the duplicate, not the backpressure, and a cap-one mailbox would turn the
// second delivery into a deferral and hide it.
machine TestIngressDeferral_UntrackedRetryDuplicatesCounterexample {
    var pipeline: IngressPipelineSpec;

    start state Init {
        entry {
            var resp: (IngressStepResult, int);

            pipeline = new IngressPipelineSpec((
                mode = NonParkingDeferral,
                track_tx_deliveries = false,
                served_watermark = true,
                capacity = 3
            ));

            send pipeline, eIngressStep, (
                reply_to = this, batch = 3, attempts = 2, commits = true
            );
            receive { case eIngressStepResp:
                (r0: (IngressStepResult, int)) { resp = r0; }
            }

            goto Done;
        }
    }

    state Done {}
}

// TestIngressDeferral_UnwatermarkedServeDuplicatesCounterexample reproduces
// the duplicate the served watermark prevents, in the only arrangement that
// produces it: a hoisted request sitting PAST the envelope a full mailbox
// turned away.
//
// Turn one answers envelope 4 optimistically — the answer has to precede the
// commit — but the fold defers at envelope 3, so the cursor stops at 3 and
// envelope 4 stays in the pull window. Turn two is clamped to the deferred
// envelope, so envelope 4 is not even looked at; that is the clamp doing its
// half. Turn three finally pulls envelope 4 again, and with no watermark the
// operator gets a second answer to a request it already had answered. While
// the target stayed wedged this repeats once per backoff cycle, forever.
machine TestIngressDeferral_UnwatermarkedServeDuplicatesCounterexample {
    var pipeline: IngressPipelineSpec;

    start state Init {
        entry {
            var resp: (IngressStepResult, int);

            pipeline = new IngressPipelineSpec((
                mode = NonParkingDeferral,
                track_tx_deliveries = true,
                served_watermark = false,
                capacity = 1
            ));

            // Turn one: answer envelope 4, defer at envelope 3, commit the
            // cursor at 3.
            send pipeline, eIngressStep, (
                reply_to = this, batch = 5, attempts = 1, commits = true
            );
            receive { case eIngressStepResp:
                (r0: (IngressStepResult, int)) { resp = r0; }
            }

            // The target catches up, so the redrive can get past envelope 3.
            send pipeline, eIngressDrain, (reply_to = this, count = 1);
            receive { case eIngressStepResp:
                (r1: (IngressStepResult, int)) { resp = r1; }
            }

            // Turn two: clamped to envelope 3, which now fits. The cursor
            // reaches 4.
            send pipeline, eIngressStep, (
                reply_to = this, batch = 5, attempts = 1, commits = true
            );
            receive { case eIngressStepResp:
                (r2: (IngressStepResult, int)) { resp = r2; }
            }

            // Turn three: envelope 4 is pulled again, and with no watermark
            // it is answered a second time in the same incarnation.
            send pipeline, eIngressStep, (
                reply_to = this, batch = 5, attempts = 1, commits = true
            );
            receive { case eIngressStepResp:
                (r3: (IngressStepResult, int)) { resp = r3; }
            }

            goto Done;
        }
    }

    state Done {}
}

// tcIngressDeferralNoLoss checks the four safety contracts together against
// the production profile: no envelope is acked without being handled, a
// bounded target never sees an envelope twice in one folded dispatch or twice
// in one redrive epoch, a hoisted request is answered once per process
// lifetime, and the writer never parks.
test tcIngressDeferralNoLoss
    [main=TestIngressDeferral_BoundedMailboxRedriveNoLoss]:
  assert IngressCursorCoversOnlyHandledEnvelopes,
         IngressMemoryTargetNoTxScopedDuplicate,
         IngressMemoryTargetAtLeastOnceBounded,
         IngressNonTxRequestServedOncePerIncarnation,
         IngressWriterNeverParks in
  { IngressPipelineSpec,
    TestIngressDeferral_BoundedMailboxRedriveNoLoss };

// tcIngressDeferralLiveness checks the progress half: deferring on a full
// mailbox must delay the stream, never stop it, so a target that eventually
// keeps up lets the cursor reach the end.
test tcIngressDeferralLiveness
    [main=TestIngressDeferral_BoundedMailboxRedriveNoLoss]:
  assert IngressBacklogEventuallyDrains in
  { IngressPipelineSpec,
    TestIngressDeferral_BoundedMailboxRedriveNoLoss };

// tcIngressParkedWriterCounterexample runs the pre-fix blocking send with no
// in-machine assertion, so the wedge is raised solely by
// IngressWriterNeverParks. It is expected to find a bug.
test tcIngressParkedWriterCounterexample
    [main=TestIngressDeferral_ParkingSendWedgesWriterCounterexample]:
  assert IngressWriterNeverParks in
  { IngressPipelineSpec,
    TestIngressDeferral_ParkingSendWedgesWriterCounterexample };

// tcIngressParkedWriterStarvesCounterexample runs the same wedge under the
// progress monitor instead, so the failure shows up the way an operator sees
// it: the cursor stops advancing and the stream never drains. It is expected
// to find a bug.
test tcIngressParkedWriterStarvesCounterexample
    [main=TestIngressDeferral_ParkingSendWedgesWriterCounterexample]:
  assert IngressBacklogEventuallyDrains in
  { IngressPipelineSpec,
    TestIngressDeferral_ParkingSendWedgesWriterCounterexample };

// tcIngressUntrackedRetryDuplicateCounterexample runs a replayed transaction
// body with no per-invocation delivery record and no in-machine assertion, so
// the duplicate is raised solely by IngressMemoryTargetNoTxScopedDuplicate. It
// is expected to find a bug.
test tcIngressUntrackedRetryDuplicateCounterexample
    [main=TestIngressDeferral_UntrackedRetryDuplicatesCounterexample]:
  assert IngressMemoryTargetNoTxScopedDuplicate in
  { IngressPipelineSpec,
    TestIngressDeferral_UntrackedRetryDuplicatesCounterexample };

// tcIngressMonitorCatchesRetryDuplicate runs the same replay under the
// at-least-once bound instead, proving the duplicate is caught a second,
// independent way: two deliveries in one redrive epoch. It is expected to
// find a bug.
test tcIngressMonitorCatchesRetryDuplicate
    [main=TestIngressDeferral_UntrackedRetryDuplicatesCounterexample]:
  assert IngressMemoryTargetAtLeastOnceBounded in
  { IngressPipelineSpec,
    TestIngressDeferral_UntrackedRetryDuplicatesCounterexample };

// tcIngressUnwatermarkedServeCounterexample runs the redrive with no served
// watermark and no in-machine assertion, so the second answer is raised
// solely by IngressNonTxRequestServedOncePerIncarnation. It is expected to
// find a bug.
test tcIngressUnwatermarkedServeCounterexample
    [main=TestIngressDeferral_UnwatermarkedServeDuplicatesCounterexample]:
  assert IngressNonTxRequestServedOncePerIncarnation in
  { IngressPipelineSpec,
    TestIngressDeferral_UnwatermarkedServeDuplicatesCounterexample };
