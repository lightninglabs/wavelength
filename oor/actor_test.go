package oor

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
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

	ownerLeafScript := []byte{txscript.OP_TRUE}
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
			OwnerLeafScript: ownerLeafScript,
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

	leaf, err := oorlib.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeafScript,
	)
	require.NoError(t, err)
	arkPsbt.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	return policy, arkPsbt, []*psbt.Packet{checkpointRes.PSBT}
}

// buildTestSubmitPackageWithDescriptor constructs a valid submit package and
// returns the signing descriptor for the input VTXO.
func buildTestSubmitPackageWithDescriptor(t *testing.T,
	recipients []oorlib.RecipientOutput) (
	scripts.CheckpointPolicy, *psbt.Packet, []*psbt.Packet,
	VTXOSigningDescriptor, *btcec.PrivateKey, *btcec.PrivateKey,
) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	vtxoTapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	vtxoOutpoint := wire.OutPoint{
		Hash:  [32]byte{1},
		Index: 7,
	}

	ownerLeafScript := []byte{txscript.OP_TRUE}
	checkpointRes, err := oorlib.BuildCheckpointPSBT(
		policy, oorlib.CheckpointInput{
			SpentVTXO: oorlib.SpentVTXORef{
				Outpoint: vtxoOutpoint,
				Output: &wire.TxOut{
					Value:    testVTXOValue,
					PkScript: vtxoPkScript,
				},
			},
			OwnerLeafScript: ownerLeafScript,
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
			Txid:           checkpointRes.PSBT.UnsignedTx.TxHash(),
			Output:         checkpointRes.PSBT.UnsignedTx.TxOut[0],
			TapTreeEncoded: checkpointRes.TapTreeEncoded,
		},
	}
	arkPsbt, err := oorlib.BuildArkPSBT(checkpointOutputs, recipients)
	require.NoError(t, err)

	leaf, err := oorlib.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, ownerLeafScript,
	)
	require.NoError(t, err)
	arkPsbt.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	desc := VTXOSigningDescriptor{
		Outpoint:  vtxoOutpoint,
		OwnerKey:  ownerKey.PubKey(),
		ExitDelay: exitDelay,
	}

	return policy, arkPsbt, []*psbt.Packet{checkpointRes.PSBT}, desc,
		operatorKey, ownerKey
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

// newTestActor creates a test actor without starting the durable runtime.
// Tests that call Receive directly don't need the durable mailbox; starting
// it would race with RestartMessage processing that clears the session map.
func newTestActor(t *testing.T, cfg ActorCfg) *Actor {
	t.Helper()

	a := NewActor(cfg)

	return a
}

// clonePSBTSliceForTest deep-copies PSBTs by serialize/parse so tests avoid
// sharing mutable packet pointers across actor boundaries.
func clonePSBTSliceForTest(t *testing.T,
	pkts []*psbt.Packet) []*psbt.Packet {

	t.Helper()

	out := make([]*psbt.Packet, 0, len(pkts))
	for _, pkt := range pkts {
		require.NotNil(t, pkt)
		require.NotNil(t, pkt.UnsignedTx)

		raw, err := psbtutil.Serialize(pkt)
		require.NoError(t, err)

		clone, err := psbtutil.Parse(raw)
		require.NoError(t, err)
		out = append(out, clone)
	}

	return out
}

// buildClientTransferInput constructs a minimal transfer input with all data
// required for client-side collaborative checkpoint signing.
func buildClientTransferInput(t *testing.T, ownerKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	outpoint wire.OutPoint, amount btcutil.Amount,
	ownerLeafScript []byte) clientoor.TransferInput {

	t.Helper()

	tapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	tapscript, err := scripts.VTXOTapScript(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return clientoor.TransferInput{
		VTXO: &clientvtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			OwnerKey: keychain.KeyDescriptor{
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
			Status:         clientvtxo.VTXOStatusLive,
		},
		OwnerLeafScript: ownerLeafScript,
	}
}

// TestActorSubmitAcceptsCollaborativeOwnerLeaf ensures submit
// validation accepts the real client flow where the Ark input
// uses the collaborative checkpoint leaf and only carries the
// client-side signature at submit time.
func TestActorSubmitAcceptsCollaborativeOwnerLeaf(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)
	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	inputOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x21},
		Index: 0,
	}

	collabLeaf, err := scripts.MultiSigCollabTapLeaf(
		ownerKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)

	transferInput := buildClientTransferInput(
		t, ownerKey, operatorKey.PubKey(), exitDelay,
		inputOutpoint, btcutil.Amount(testVTXOValue),
		collabLeaf.Script,
	)

	checkpointRes, err := oorlib.BuildCheckpointPSBT(
		policy, oorlib.CheckpointInput{
			SpentVTXO: oorlib.SpentVTXORef{
				Outpoint: inputOutpoint,
				Output: &wire.TxOut{
					Value:    testVTXOValue,
					PkScript: transferInput.VTXO.PkScript,
				},
			},
			OwnerLeafScript: collabLeaf.Script,
		},
	)
	require.NoError(t, err)

	arkPsbt, err := oorlib.BuildArkPSBT(
		[]oorlib.CheckpointOutput{{
			Txid: checkpointRes.PSBT.UnsignedTx.TxHash(),
			Output: checkpointRes.PSBT.
				UnsignedTx.TxOut[0],
			TapTreeEncoded: checkpointRes.TapTreeEncoded,
		}},
		[]oorlib.RecipientOutput{{
			PkScript: randomP2TRScript(t),
			Value:    btcutil.Amount(testVTXOValue),
		}},
	)
	require.NoError(t, err)

	leaf, err := oorlib.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded, collabLeaf.Script,
	)
	require.NoError(t, err)

	arkPsbt.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{ownerKey}, nil,
	)
	err = clientoor.SignArkPSBT(
		clientSigner, arkPsbt, []*psbt.Packet{checkpointRes.PSBT},
		[]clientoor.TransferInput{transferInput},
	)
	require.NoError(t, err)

	_, err = oorlib.ValidateSubmitPackageSigned(
		arkPsbt, []*psbt.Packet{checkpointRes.PSBT},
	)
	require.Error(t, err)

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointRes.PSBT},
		VTXOSigningDescriptors: []VTXOSigningDescriptor{{
			Outpoint:  inputOutpoint,
			OwnerKey:  ownerKey.PubKey(),
			ExitDelay: exitDelay,
		}},
	})
	require.True(t, submitResp.IsOk(), submitResp.Err())
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

			handle, err := actor.getOrCreateSessionFSM(
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

	actor.sessionsMu.RLock()
	require.Len(t, actor.sessions, 1)
	actor.sessionsMu.RUnlock()
}

// TestActorHappyPath exercises a submit and finalize flow through the actor
// using the in-process outbox driver.
func TestActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	submitRaw := submitResp.UnwrapOr(nil)
	submitMsg, ok := submitRaw.(*SubmitOORResponse)
	if !ok {
		t.Fatalf("unexpected submit response type: %T", submitRaw)
	}

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	if finalizeResp.IsErr() {
		t.Fatalf("finalize failed: %v", finalizeResp.Err())
	}

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
	actor := newTestActor(t, ActorCfg{
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
	actor := newTestActor(t, ActorCfg{
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
	actor := newTestActor(t, ActorCfg{
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

	// FailedState is terminal so the session is cleaned up from
	// the in-memory map. Verify the error message confirms
	// failure.
	require.ErrorContains(t, finalizeResp.Err(), "finalize failed")

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
	actor := newTestActor(t, ActorCfg{
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

	// Session is cleaned up from the map after reaching
	// FinalizedState, so we verify via the response type instead.
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

	// Use the same database for the delivery store so the actor's
	// outer transaction can see data written by the test setup.
	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)

	driver := NewDriver(DriverCfg{
		SessionStore: failStore,
	})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
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

	// Session is cleaned up from the map after reaching
	// FinalizedState, so we verify via the response type instead.
	_, ok := retryResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)
}

// TestActorFinalizeRetryAfterCleanupIsIdempotent asserts that a repeated
// finalize request returns the same success response after terminal session
// cleanup by consulting the durable session store.
func TestActorFinalizeRetryAfterCleanupIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	dbh := db.NewTestDB(t)
	sessionStore := NewDBSessionStore(
		dbh, clock.NewDefaultClock(), btclog.Disabled,
	)
	driver := NewDriver(DriverCfg{
		SessionStore: sessionStore,
	})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		SessionStore:     sessionStore,
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

	firstFinalize := actor.Receive(ctx, finalizeReq)
	require.True(t, firstFinalize.IsOk())

	// Session has been removed from memory after terminalization; retry
	// should succeed via durable store fallback.
	retryFinalize := actor.Receive(ctx, finalizeReq)
	require.True(t, retryFinalize.IsOk())

	_, ok := retryFinalize.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok)
}

// TestActorFinalizeRetryAfterCleanupRejectsMismatchedPayload asserts that
// finalize retries after terminal cleanup must match the originally finalized
// checkpoint payload.
func TestActorFinalizeRetryAfterCleanupRejectsMismatchedPayload(
	t *testing.T) {

	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	dbh := db.NewTestDB(t)
	sessionStore := NewDBSessionStore(
		dbh, clock.NewDefaultClock(), btclog.Disabled,
	)
	driver := NewDriver(DriverCfg{
		SessionStore: sessionStore,
	})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		SessionStore:     sessionStore,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())

	firstFinalize := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, firstFinalize.IsOk())

	mismatch := buildFinalCheckpointPSBT(t, checkpointPsbts[0])
	mismatch.Inputs[0].FinalScriptWitness = []byte{0x02}

	retryFinalize := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{mismatch},
	})
	require.True(t, retryFinalize.IsErr())
	require.ErrorContains(
		t, retryFinalize.Err(), "final checkpoint package mismatch",
	)
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
	actor := newTestActor(t, ActorCfg{
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
	actor := newTestActor(t, ActorCfg{
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
	actor := newTestActor(t, ActorCfg{
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

	// Use the same database for the delivery store so the actor's
	// outer transaction can see data written by the test setup.
	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)

	driver := NewDriver(DriverCfg{Locker: locker})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsErr())

	// Failed sessions are cleaned from the in-memory map, so
	// CurrentState returns an error for the evicted session.
	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err = actor.CurrentState(ctx, sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown session")

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

	// Use the same database for the delivery store so the actor's
	// outer transaction can see data written by the test setup.
	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)

	driver := NewDriver(DriverCfg{Locker: locker})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
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

	policy, arkPsbt, checkpointPsbts, signDesc, operatorKey,
		ownerKey := buildTestSubmitPackageWithDescriptor(
		t, recipients,
	)

	inputOutpoint := signDesc.Outpoint

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

	// Use the same database for the delivery store so the actor's
	// outer transaction can see data written by the test setup.
	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	driver := NewDriver(DriverCfg{
		Store:          store,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: policy.OperatorKey,
		},
	})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
		VTXOSigningDescriptors: []VTXOSigningDescriptor{
			signDesc,
		},
	})
	if submitResp.IsErr() {
		t.Fatalf("submit failed: %v", submitResp.Err())
	}

	submitRaw := submitResp.UnwrapOr(nil)
	submitMsg, ok := submitRaw.(*SubmitOORResponse)
	if !ok {
		t.Fatalf("unexpected submit response type: %T", submitRaw)
	}

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{ownerKey}, nil,
	)
	inputs := []clientoor.TransferInput{
		buildClientTransferInput(
			t, ownerKey, policy.OperatorKey,
			signDesc.ExitDelay, signDesc.Outpoint,
			btcutil.Amount(testVTXOValue),
			[]byte{txscript.OP_TRUE},
		),
	}
	finalized := clonePSBTSliceForTest(
		t, submitMsg.CoSignedCheckpointPSBTs,
	)
	err = clientoor.SignCheckpointPSBTs(
		clientSigner, inputs, finalized,
	)
	require.NoError(t, err)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            submitMsg.SessionID,
		FinalCheckpointPSBTs: finalized,
	})
	if finalizeResp.IsErr() {
		t.Fatalf("finalize failed: %v", finalizeResp.Err())
	}

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

// ---------------------------------------------------------------------------
// Regression tests for session cleanup, restart, and delivery correctness.
// ---------------------------------------------------------------------------

// TestSubmitFailedCleansSessionMap verifies that sessions reaching FailedState
// are removed from the in-memory map, preventing unbounded growth from
// repeated failed submissions.
func TestSubmitFailedCleansSessionMap(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	// Use a driver that always fails validation to trigger
	// FailedState after the session is created in the map.
	failDriver := &failingOutboxHandler{}
	a := newTestActor(t, ActorCfg{
		OutboxHandler:    failDriver,
		CheckpointPolicy: policy,
	})

	const numSubmits = 10

	for i := 0; i < numSubmits; i++ {
		// Each iteration uses a unique locktime to create a
		// distinct session ID.
		attackPsbt := clonePSBTSliceForTest(
			t, []*psbt.Packet{arkPsbt},
		)[0]
		attackPsbt.UnsignedTx.LockTime = uint32(i + 1)

		resp := a.Receive(ctx, &SubmitOORRequest{
			ArkPSBT:         attackPsbt,
			CheckpointPSBTs: checkpointPsbts,
		})
		require.True(t, resp.IsErr(),
			"iteration %d should fail", i)
	}

	// All failed sessions must be cleaned up.
	a.sessionsMu.RLock()
	leakedCount := len(a.sessions)
	a.sessionsMu.RUnlock()

	require.Zero(t, leakedCount,
		"expected 0 leaked sessions, got %d", leakedCount)
}

// TestRestartPopulatesFinalCheckpointPSBTs verifies that sessions restored
// in AwaitingRecipientsNotifyState have FinalCheckpointPSBTs populated from
// the DB session record. This uses the full durable actor restart flow
// with a real DB-backed driver.
func TestRestartPopulatesFinalCheckpointPSBTs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sqlStore := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	testLog := btclog.Disabled

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	sessionStore1 := NewDBSessionStore(sqlStore, clk, testLog)
	realDriver := NewDriver(DriverCfg{
		SessionStore: sessionStore1,
		OperatorKey:  keychain.KeyDescriptor{},
	})

	// Wrap the real driver to intercept notification and force it to
	// fail, leaving the session in awaiting_notify state in the DB.
	failNotifyDriver := &notifyFailingDriver{
		OutboxHandler: realDriver,
	}

	// Use actor1 without durable runtime — call Receive directly for
	// submit and finalize. The session is persisted to the DB via the
	// outbox driver's SessionStore.
	actor1 := NewActor(ActorCfg{
		OutboxHandler:    failNotifyDriver,
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

	// Finalize returns an error because notification fails, but the
	// session is persisted in awaiting_notify state in the DB.
	finalizeResp := actor1.Receive(ctx, &FinalizeOORRequest{
		SessionID: submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
	})
	require.True(t, finalizeResp.IsErr(),
		"finalize should error due to failed notification")

	// Verify the DB row is in awaiting_notify state before restart.
	row, err := sqlStore.GetOORSession(
		ctx, sessionIDBytes(submitMsg.SessionID),
	)
	require.NoError(t, err)
	require.Equal(t, string(oorStateAwaitingNotify), row.State)

	// Simulate restart: new actor backed by the same DB. The durable
	// runtime's RestartMessage rebuilds active sessions from persisted
	// rows.
	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)
	sessionStore2 := NewDBSessionStore(sqlStore, clk, testLog)

	actor2 := NewActor(ActorCfg{
		OutboxHandler:    realDriver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
		SessionStore:     sessionStore2,
	})

	err = actor2.Start(ctx)
	require.NoError(t, err)
	defer actor2.Stop()

	// Poll the restored session state directly. Start enqueues a restart
	// message and returns before the durable runtime has necessarily
	// rebuilt active sessions from the DB.
	var state State
	require.Eventually(t, func() bool {
		var currentErr error
		state, currentErr = actor2.CurrentState(
			ctx, submitMsg.SessionID,
		)

		return currentErr == nil
	}, 5*time.Second, 100*time.Millisecond)

	notifyState, isNotify := state.(*AwaitingRecipientsNotifyState)
	require.True(t, isNotify,
		"restored state must be AwaitingRecipientsNotifyState, "+
			"got %T", state)
	require.NotNil(t, notifyState.FinalCheckpointPSBTs,
		"FinalCheckpointPSBTs must be populated on restart")
	require.Len(t, notifyState.FinalCheckpointPSBTs, 1)
}

// TestToProtoReturnsNonNil verifies that SubmitOORResponse.ToProto() and
// FinalizeOORResponse.ToProto() return non-nil proto messages, which is
// required for the production durable egress delivery path.
func TestToProtoReturnsNonNil(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(t, checkpointPsbts[0])

	driver := NewDriver(DriverCfg{})
	a := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	// Get a real SubmitOORResponse.
	submitResp := a.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok, "expected *SubmitOORResponse")
	require.NotNil(t, submitMsg.ToProto(),
		"SubmitOORResponse.ToProto() must not return nil")

	// Get a real FinalizeOORResponse.
	finalizeResp := a.Receive(ctx, &FinalizeOORRequest{
		SessionID: submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
	})
	require.True(t, finalizeResp.IsOk())

	finalizeMsg, ok := finalizeResp.UnwrapOr(nil).(*FinalizeOORResponse)
	require.True(t, ok, "expected *FinalizeOORResponse")
	require.NotNil(t, finalizeMsg.ToProto(),
		"FinalizeOORResponse.ToProto() must not return nil")
}

// TestClientIDFlowsThroughPushDelivery verifies that when ClientID is set on
// the request, it propagates to the response pushed via ClientsConn.
func TestClientIDFlowsThroughPushDelivery(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts := buildTestSubmitPackage(t, nil)

	driver := NewDriver(DriverCfg{})

	// Capture the ClientID from the pushed response.
	var pushedClientID clientconn.ClientID
	mockConn := &mockTellOnlyRef{
		tellFn: func(_ context.Context,
			msg clientconn.ClientConnMsg) error {

			sendReq, ok :=
				msg.(*clientconn.SendServerEventRequest)
			if ok {
				pushedClientID =
					sendReq.Message.ClientID()
			}

			return nil
		},
	}

	a := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		ClientsConn:      mockConn,
	})

	const expectedClientID = clientconn.ClientID("test-client-42")

	submitResp := a.Receive(ctx, &SubmitOORRequest{
		ClientID:        expectedClientID,
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPsbts,
	})
	require.True(t, submitResp.IsOk())
	require.Equal(t, expectedClientID, pushedClientID,
		"pushed response must carry the submitting client's ID")
}

// -- Test helper types for regression tests --

// notifyFailingDriver wraps a real OutboxHandler but intercepts
// NotifyRecipientsReq to force a failure, leaving sessions in the
// awaiting_notify state for restart testing.
type notifyFailingDriver struct {
	OutboxHandler
}

// Handle delegates to the wrapped handler except for notification, which
// always returns a failure event.
func (d *notifyFailingDriver) Handle(ctx context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	if _, isNotify := outbox.(*NotifyRecipientsReq); isNotify {
		return []Event{
			&NotifyRecipientsFailedEvent{
				Reason: "simulated notification failure",
			},
		}, nil
	}

	return d.OutboxHandler.Handle(ctx, sessionID, outbox)
}

// failingOutboxHandler is an OutboxHandler that always fails validation,
// triggering FailedState after session creation.
type failingOutboxHandler struct{}

// Handle returns a lock success then a validation failure to exercise
// the FailedState cleanup path.
func (f *failingOutboxHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	switch outbox.(type) {
	case *LockInputsReq:
		return []Event{
			&InputsLockSucceededEvent{},
		}, nil

	case *ValidateSubmitReq:
		return []Event{
			&SubmitFailedEvent{
				Reason: "simulated validation failure",
			},
		}, nil

	case *UnlockInputsReq:
		return nil, nil

	default:
		return nil, fmt.Errorf("unexpected outbox: %T", outbox)
	}
}

// mockTellOnlyRef implements actor.TellOnlyRef[clientconn.ClientConnMsg]
// for testing push delivery routing.
type mockTellOnlyRef struct {
	tellFn func(context.Context, clientconn.ClientConnMsg) error
}

// ID returns a test identifier.
func (m *mockTellOnlyRef) ID() string {
	return "mock-clients-conn"
}

// Tell delegates to the configured function.
func (m *mockTellOnlyRef) Tell(ctx context.Context,
	msg clientconn.ClientConnMsg) error {

	if m.tellFn != nil {
		return m.tellFn(ctx, msg)
	}

	return nil
}
