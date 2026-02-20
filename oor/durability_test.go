package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorFinalizeAfterRestart asserts that if the coordinator reaches
// the point-of-no-return and persists its session snapshot, it can be
// rehydrated from the database after a restart and still accept finalize.
func TestCoordinatorFinalizeAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	db1 := db.NewTestDB(t)
	sessionStore1 := NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	deliveryStore := newActorDeliveryStoreForTest(t, db1)

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	driver1 := NewDriver(DriverCfg{
		SessionStore: sessionStore1,
		OperatorKey:  keychain.KeyDescriptor{},
	})
	actor1 := NewActor(ActorCfg{
		OutboxHandler:    driver1,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
		SessionStore:     sessionStore1,
	})

	err := actor1.Start(ctx)
	require.NoError(t, err)

	submitResp := actor1.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok)

	state, err := actor1.CurrentState(ctx, submitMsg.SessionID)
	require.NoError(t, err)
	if failed, ok := state.(*FailedState); ok {
		t.Fatalf("submit failed before restart: %s", failed.Reason)
	}
	require.IsType(t, &CoSignedState{}, state)

	// Stop the first actor before constructing a second durable actor
	// that rehydrates state from the shared database.
	actor1.Stop()

	// Simulate restart with a new actor backed by the same DB and
	// delivery store. The DB-authoritative restart rebuilds active
	// sessions from persisted rows rather than TLV checkpoint blobs.
	sessionStore2 := NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)

	driver2 := NewDriver(DriverCfg{
		SessionStore: sessionStore2,
		OperatorKey:  keychain.KeyDescriptor{},
	})
	actor2 := NewActor(ActorCfg{
		OutboxHandler:    driver2,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
		SessionStore:     sessionStore2,
	})

	err = actor2.Start(ctx)
	require.NoError(t, err)
	defer actor2.Stop()

	finalizeResp := actor2.Receive(ctx, &FinalizeOORRequest{
		SessionID: submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
	})
	require.True(t, finalizeResp.IsOk())

	// The session is cleaned up from the in-memory map on terminal
	// states, so verify finalization via the response type.
	_, ok = finalizeResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)

	// Assert durable state is updated to finalized.
	row, err := db1.GetOORSession(
		ctx, sessionIDBytes(submitMsg.SessionID),
	)
	require.NoError(t, err)
	require.Equal(t, string(oorStateFinalized), row.State)
}
