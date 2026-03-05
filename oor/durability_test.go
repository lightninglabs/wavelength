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

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	// Use actor1 without the durable runtime. We call Receive directly
	// to process the submit. The session is persisted to the DB by the
	// outbox driver's SessionStore, not by actor checkpoints.
	driver1 := NewDriver(DriverCfg{
		SessionStore: sessionStore1,
		OperatorKey:  keychain.KeyDescriptor{},
	})
	actor1 := NewActor(ActorCfg{
		OutboxHandler:    driver1,
		CheckpointPolicy: policy,
		SessionStore:     sessionStore1,
	})

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

	// Simulate restart with a new actor backed by the same DB. The
	// durable runtime's RestartMessage rebuilds active sessions from
	// persisted rows. Use Ask through the ref so finalize is ordered
	// after restart processing.
	deliveryStore := newActorDeliveryStoreForTest(t, db1)
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

	// Use the durable ref so the finalize is ordered after the restart
	// message that rebuilds session state from the database.
	finalizeFut := actor2.Ref().Ask(ctx, &FinalizeOORRequest{
		SessionID: submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
	})
	finalizeResult := finalizeFut.Await(ctx)
	require.True(t, finalizeResult.IsOk())

	// The session is cleaned up from the in-memory map on terminal
	// states, so verify finalization via the response type.
	_, ok = finalizeResult.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)

	// Assert durable state is updated to finalized.
	row, err := db1.GetOORSession(
		ctx, sessionIDBytes(submitMsg.SessionID),
	)
	require.NoError(t, err)
	require.Equal(t, string(oorStateFinalized), row.State)
}
