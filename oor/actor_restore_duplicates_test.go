package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func TestHandleRestoreSessionRejectsDuplicateSessionID(t *testing.T) {
	t.Parallel()

	behavior := &oorDurableBehavior{
		cfg: ClientActorCfg{
			ActorID:      "oor-duplicate-restore-test",
			SessionStore: newTestSessionStore(),
		},
		sessions: make(map[SessionID]*sessionHandle),
	}

	snapshot := &OutgoingSnapshot{
		Version:   2,
		SessionID: SessionID(chainhash.Hash{1, 2, 3}),
		Phase:     OutgoingPhaseCompleted,
	}

	first := behavior.handleRestoreSession(t.Context(),
		&RestoreSessionRequest{
			Snapshot: snapshot,
		},
	)
	require.True(t, first.IsOk())

	second := behavior.handleRestoreSession(t.Context(),
		&RestoreSessionRequest{
			Snapshot: snapshot,
		},
	)
	require.True(t, second.IsErr())
	require.ErrorContains(t, second.Err(), "duplicate session id")
}
