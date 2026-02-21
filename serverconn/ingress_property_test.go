package serverconn

import (
	"testing"

	"pgregory.net/rapid"
)

// TestIngress_AckNeverExceedsCommitted_Property validates that randomized ack
// and partial-dispatch progressions preserve the invariant:
// AckCommittedTo <= DispatchCommittedTo.
func TestIngress_AckNeverExceedsCommitted_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var state AckState
		steps := rapid.IntRange(1, 400).Draw(rt, "steps")

		for i := 0; i < steps; i++ {
			if state.NeedsAck() {
				ackSucceeds := rapid.Bool().Draw(
					rt, "ack_succeeds",
				)
				if ackSucceeds {
					state.AdvanceAck()
				}
			}

			dispatchFails := rapid.Bool().Draw(
				rt, "dispatch_fails",
			)
			commitDelta := rapid.Uint32Range(0, 4).Draw(
				rt, "commit_delta",
			)
			committedCursor := state.PullCursor +
				uint64(commitDelta)

			if dispatchFails {
				// Partial commit advances only work done
				// before the failure point.
				if committedCursor > state.PullCursor {
					state.AdvanceDispatch(committedCursor)
					state.PullCursor = committedCursor
				}
			} else {
				// Full batch commit may move past the last
				// event sequence processed in this loop.
				nextExtra := rapid.Uint32Range(0, 4).Draw(
					rt, "next_extra",
				)
				batchNext := committedCursor + uint64(nextExtra)

				state.AdvanceDispatch(batchNext)
				state.PullCursor = batchNext
			}

			if state.AckCommittedTo > state.DispatchCommittedTo {
				rt.Fatalf(
					"ack cursor > dispatch: %d > %d",
					state.AckCommittedTo,
					state.DispatchCommittedTo,
				)
			}

			if state.AckCommittedTo > state.PullCursor {
				rt.Fatalf(
					"ack cursor > pull: %d > %d",
					state.AckCommittedTo,
					state.PullCursor,
				)
			}
		}
	})
}

// TestIngress_PartialFailureCursor_Property models the inclusive→exclusive
// cursor conversion that ingressLoop applies when dispatchBatch returns a
// partial failure. dispatchBatch returns the inclusive event_seq of the last
// successfully dispatched envelope on error, so the caller must add 1 to get
// the exclusive next-pull position. This property test verifies that the
// converted cursor never re-includes the last committed envelope (which
// would cause duplicate dispatch).
func TestIngress_PartialFailureCursor_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var state AckState
		steps := rapid.IntRange(1, 400).Draw(rt, "steps")

		for i := 0; i < steps; i++ {
			if state.NeedsAck() {
				if rapid.Bool().Draw(rt, "ack_ok") {
					state.AdvanceAck()
				}
			}

			batchSize := rapid.Uint32Range(1, 8).Draw(
				rt, "batch_size",
			)
			dispatchFails := rapid.Bool().Draw(
				rt, "dispatch_fails",
			)

			// Model batchNextCursor as exclusive
			// (PullCursor + batchSize).
			batchNextCursor := state.PullCursor +
				uint64(batchSize)

			if dispatchFails {
				// Pick a failure point within the
				// batch. failAt is the 0-based index
				// of the envelope that fails.
				failAt := rapid.Uint32Range(
					0, batchSize-1,
				).Draw(rt, "fail_at")

				if failAt == 0 {
					// Nothing dispatched, no cursor
					// advance.
					continue
				}

				// lastCommitted is the inclusive
				// event_seq of the last OK envelope.
				// Event seqs start at PullCursor.
				lastCommitted := state.PullCursor +
					uint64(failAt) - 1

				// The ingressLoop converts inclusive
				// to exclusive by adding 1.
				nextCursor := lastCommitted + 1
				if nextCursor > state.PullCursor {
					state.AdvanceDispatch(nextCursor)
					state.PullCursor = nextCursor
				}
			} else {
				state.AdvanceDispatch(batchNextCursor)
				state.PullCursor = batchNextCursor
			}

			// Core invariants.
			if state.AckCommittedTo > state.DispatchCommittedTo {
				rt.Fatalf(
					"ack > dispatch: %d > %d",
					state.AckCommittedTo,
					state.DispatchCommittedTo,
				)
			}

			if state.AckCommittedTo > state.PullCursor {
				rt.Fatalf(
					"ack > pull: %d > %d",
					state.AckCommittedTo,
					state.PullCursor,
				)
			}

			// PullCursor must always be strictly past
			// the last committed envelope's event_seq to
			// prevent re-dispatch on the next pull.
			if state.DispatchCommittedTo > 0 &&
				state.PullCursor < state.DispatchCommittedTo {

				rt.Fatalf(
					"pull cursor behind dispatch: "+
						"%d < %d",
					state.PullCursor,
					state.DispatchCommittedTo,
				)
			}
		}
	})
}
