package credit

import (
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// TestCreditCodecRoundTrip asserts that ResumeCreditOpRequest -- the only
// application message that crosses a per-operation child's durable mailbox --
// encodes and decodes back to an equal value through the codec, so the child
// can reconstruct it across a restart. Every other credit message rides the
// supervisor's plain in-memory mailbox and is never serialized.
func TestCreditCodecRoundTrip(t *testing.T) {
	t.Parallel()

	codec := NewCodec()

	cases := []actor.TLVMessage{
		&ResumeCreditOpRequest{
			OpID:           "op-1",
			FromRetryTimer: true,
		},
	}

	for _, msg := range cases {
		msg := msg
		t.Run(msg.MessageType(), func(t *testing.T) {
			t.Parallel()

			raw, err := codec.Encode(msg)
			require.NoError(t, err)

			got, err := codec.Decode(raw)
			require.NoError(t, err)
			require.Equal(t, msg, got)
		})
	}
}

// TestStateStatusMapping asserts the FSM-state to durable-status projection
// used by the dedup index and the boot-time restore scan: only the two
// terminal states are terminal, everything else is pending.
func TestStateStatusMapping(t *testing.T) {
	t.Parallel()

	require.True(t, StateCompleted.IsTerminal())
	require.True(t, StateFailed.IsTerminal())

	nonTerminal := []State{
		StateQuoting, StateTopupCreating, StateTopupFunding,
		StateTopupAwaitingCredit, StatePaying, StateReceiveCreating,
		StateAwaitingSettlement, StateRedeemReserving, StateAwaitingOOR,
	}
	for _, s := range nonTerminal {
		require.False(t, s.IsTerminal(), "state %s", s)
		require.Equal(
			t, db.CreditOpStatusPending, s.Status(),
			"state %s", s,
		)
	}

	require.Equal(t, db.CreditOpStatusCompleted, StateCompleted.Status())
	require.Equal(t, db.CreditOpStatusFailed, StateFailed.Status())
}

// TestInitialState asserts each operation kind starts in the correct FSM entry
// state.
func TestInitialState(t *testing.T) {
	t.Parallel()

	require.Equal(t, StateQuoting, initialState(KindPay))
	require.Equal(t, StateReceiveCreating, initialState(KindReceive))
	require.Equal(t, StateRedeemReserving, initialState(KindRedeem))
}
