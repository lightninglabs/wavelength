package db

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestOOROutgoingSessionStoreRoundTrip ensures snapshots survive SQL
// round-trips unchanged.
func TestOOROutgoingSessionStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOOROutgoingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	sessionID := testSessionID(t, 7)
	snapshot := &oor.OutgoingSnapshot{
		Version:   3,
		SessionID: sessionID,
		Phase:     oor.OutgoingPhaseFinalizeSent,
		ArkPSBT:   []byte{1, 2, 3},
		CheckpointPSBTs: [][]byte{
			{4, 5},
			{6, 7},
		},
		InputOutpoints: []wire.OutPoint{
			{
				Hash:  testHash(t, 31),
				Index: 2,
			},
		},
	}

	err := store.UpsertOutgoing(ctx, snapshot)
	require.NoError(t, err)

	restored, err := store.GetOutgoing(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, snapshot, restored)
}

// TestOOROutgoingSessionStoreOverwrite ensures subsequent writes replace the
// stored snapshot.
func TestOOROutgoingSessionStoreOverwrite(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOOROutgoingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	sessionID := testSessionID(t, 9)

	first := &oor.OutgoingSnapshot{
		Version:   3,
		SessionID: sessionID,
		Phase:     oor.OutgoingPhaseSubmitSent,
	}
	err := store.UpsertOutgoing(ctx, first)
	require.NoError(t, err)

	second := &oor.OutgoingSnapshot{
		Version:   3,
		SessionID: sessionID,
		Phase:     oor.OutgoingPhaseCoSigned,
		ArkPSBT:   []byte{9, 8, 7},
	}
	err = store.UpsertOutgoing(ctx, second)
	require.NoError(t, err)

	restored, err := store.GetOutgoing(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, second, restored)
}

// TestOOROutgoingSessionStoreMissing ensures unknown sessions return an error.
func TestOOROutgoingSessionStoreMissing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	store := NewOOROutgoingSessionStore(
		db, clock.NewDefaultClock(), btclog.Disabled,
	)

	_, err := store.GetOutgoing(ctx, testSessionID(t, 11))
	require.Error(t, err)
	require.ErrorIs(t, err, oor.ErrOutgoingSnapshotNotFound)
}

// testSessionID builds a deterministic session id from a one-byte seed.
func testSessionID(t *testing.T, seed byte) oor.SessionID {
	t.Helper()

	hash := testHash(t, seed)
	return oor.SessionID(hash)
}

// testHash builds a deterministic 32-byte hash from a one-byte seed.
func testHash(t *testing.T, seed byte) chainhash.Hash {
	t.Helper()

	var raw [32]byte
	for i := range raw {
		raw[i] = seed
	}

	hash, err := chainhash.NewHash(raw[:])
	require.NoError(t, err)

	return *hash
}
