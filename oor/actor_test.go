package oor

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// testVTXOValue is the amount used for the single-input test VTXO.
const testVTXOValue = int64(1234)

// failOnceApplyFinalizeStore wraps a real SessionStore and fails the first
// ApplyFinalize call with a configured error, succeeding on retries.
type failOnceApplyFinalizeStore struct {
	SessionStore

	err   error
	calls int
}

// ApplyFinalize fails on the first call and delegates to the real store
// thereafter.
func (s *failOnceApplyFinalizeStore) ApplyFinalize(ctx context.Context,
	sessionID SessionID,
	finalCheckpointPSBTs []*psbt.Packet) error {

	if s.calls == 0 {
		s.calls++

		return s.err
	}

	return s.SessionStore.ApplyFinalize(
		ctx, sessionID, finalCheckpointPSBTs,
	)
}

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

// stripTapTreeMetadata removes the v0 OOR taptree metadata from a PSBT input.
func stripTapTreeMetadata(t *testing.T, pkt *psbt.Packet, inputIndex int) {
	t.Helper()

	require.NotNil(t, pkt)
	require.Greater(t, len(pkt.Inputs), inputIndex)

	unknowns := pkt.Inputs[inputIndex].Unknowns
	filtered := make([]*psbt.Unknown, 0, len(unknowns))

	for _, u := range unknowns {
		if u == nil {
			continue
		}

		if bytes.Equal(u.Key, oorlib.TapTreePSBTKey) {
			continue
		}

		filtered = append(filtered, u)
	}

	pkt.Inputs[inputIndex].Unknowns = filtered
}

// buildTestSubmitPackage constructs a minimal valid v0 OOR submit package.
func buildTestSubmitPackage(t *testing.T,
	recipients []oorlib.RecipientOutput) (
	scripts.CheckpointPolicy, *psbt.Packet, []*psbt.Packet,
) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	checkpointRes, err := oorlib.BuildCheckpointPSBT(
		policy, oorlib.CheckpointInput{
			SpentVTXO: oorlib.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash:  [32]byte{1},
					Index: 7,
				},
				Output: &wire.TxOut{
					Value:    testVTXOValue,
					PkScript: randomP2TRScript(t),
				},
			},
			OwnerLeafScript: []byte{txscript.OP_TRUE},
		},
	)
	require.NoError(t, err)

	if len(recipients) == 0 {
		recipients = []oorlib.RecipientOutput{
			{
				PkScript: randomP2TRScript(t),
				Value:    btcutil.Amount(testVTXOValue),
			},
		}
	}

	checkpointOutputs := []oorlib.CheckpointOutput{
		{
			Txid: checkpointRes.PSBT.UnsignedTx.TxHash(),
			Output: checkpointRes.PSBT.
				UnsignedTx.TxOut[0],
			TapTreeEncoded: checkpointRes.TapTreeEncoded,
		},
	}
	arkPsbt, err := oorlib.BuildArkPSBT(
		checkpointOutputs, recipients,
	)
	require.NoError(t, err)

	return policy, arkPsbt, []*psbt.Packet{checkpointRes.PSBT}
}

// buildFinalCheckpointPSBT creates a finalize checkpoint PSBT with placeholder
// signature material so structural finalize validation succeeds.
func buildFinalCheckpointPSBT(t *testing.T,
	checkpoint *psbt.Packet) *psbt.Packet {

	t.Helper()

	require.NotNil(t, checkpoint)
	require.NotNil(t, checkpoint.UnsignedTx)

	finalCheckpoint, err := psbt.NewFromUnsignedTx(
		checkpoint.UnsignedTx,
	)
	require.NoError(t, err)

	finalCheckpoint.Inputs[0].FinalScriptWitness = []byte{0x01}

	return finalCheckpoint
}

// startTestActor creates and starts a test actor instance.
func startTestActor(t *testing.T, cfg ActorCfg) *Actor {
	t.Helper()

	if cfg.DeliveryStore == nil {
		cfg.DeliveryStore = newActorDeliveryStoreWithNewDB(t)
	}

	a := NewActor(cfg)

	err := a.Start(t.Context())
	require.NoError(t, err)

	t.Cleanup(a.Stop)

	return a
}

// TestActorGetOrCreateSessionFSMConcurrent verifies concurrent access to the
// session map safely converges on a single handle instance.
func TestActorGetOrCreateSessionFSMConcurrent(t *testing.T) {
	t.Parallel()

	const workers = 32

	ctx := t.Context()
	sessionID := SessionID(chainhash.Hash{1})

	actor := NewActor(ActorCfg{})

	handles := make(chan *sessionHandle, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()

			handle, err := actor.behavior.getOrCreateSessionFSM(
				ctx, sessionID,
			)
			if err != nil {
				errs <- err
				return
			}

			handles <- handle
		}()
	}

	wg.Wait()
	close(handles)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var first *sessionHandle
	for handle := range handles {
		if first == nil {
			first = handle
			continue
		}

		require.Same(t, first, handle)
	}

	actor.behavior.sessionsMu.RLock()
	require.Len(t, actor.behavior.sessions, 1)
	actor.behavior.sessionsMu.RUnlock()
}

// TestActorHappyPath exercises a submit and finalize flow through the actor
// using the in-process outbox driver.
func TestActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, finalizeResp.IsOk())

	// Session is cleaned up from the map after reaching FinalizedState,
	// so we verify via the response type instead.
	_, ok = finalizeResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)
}

// TestActorSubmitMissingWitnessAssertsUnlock exercises a submit that fails
// validation because the Ark PSBT input does not include a witness UTXO.
func TestActorSubmitMissingWitnessAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	arkPsbt.Inputs[0].WitnessUtxo = nil

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorSubmitMissingTapTreeAssertsUnlock exercises a submit that fails
// validation because the Ark PSBT input does not include tap tree metadata.
func TestActorSubmitMissingTapTreeAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	stripTapTreeMetadata(t, arkPsbt, 0)

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorFinalizeMissingSigDoesNotUnlock asserts that finalize failures after
// the point-of-no-return do not emit an unlock request.
func TestActorFinalizeMissingSigDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &CoSignedState{}, state)

	// Finalize without FinalScriptWitness fails structural validation.
	finalCheckpoint, err := psbt.NewFromUnsignedTx(
		checkpointPsbts[0].UnsignedTx,
	)
	require.NoError(t, err)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, finalizeResp.IsErr())
	require.ErrorContains(t, finalizeResp.Err(), "finalize failed")

	// Session is cleaned up from the map after reaching FailedState,
	// so we verify the failure via the error response above.
	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.NotContains(t, seen, "UnlockInputsReq")
}

// TestActorFinalizeNotifyFailureIsRetryable asserts recipient event-store
// failures surface as finalize errors while keeping the session retryable.
func TestActorFinalizeNotifyFailureIsRetryable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	recipientEvents := &failingRecipientEventStore{
		err: errors.New("notify failed"),
	}
	driver := NewDriver(DriverCfg{RecipientEvents: recipientEvents})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	finalizeReq := &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	}

	// First finalize attempt fails because of the recipient event store
	// error.
	finalizeResp := actor.Receive(ctx, finalizeReq)
	require.True(t, finalizeResp.IsErr())
	require.ErrorContains(
		t, finalizeResp.Err(),
		"notify recipients failed: notify failed",
	)

	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	awaiting, ok := state.(*AwaitingRecipientsNotifyState)
	require.True(t, ok)
	require.Equal(t, "notify failed", awaiting.LastNotifyFailureReason)

	// Clear the error and retry succeeds.
	recipientEvents.err = nil

	retryResp := actor.Receive(ctx, finalizeReq)
	require.True(t, retryResp.IsOk())

	// Session is cleaned up after reaching FinalizedState, so we verify
	// via the response type.
	_, ok = retryResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)
}

// TestActorFinalizeSessionStoreFailureIsRetryable asserts finalize
// persistence errors are surfaced to the caller without terminalizing state.
func TestActorFinalizeSessionStoreFailureIsRetryable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	sqlStore := db.NewTestDB(t)
	sessionStore := NewDBSessionStore(
		sqlStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	failStore := &failOnceApplyFinalizeStore{
		SessionStore: sessionStore,
		err:          errors.New("apply finalize failed"),
	}

	driver := NewDriver(DriverCfg{
		SessionStore: failStore,
	})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	finalizeReq := &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	}

	// First finalize fails on session store persistence.
	finalizeResp := actor.Receive(ctx, finalizeReq)
	require.True(t, finalizeResp.IsErr())

	// Session should still be in a retryable state.
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &CoSignedState{}, state)

	// Retry succeeds.
	retryResp := actor.Receive(ctx, finalizeReq)
	require.True(t, retryResp.IsOk())

	// Session is cleaned up after reaching FinalizedState, so we verify
	// via the response type.
	_, ok := retryResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)
}

// TestActorSubmitNonCanonicalOutputsAssertsUnlock exercises a submit that fails
// because the Ark tx recipient outputs are not in canonical order.
func TestActorSubmitNonCanonicalOutputsAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	recipients := []oorlib.RecipientOutput{
		{
			PkScript: []byte{0x51},
			Value:    500,
		},
		{
			PkScript: []byte{0x52},
			Value:    btcutil.Amount(testVTXOValue) - 500,
		},
	}

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(
		t, recipients,
	)

	// BuildArkPSBT canonicalizes ordering. Break it by swapping the first
	// two recipient outputs while keeping the anchor in the final position.
	require.GreaterOrEqual(t, len(arkPsbt.UnsignedTx.TxOut), 3)
	outs := arkPsbt.UnsignedTx.TxOut
	outs[0], outs[1] = outs[1], outs[0]

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorSubmitAnchorNotLastAssertsUnlock exercises a submit that fails
// because the Ark tx anchor output is not the last output.
func TestActorSubmitAnchorNotLastAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	recipients := []oorlib.RecipientOutput{
		{
			PkScript: []byte{0x51},
			Value:    500,
		},
		{
			PkScript: []byte{0x52},
			Value:    btcutil.Amount(testVTXOValue) - 500,
		},
	}

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(
		t, recipients,
	)

	require.GreaterOrEqual(t, len(arkPsbt.UnsignedTx.TxOut), 3)
	outs := arkPsbt.UnsignedTx.TxOut
	last := len(outs) - 1
	outs[0], outs[last] = outs[last], outs[0]

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorSubmitMissingAnchorAssertsUnlock exercises a submit that fails
// because the Ark tx is missing the anchor output.
func TestActorSubmitMissingAnchorAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	require.GreaterOrEqual(t, len(arkPsbt.UnsignedTx.TxOut), 2)
	outs := arkPsbt.UnsignedTx.TxOut
	arkPsbt.UnsignedTx.TxOut = outs[:len(outs)-1]

	driver := NewDriver(DriverCfg{})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorLockConflictFailsWithoutUnlock asserts that if VTXO input locking
// fails (because another subsystem holds the lock), the session fails without
// emitting any unlock request.
func TestActorLockConflictFailsWithoutUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	inputOutpoint := checkpointPsbts[0].UnsignedTx.
		TxIn[0].PreviousOutPoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries,
		sqlStore.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)
	store := dbStore.NewVTXORecordStore()
	locker := db.NewVTXOLockerDB(sqlStore, btclog.Disabled)

	err := store.Create(ctx, &vtxo.Record{
		Outpoint: inputOutpoint,
		Value:    checkpointPsbts[0].Inputs[0].WitnessUtxo.Value,
		PkScript: checkpointPsbts[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	err = locker.LockMany(
		ctx, []wire.OutPoint{inputOutpoint},
		vtxo.RoundLockOwner("12345678-1234-1234-1234-123456789012"),
	)
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{Locker: locker})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &FailedState{}, state)

	seen := driver.SeenOutboxTypes()
	require.Contains(t, seen, "LockInputsReq")
	require.NotContains(t, seen, "UnlockInputsReq")
}

// TestActorOORLockBlocksRoundLock asserts that an accepted OOR submit holds a
// lock that prevents a round from concurrently locking the same input.
func TestActorOORLockBlocksRoundLock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	inputOutpoint := checkpointPsbts[0].UnsignedTx.
		TxIn[0].PreviousOutPoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries,
		sqlStore.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)
	store := dbStore.NewVTXORecordStore()
	locker := db.NewVTXOLockerDB(sqlStore, btclog.Disabled)

	err := store.Create(ctx, &vtxo.Record{
		Outpoint: inputOutpoint,
		Value:    checkpointPsbts[0].Inputs[0].WitnessUtxo.Value,
		PkScript: checkpointPsbts[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{Locker: locker})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	err = locker.LockMany(
		ctx, []wire.OutPoint{inputOutpoint},
		vtxo.RoundLockOwner("12345678-1234-1234-1234-123456789012"),
	)
	require.Error(t, err)

	var lockedErr *vtxo.ErrLocked
	require.ErrorAs(t, err, &lockedErr)
}

// TestActorFinalizeUpdatesVTXOStore asserts that finalize updates the shared
// VTXO store by marking inputs spent and materializing recipient outputs.
func TestActorFinalizeUpdatesVTXOStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Use multiple recipients to ensure we materialize multiple outputs.
	secondRecipientValue := btcutil.Amount(
		testVTXOValue - testVTXOValue/2,
	)
	recipients := []oorlib.RecipientOutput{
		{
			PkScript: randomP2TRScript(t),
			Value:    btcutil.Amount(testVTXOValue / 2),
		},
		{
			PkScript: randomP2TRScript(t),
			Value:    secondRecipientValue,
		},
	}

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(
		t, recipients,
	)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	inputOutpoint := checkpointPsbts[0].UnsignedTx.
		TxIn[0].PreviousOutPoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries,
		sqlStore.Backend(), btclog.Disabled,
		clock.NewDefaultClock(),
	)
	store := dbStore.NewVTXORecordStore()
	err := store.Create(ctx, &vtxo.Record{
		Outpoint: inputOutpoint,
		Value:    testVTXOValue,
		PkScript: checkpointPsbts[0].Inputs[0].WitnessUtxo.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	driver := NewDriver(DriverCfg{Store: store})
	actor := startTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, finalizeResp.IsOk())

	// Input should be marked spent.
	inRec, err := store.Get(ctx, inputOutpoint)
	require.NoError(t, err)
	require.NotNil(t, inRec)
	require.Equal(t, vtxo.StatusSpent, inRec.Status)

	// Recipient outputs should exist as live VTXOs (excluding anchor).
	arkTxid := arkPsbt.UnsignedTx.TxHash()
	expectedScripts := make(map[string]struct{}, len(recipients))
	for _, r := range recipients {
		expectedScripts[string(r.PkScript)] = struct{}{}
	}

	for i := 0; i < len(recipients); i++ {
		outRec, err := store.Get(ctx, wire.OutPoint{
			Hash:  arkTxid,
			Index: uint32(i),
		})
		require.NoError(t, err)
		require.NotNil(t, outRec)
		require.Equal(t, vtxo.StatusLive, outRec.Status)

		_, ok := expectedScripts[string(outRec.PkScript)]
		require.True(t, ok)
	}
}
