package serverconn

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAckState_AdvanceDispatch_SetsAckTarget verifies that AdvanceDispatch
// moves both DispatchCommittedTo and AckTarget forward.
func TestAckState_AdvanceDispatch_SetsAckTarget(t *testing.T) {
	t.Parallel()

	var s AckState
	s.AdvanceDispatch(10)

	require.Equal(t, uint64(10), s.DispatchCommittedTo)
	require.Equal(t, uint64(10), s.AckTarget)
}

// TestAckState_AdvanceDispatch_Monotonic verifies that AdvanceDispatch never
// decreases DispatchCommittedTo or AckTarget.
func TestAckState_AdvanceDispatch_Monotonic(t *testing.T) {
	t.Parallel()

	var s AckState
	s.AdvanceDispatch(10)
	s.AdvanceDispatch(5) // Should be ignored.
	s.AdvanceDispatch(7) // Still lower, should be ignored.

	require.Equal(t, uint64(10), s.DispatchCommittedTo)
	require.Equal(t, uint64(10), s.AckTarget)

	// A higher value should advance.
	s.AdvanceDispatch(15)
	require.Equal(t, uint64(15), s.DispatchCommittedTo)
	require.Equal(t, uint64(15), s.AckTarget)
}

// TestAckState_AdvanceAck_UpdatesPullCursor verifies that AdvanceAck moves
// AckCommittedTo to AckTarget and advances PullCursor if needed.
func TestAckState_AdvanceAck_UpdatesPullCursor(t *testing.T) {
	t.Parallel()

	s := AckState{
		PullCursor:          0,
		DispatchCommittedTo: 10,
		AckTarget:           10,
		AckCommittedTo:      0,
	}

	s.AdvanceAck()

	require.Equal(t, uint64(10), s.AckCommittedTo)
	require.Equal(t, uint64(10), s.PullCursor,
		"PullCursor should advance to match AckCommittedTo")
}

// TestAckState_AdvanceAck_DoesNotRegressPullCursor verifies that AdvanceAck
// does not move PullCursor backward if it is already ahead.
func TestAckState_AdvanceAck_DoesNotRegressPullCursor(t *testing.T) {
	t.Parallel()

	s := AckState{
		PullCursor:          20,
		DispatchCommittedTo: 10,
		AckTarget:           10,
		AckCommittedTo:      0,
	}

	s.AdvanceAck()

	require.Equal(t, uint64(10), s.AckCommittedTo)
	require.Equal(t, uint64(20), s.PullCursor,
		"PullCursor should not regress")
}

// TestAckState_NeedsAck verifies that NeedsAck returns true only when
// AckTarget exceeds AckCommittedTo.
func TestAckState_NeedsAck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		state   AckState
		wantAck bool
	}{
		{
			name:    "zero state needs no ack",
			state:   AckState{},
			wantAck: false,
		},
		{
			name: "target ahead of committed needs ack",
			state: AckState{
				AckTarget:      10,
				AckCommittedTo: 5,
			},
			wantAck: true,
		},
		{
			name: "target equal to committed needs no ack",
			state: AckState{
				AckTarget:      10,
				AckCommittedTo: 10,
			},
			wantAck: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.wantAck, tc.state.NeedsAck())
		})
	}
}

// TestAckState_FullCycle verifies a complete dispatch-ack cycle: dispatch
// advances the state, ack catches up, and NeedsAck transitions correctly.
func TestAckState_FullCycle(t *testing.T) {
	t.Parallel()

	var s AckState

	// Initially no ack needed.
	require.False(t, s.NeedsAck())

	// Dispatch first batch.
	s.AdvanceDispatch(10)
	s.PullCursor = 10
	require.True(t, s.NeedsAck())

	// Ack catches up.
	s.AdvanceAck()
	require.False(t, s.NeedsAck())
	require.Equal(t, uint64(10), s.AckCommittedTo)
	require.Equal(t, uint64(10), s.PullCursor)

	// Dispatch second batch.
	s.AdvanceDispatch(25)
	s.PullCursor = 25
	require.True(t, s.NeedsAck())

	// Ack again.
	s.AdvanceAck()
	require.False(t, s.NeedsAck())
	require.Equal(t, uint64(25), s.AckCommittedTo)
}

// TestAckState_EncodeDecode verifies TLV round-trip serialization.
func TestAckState_EncodeDecode(t *testing.T) {
	t.Parallel()

	original := AckState{
		PullCursor:          42,
		DispatchCommittedTo: 100,
		AckTarget:           100,
		AckCommittedTo:      90,
	}

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	var decoded AckState
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))

	require.Equal(t, original, decoded)
}

// TestAckState_EncodeDecode_Zero verifies that zero-value state round-trips.
func TestAckState_EncodeDecode_Zero(t *testing.T) {
	t.Parallel()

	var original AckState

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	var decoded AckState
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))

	require.Equal(t, original, decoded)
}
