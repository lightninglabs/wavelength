package db

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestOORIncomingSessionStoreRoundTrip ensures incoming snapshots survive SQL
// round-trips unchanged.
func TestOORIncomingSessionStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOORIncomingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	sessionID := testSessionID(t, 17)
	snapshot := &oor.IncomingSnapshot{
		Version:   1,
		SessionID: sessionID,
		Phase:     oor.IncomingPhaseNotified,
		ArkPSBT:   []byte{1, 2, 3},
		FinalCheckpointPSBTs: [][]byte{
			{9, 8},
			{7, 6},
		},
	}

	err := store.UpsertIncoming(ctx, snapshot)
	require.NoError(t, err)

	restored, err := store.GetIncoming(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, snapshot, restored)
}

// TestOORIncomingSessionStoreOverwrite ensures subsequent writes replace the
// stored snapshot.
func TestOORIncomingSessionStoreOverwrite(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOORIncomingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	sessionID := testSessionID(t, 19)

	first := &oor.IncomingSnapshot{
		Version:   1,
		SessionID: sessionID,
		Phase:     oor.IncomingPhaseNotified,
		ArkPSBT:   []byte{4, 5, 6},
	}
	err := store.UpsertIncoming(ctx, first)
	require.NoError(t, err)

	second := &oor.IncomingSnapshot{
		Version:   1,
		SessionID: sessionID,
		Phase:     oor.IncomingPhaseAwaitingAck,
	}
	err = store.UpsertIncoming(ctx, second)
	require.NoError(t, err)

	restored, err := store.GetIncoming(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, second, restored)
}

// TestOORIncomingSessionStoreMissing ensures unknown sessions return a missing
// snapshot error.
func TestOORIncomingSessionStoreMissing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOORIncomingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	_, err := store.GetIncoming(ctx, testSessionID(t, 21))
	require.Error(t, err)
	require.ErrorIs(t, err, oor.ErrIncomingSnapshotNotFound)
}
