package conn

import (
	"testing"

	"pgregory.net/rapid"
)

// TestAckState_MonotonicInvariants_Property verifies that random operation
// sequences preserve AckState invariants.
func TestAckState_MonotonicInvariants_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var state AckState
		steps := rapid.IntRange(1, 300).Draw(rt, "steps")

		for i := 0; i < steps; i++ {
			prev := state
			op := rapid.IntRange(0, 2).Draw(rt, "op")

			switch op {
			case 0:
				nextCursor := rapid.
					Uint64().Draw(rt, "next_cursor")
				state.AdvanceDispatch(nextCursor)

			case 1:
				state.AdvanceAck()

			case 2:
				// PullCursor can advance externally in ingress.
				// Keep that monotonic behavior.
				nextPull := rapid.Uint64().Draw(rt, "next_pull")
				if nextPull > state.PullCursor {
					state.PullCursor = nextPull
				}
			}

			if state.DispatchCommittedTo <
				prev.DispatchCommittedTo {

				rt.Fatalf("dispatch regressed: %d -> %d",
					prev.DispatchCommittedTo,
					state.DispatchCommittedTo)
			}

			if state.AckTarget < prev.AckTarget {
				rt.Fatalf("ack target regressed: %d -> %d",
					prev.AckTarget, state.AckTarget)
			}

			if state.AckCommittedTo < prev.AckCommittedTo {
				rt.Fatalf("ack committed regressed: %d -> %d",
					prev.AckCommittedTo,
					state.AckCommittedTo)
			}

			if state.PullCursor < prev.PullCursor {
				rt.Fatalf("pull cursor regressed: %d -> %d",
					prev.PullCursor, state.PullCursor)
			}

			if state.AckTarget < state.DispatchCommittedTo {
				rt.Fatalf("ack target below committed: %d < %d",
					state.AckTarget,
					state.DispatchCommittedTo)
			}

			if state.AckCommittedTo > state.AckTarget {
				rt.Fatalf("ack committed above target: %d > %d",
					state.AckCommittedTo, state.AckTarget)
			}

			if state.AckCommittedTo >
				state.DispatchCommittedTo {

				rt.Fatalf("ack committed above dispatch: "+
					"%d > %d", state.AckCommittedTo,
					state.DispatchCommittedTo)
			}

			if state.PullCursor < state.AckCommittedTo {
				rt.Fatalf("pull cursor behind ack: %d < %d",
					state.PullCursor, state.AckCommittedTo)
			}

			expectedNeedsAck := state.AckTarget >
				state.AckCommittedTo
			if state.NeedsAck() != expectedNeedsAck {
				rt.Fatalf("NeedsAck mismatch: got=%v "+
					"expected=%v", state.NeedsAck(),
					expectedNeedsAck)
			}
		}
	})
}
