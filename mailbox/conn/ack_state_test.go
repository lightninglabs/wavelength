package conn

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
	s.AdvanceDispatch(5)
	s.AdvanceDispatch(7)

	require.Equal(t, uint64(10), s.DispatchCommittedTo)
	require.Equal(t, uint64(10), s.AckTarget)

	s.AdvanceDispatch(15)
	require.Equal(t, uint64(15), s.DispatchCommittedTo)
	require.Equal(t, uint64(15), s.AckTarget)
}

// TestAckState_AdvanceAck_UpdatesPullCursor verifies that AdvanceAck updates
// AckCommittedTo and advances PullCursor when needed.
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
	require.Equal(t, uint64(10), s.PullCursor)
}

// TestAckState_NeedsAck verifies NeedsAck behavior across key cases.
func TestAckState_NeedsAck(t *testing.T) {
	t.Parallel()

	s0 := AckState{}
	require.False(t, s0.NeedsAck())

	s1 := AckState{
		AckTarget:      10,
		AckCommittedTo: 5,
	}
	require.True(t, s1.NeedsAck())

	s2 := AckState{
		AckTarget:      10,
		AckCommittedTo: 10,
	}
	require.False(t, s2.NeedsAck())
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
