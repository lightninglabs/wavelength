package oor

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/stretchr/testify/require"
)

// TestActorIDForSession verifies the per-session actor id is deterministic,
// prefixed, and distinct per session.
func TestActorIDForSession(t *testing.T) {
	t.Parallel()

	var a, b chainhash.Hash
	a[0] = 0x01
	b[0] = 0x02

	idA := ActorIDForSession(SessionID(a))
	idB := ActorIDForSession(SessionID(b))

	require.True(t, strings.HasPrefix(idA, SessionActorIDPrefix))
	require.Equal(t, idA, ActorIDForSession(SessionID(a)))
	require.NotEqual(t, idA, idB)
	require.Contains(t, idA, a.String())
}

// TestOutgoingPhaseStatus verifies the outgoing phase to status mapping covers
// terminal and in-flight phases.
func TestOutgoingPhaseStatus(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, clientdb.OORSessionStatusCompleted,
		outgoingPhaseStatus(OutgoingPhaseCompleted),
	)
	require.Equal(
		t, clientdb.OORSessionStatusFailed,
		outgoingPhaseStatus(OutgoingPhaseFailed),
	)

	inFlight := []OutgoingPhase{
		OutgoingPhaseArkSignRequested,
		OutgoingPhaseSubmitSent,
		OutgoingPhaseCoSigned,
		OutgoingPhaseFinalizeSent,
		OutgoingPhaseLocalVTXOUpdate,
	}
	for _, phase := range inFlight {
		require.Equal(
			t, clientdb.OORSessionStatusPending,
			outgoingPhaseStatus(phase),
		)
	}
}

// TestIncomingPhaseStatus verifies the incoming phase to status mapping covers
// terminal and in-flight phases.
func TestIncomingPhaseStatus(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, clientdb.OORSessionStatusCompleted,
		incomingPhaseStatus(IncomingPhaseCompleted),
	)
	require.Equal(
		t, clientdb.OORSessionStatusFailed,
		incomingPhaseStatus(IncomingPhaseFailed),
	)

	inFlight := []IncomingPhase{
		IncomingPhaseResolvePending,
		IncomingPhaseMaterializePending,
		IncomingPhaseAckPending,
	}
	for _, phase := range inFlight {
		require.Equal(
			t, clientdb.OORSessionStatusPending,
			incomingPhaseStatus(phase),
		)
	}
}
