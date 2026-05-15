package oor

import (
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
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
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
	sessionID SessionID, finalCheckpointPSBTs []*psbt.Packet) error {

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

// stripCheckpointTapTreeMetadata removes the checkpoint output tap tree
// metadata so submit validation fails before any session state changes.
func stripCheckpointTapTreeMetadata(t *testing.T, pkt *psbt.Packet,
	outputIndex int) {

	t.Helper()

	require.NotNil(t, pkt)
	require.Greater(t, len(pkt.Outputs), outputIndex)
	pkt.Outputs[outputIndex].TaprootTapTree = nil
}

// buildTestSubmitPackage constructs a minimal valid v0 OOR submit package.
func buildTestSubmitPackage(t *testing.T, recipients []oorlib.RecipientOutput) (
	arkscript.CheckpointPolicy, *psbt.Packet, []*psbt.Packet) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	ownerLeafScript, ownerLeafPolicy := testOwnerLeaf(
		t, ownerKey.PubKey(), operatorKey.PubKey(),
	)
	checkpointRes, err := oorlib.BuildCheckpointPSBT(
		arkscript.CheckpointPolicy{
			OperatorKey: policy.OperatorKey,
			CSVDelay:    policy.CSVDelay,
		}, oorlib.CheckpointInput{
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
			OwnerLeafPolicy: ownerLeafPolicy,
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
			TapTreeEncoded:  checkpointRes.TapTreeEncoded,
			OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
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
// returns the package plus the signing descriptor and test keys.
func buildTestSubmitPackageWithDescriptor(t *testing.T,
	recipients []oorlib.RecipientOutput) (arkscript.CheckpointPolicy,
	*psbt.Packet, []*psbt.Packet, VTXOSigningDescriptor, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	vtxoTapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
	require.NoError(t, err)

	vtxoOutpoint := wire.OutPoint{
		Hash: [32]byte{
			1,
		},
		Index: 7,
	}

	ownerLeafScript, ownerLeafPolicy := testOwnerLeaf(
		t, ownerKey.PubKey(), operatorKey.PubKey(),
	)
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
			OwnerLeafPolicy: ownerLeafPolicy,
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
			Txid:            checkpointRes.PSBT.UnsignedTx.TxHash(),
			Output:          checkpointRes.PSBT.UnsignedTx.TxOut[0],
			TapTreeEncoded:  checkpointRes.TapTreeEncoded,
			OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
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

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{ownerKey}, nil,
	)
	err = clientoor.SignArkPSBT(
		clientSigner, arkPsbt, []*psbt.Packet{checkpointRes.PSBT},
		[]clientoor.TransferInput{
			buildClientTransferInput(
				t, ownerKey, policy.OperatorKey, exitDelay,
				vtxoOutpoint, btcutil.Amount(testVTXOValue),
				ownerLeafScript, ownerLeafPolicy,
			),
		},
	)
	require.NoError(t, err)

	desc := VTXOSigningDescriptor{
		Outpoint: vtxoOutpoint,
		VTXOPolicyTemplate: testStandardVTXOPolicyTemplate(
			t, ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
		),
		SpendPath: testStandardCollabSpendPath(
			t, ownerKey.PubKey(), operatorKey.PubKey(), exitDelay,
		),
		OwnerLeafPolicy: ownerLeafPolicy,
	}

	return policy, arkPsbt, []*psbt.Packet{checkpointRes.PSBT}, desc,
		operatorKey, ownerKey
}

// buildTestMultiInputSubmitPackageWithDescriptors constructs a valid submit
// package that spends multiple checkpoint outputs. The Ark signatures are
// produced with the same full prevout context used by the client signer.
func buildTestMultiInputSubmitPackageWithDescriptors(t *testing.T) (
	arkscript.CheckpointPolicy, *psbt.Packet, []*psbt.Packet,
	[]VTXOSigningDescriptor) {

	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	type submitInput struct {
		ownerLeafScript []byte
		checkpoint      *oorlib.CheckpointArtifact
	}

	const inputCount = 2

	inputs := make([]submitInput, 0, inputCount)
	checkpointOutputs := make([]oorlib.CheckpointOutput, 0, inputCount)
	checkpointPSBTs := make([]*psbt.Packet, 0, inputCount)
	transferInputs := make([]clientoor.TransferInput, 0, inputCount)
	descs := make([]VTXOSigningDescriptor, 0, inputCount)
	ownerKeys := make([]*btcec.PrivateKey, 0, inputCount)

	var totalValue btcutil.Amount
	for i := 0; i < inputCount; i++ {
		ownerKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		vtxoTapKey, err := arkscript.VTXOTapKey(
			ownerKey.PubKey(), policy.OperatorKey, exitDelay,
		)
		require.NoError(t, err)

		vtxoPkScript, err := txscript.PayToTaprootScript(vtxoTapKey)
		require.NoError(t, err)

		outpoint := wire.OutPoint{
			Hash: [32]byte{
				byte(i + 1),
			},
			Index: uint32(7 + i),
		}
		amount := btcutil.Amount(testVTXOValue + int64(i*111))

		ownerLeafScript, ownerLeafPolicy := testOwnerLeaf(
			t, ownerKey.PubKey(), operatorKey.PubKey(),
		)
		checkpointRes, err := oorlib.BuildCheckpointPSBT(
			policy, oorlib.CheckpointInput{
				SpentVTXO: oorlib.SpentVTXORef{
					Outpoint: outpoint,
					Output: &wire.TxOut{
						Value:    int64(amount),
						PkScript: vtxoPkScript,
					},
				},
				OwnerLeafScript: ownerLeafScript,
				OwnerLeafPolicy: ownerLeafPolicy,
			},
		)
		require.NoError(t, err)

		transferInput := buildClientTransferInput(
			t, ownerKey, policy.OperatorKey, exitDelay, outpoint,
			amount, ownerLeafScript, ownerLeafPolicy,
		)

		inputs = append(inputs, submitInput{
			ownerLeafScript: ownerLeafScript,
			checkpoint:      checkpointRes,
		})
		checkpointTxid := checkpointRes.PSBT.UnsignedTx.TxHash()
		checkpointTxOut := checkpointRes.PSBT.UnsignedTx.TxOut[0]

		checkpointOutputs = append(
			checkpointOutputs, oorlib.CheckpointOutput{
				Txid:            checkpointTxid,
				Output:          checkpointTxOut,
				TapTreeEncoded:  checkpointRes.TapTreeEncoded,
				OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
			},
		)
		checkpointPSBTs = append(checkpointPSBTs, checkpointRes.PSBT)
		transferInputs = append(transferInputs, transferInput)
		descs = append(descs, VTXOSigningDescriptor{
			Outpoint: outpoint,
			VTXOPolicyTemplate: testStandardVTXOPolicyTemplate(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			SpendPath: testStandardCollabSpendPath(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			OwnerLeafPolicy: ownerLeafPolicy,
		})
		ownerKeys = append(ownerKeys, ownerKey)
		totalValue += amount
	}

	arkPsbt, err := oorlib.BuildArkPSBT(
		checkpointOutputs, []oorlib.RecipientOutput{{
			PkScript: randomP2TRScript(t),
			Value:    totalValue,
		}},
	)
	require.NoError(t, err)

	inputByCheckpointTxid := make(
		map[chainhash.Hash]submitInput, len(inputs),
	)
	for _, in := range inputs {
		checkpointTxid := in.checkpoint.PSBT.UnsignedTx.TxHash()
		inputByCheckpointTxid[checkpointTxid] = in
	}
	for i, txIn := range arkPsbt.UnsignedTx.TxIn {
		in, ok := inputByCheckpointTxid[txIn.PreviousOutPoint.Hash]
		require.True(t, ok)

		leaf, err := oorlib.BuildTaprootTapLeafScript(
			in.checkpoint.TapTreeEncoded, in.ownerLeafScript,
		)
		require.NoError(t, err)

		arkPsbt.Inputs[i].TaprootLeafScript =
			[]*psbt.TaprootTapLeafScript{leaf}
	}

	clientSigner := input.NewMockSigner(ownerKeys, nil)
	err = clientoor.SignArkPSBT(
		clientSigner, arkPsbt, checkpointPSBTs, transferInputs,
	)
	require.NoError(t, err)

	return policy, arkPsbt, checkpointPSBTs, descs
}

func testOwnerLeaf(t *testing.T, ownerKey, operatorKey *btcec.PublicKey) (
	[]byte, []byte) {

	t.Helper()

	leaf := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey,
				operatorKey,
			},
		},
	}

	script, err := leaf.Script()
	require.NoError(t, err)

	encoded, err := leaf.Encode()
	require.NoError(t, err)

	return script, encoded
}

func testStandardVTXOPolicyTemplate(t *testing.T,
	ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return policy
}

func testStandardCollabSpendPath(t *testing.T,
	ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.NewVTXOPolicy(
		ownerKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	info, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	path := &arkscript.SpendPath{
		SpendInfo: info,
	}
	raw, err := path.Encode()
	require.NoError(t, err)

	return raw
}

// buildTestSubmitRequest constructs a valid submit request with the signing
// descriptor needed by server-side owner-proof validation.
func buildTestSubmitRequest(t *testing.T, recipients []oorlib.RecipientOutput) (
	arkscript.CheckpointPolicy, *SubmitOORRequest, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	policy, arkPsbt, checkpointPSBTs, desc, operatorKey, ownerKey :=
		buildTestSubmitPackageWithDescriptor(t, recipients)

	return policy, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: checkpointPSBTs,
		VTXOSigningDescriptors: []VTXOSigningDescriptor{
			desc,
		},
	}, operatorKey, ownerKey
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
func clonePSBTSliceForTest(t *testing.T, pkts []*psbt.Packet) []*psbt.Packet {
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
	operatorKey *btcec.PublicKey, exitDelay uint32, outpoint wire.OutPoint,
	amount btcutil.Amount, ownerLeafScript,
	ownerLeafPolicy []byte) clientoor.TransferInput {

	t.Helper()

	tapKey, err := arkscript.VTXOTapKey(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	tapscript, err := arkscript.VTXOTapScript(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return clientoor.TransferInput{
		VTXO: &clientvtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
			Status:         clientvtxo.VTXOStatusLive,
		},
		OwnerLeafScript: ownerLeafScript,
		OwnerLeafPolicy: ownerLeafPolicy,
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
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    exitDelay,
	}

	inputOutpoint := wire.OutPoint{
		Hash: [32]byte{
			0x21,
		},
		Index: 0,
	}

	collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
		ownerKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)
	collabLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey.PubKey(), operatorKey.PubKey(),
			},
		},
	}.Encode()
	require.NoError(t, err)

	transferInput := buildClientTransferInput(
		t, ownerKey, operatorKey.PubKey(), exitDelay, inputOutpoint,
		btcutil.Amount(testVTXOValue), collabLeaf.Script,
		collabLeafPolicy,
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
			OwnerLeafPolicy: collabLeafPolicy,
		},
	)
	require.NoError(t, err)

	arkPsbt, err := oorlib.BuildArkPSBT(
		[]oorlib.CheckpointOutput{{
			Txid: checkpointRes.PSBT.UnsignedTx.TxHash(),
			Output: checkpointRes.PSBT.
				UnsignedTx.TxOut[0],
			TapTreeEncoded:  checkpointRes.TapTreeEncoded,
			OwnerLeafPolicy: checkpointRes.OwnerLeafPolicy,
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
			Outpoint: inputOutpoint,
			VTXOPolicyTemplate: testStandardVTXOPolicyTemplate(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			SpendPath: testStandardCollabSpendPath(
				t, ownerKey.PubKey(), operatorKey.PubKey(),
				exitDelay,
			),
			OwnerLeafPolicy: collabLeafPolicy,
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	submitReq.ArkPSBT.Inputs[0].WitnessUtxo = nil

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorSubmitMissingTapTreeAssertsUnlock exercises a submit that fails
// validation because the checkpoint output does not include tap tree metadata.
func TestActorSubmitMissingTapTreeAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	stripCheckpointTapTreeMetadata(t, submitReq.CheckpointPSBTs[0], 0)

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorFinalizeMissingSigDoesNotUnlock asserts that finalize failures
// after the point-of-no-return do not emit an unlock request, do not
// terminate the session, and leave the FSM in a recoverable CoSignedState
// so the client can resubmit a corrected finalize package. Regression
// coverage for issue #372.
func TestActorFinalizeMissingSigDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &CoSignedState{}, state)

	// Finalize without FinalScriptWitness fails structural validation.
	finalCheckpoint, err := psbt.NewFromUnsignedTx(
		submitReq.CheckpointPSBTs[0].UnsignedTx,
	)
	require.NoError(t, err)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, finalizeResp.IsErr())

	// The actor must surface the failure reason but keep the session
	// alive: no terminal FailedState, no unlock side effect.
	require.ErrorContains(t, finalizeResp.Err(), "finalize failed")

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.NotContains(t, seen, "UnlockInputsReq")

	// The session must remain in CoSignedState so the client can retry
	// finalize. Before the fix the FSM would have transitioned to the
	// terminal FailedState, the session would be deleted from the map,
	// and CurrentState would return an "unknown session" error.
	postState, err := actor.CurrentState(ctx, sessionID)
	require.NoError(
		t, err, "session must still be tracked after a recoverable "+
			"finalize failure",
	)
	cosigned, ok := postState.(*CoSignedState)
	require.True(
		t, ok, "expected CoSignedState recovery anchor, got %T",
		postState,
	)
	require.NotEmpty(
		t, cosigned.LastFinalizeFailureReason,
		"failure reason must be surfaced on the recovered state",
	)
}

// TestActorFinalizeNotifyFailureIsRetryable asserts recipient event-store
// failures surface as finalize errors while keeping the session retryable.
func TestActorFinalizeNotifyFailureIsRetryable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

	recipientEvents := &failingRecipientEventStore{
		err: errors.New("notify failed"),
	}
	driver := NewDriver(DriverCfg{RecipientEvents: recipientEvents})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

	finalizeReq := &FinalizeOORRequest{
		SessionID: sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

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

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	finalizeReq := &FinalizeOORRequest{
		SessionID: sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

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

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	finalizeReq := &FinalizeOORRequest{
		SessionID: sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{
			finalCheckpoint,
		},
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
func TestActorFinalizeRetryAfterCleanupRejectsMismatchedPayload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

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

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())

	firstFinalize := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{finalCheckpoint},
	})
	require.True(t, firstFinalize.IsOk())

	mismatch := buildFinalCheckpointPSBT(t, submitReq.CheckpointPSBTs[0])
	mismatch.Inputs[0].FinalScriptWitness = []byte{0x02}

	retryFinalize := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{mismatch},
	})
	require.True(t, retryFinalize.IsErr())
	require.ErrorContains(
		t, retryFinalize.Err(),
		"final checkpoint package mismatch",
	)
}

// TestActorSubmitNonCanonicalOutputsAssertsUnlock exercises a submit that fails
// because the Ark tx recipient outputs are not in canonical order.
func TestActorSubmitNonCanonicalOutputsAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	recipients := []oorlib.RecipientOutput{
		{
			PkScript: []byte{
				0x51,
			},
			Value: 500,
		},
		{
			PkScript: []byte{
				0x52,
			},
			Value: btcutil.Amount(testVTXOValue) - 500,
		},
	}

	policy, submitReq, _, _ := buildTestSubmitRequest(t, recipients)

	// BuildArkPSBT canonicalizes ordering. Break it by swapping the first
	// two recipient outputs while keeping the anchor in the final position.
	require.GreaterOrEqual(t, len(submitReq.ArkPSBT.UnsignedTx.TxOut), 3)
	outs := submitReq.ArkPSBT.UnsignedTx.TxOut
	outs[0], outs[1] = outs[1], outs[0]

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
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
			PkScript: []byte{
				0x51,
			},
			Value: 500,
		},
		{
			PkScript: []byte{
				0x52,
			},
			Value: btcutil.Amount(testVTXOValue) - 500,
		},
	}

	policy, submitReq, _, _ := buildTestSubmitRequest(t, recipients)

	require.GreaterOrEqual(t, len(submitReq.ArkPSBT.UnsignedTx.TxOut), 3)
	outs := submitReq.ArkPSBT.UnsignedTx.TxOut
	last := len(outs) - 1
	outs[0], outs[last] = outs[last], outs[0]

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
	_, err := actor.CurrentState(ctx, sessionID)
	require.Error(t, err)

	require.Empty(t, driver.SeenOutboxTypes())
}

// TestActorSubmitMissingAnchorAssertsUnlock exercises a submit that fails
// because the Ark tx is missing the anchor output.
func TestActorSubmitMissingAnchorAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

	require.GreaterOrEqual(t, len(submitReq.ArkPSBT.UnsignedTx.TxOut), 2)
	outs := submitReq.ArkPSBT.UnsignedTx.TxOut
	submitReq.ArkPSBT.UnsignedTx.TxOut = outs[:len(outs)-1]

	driver := NewDriver(DriverCfg{})
	actor := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

	inputOutpoint := submitReq.CheckpointPSBTs[0].UnsignedTx.
		TxIn[0].PreviousOutPoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	store := dbStore.NewVTXORecordStore()
	locker := db.NewVTXOLockerDB(sqlStore, btclog.Disabled)

	err := store.Create(ctx, &vtxo.Record{
		Outpoint: inputOutpoint,
		Value: submitReq.CheckpointPSBTs[0].Inputs[0].
			WitnessUtxo.Value,
		PkScript: submitReq.CheckpointPSBTs[0].Inputs[0].
			WitnessUtxo.PkScript,
		Status: vtxo.StatusLive,
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

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsErr())

	// Failed sessions are cleaned from the in-memory map, so
	// CurrentState returns an error for the evicted session.
	sessionID := SessionID(submitReq.ArkPSBT.UnsignedTx.TxHash())
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	inputOutpoint := submitReq.CheckpointPSBTs[0].UnsignedTx.
		TxIn[0].PreviousOutPoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	store := dbStore.NewVTXORecordStore()
	locker := db.NewVTXOLockerDB(sqlStore, btclog.Disabled)

	err := store.Create(ctx, &vtxo.Record{
		Outpoint: inputOutpoint,
		Value: submitReq.CheckpointPSBTs[0].Inputs[0].
			WitnessUtxo.Value,
		PkScript: submitReq.CheckpointPSBTs[0].Inputs[0].
			WitnessUtxo.PkScript,
		Status: vtxo.StatusLive,
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

	submitResp := actor.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	err = locker.LockMany(
		ctx, []wire.OutPoint{inputOutpoint},
		vtxo.RoundLockOwner("12345678-1234-1234-1234-123456789012"),
	)
	require.Error(t, err)

	var lockedErr *vtxo.ErrLocked
	require.ErrorAs(t, err, &lockedErr)
}

// TestActorUnauthorizedSubmitFailsBeforeLock asserts owner-proof failures stop
// at submit validation and leave the shared locker untouched.
func TestActorUnauthorizedSubmitFailsBeforeLock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, arkPsbt, checkpointPsbts, signDesc, _, _ :=
		buildTestSubmitPackageWithDescriptor(t, nil)
	arkPsbt.Inputs[0].TaprootScriptSpendSig = nil

	inputOutpoint := signDesc.Outpoint

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
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

	deliveryStore := newActorDeliveryStoreForTest(t, sqlStore)

	driver := NewDriver(DriverCfg{
		Store:  store,
		Locker: locker,
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
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
	_, err = actor.CurrentState(ctx, sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown session")

	seen := driver.SeenOutboxTypes()
	require.Contains(t, seen, "ValidateSubmitReq")
	require.NotContains(t, seen, "LockInputsReq")
	require.NotContains(t, seen, "UnlockInputsReq")

	err = locker.LockMany(
		ctx, []wire.OutPoint{inputOutpoint},
		vtxo.RoundLockOwner("12345678-1234-1234-1234-123456789012"),
	)
	require.NoError(t, err)
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
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
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
	signTemplate, err := arkscript.DecodePolicyTemplate(
		signDesc.VTXOPolicyTemplate,
	)
	require.NoError(t, err)

	signParams, err := arkscript.DecodeStandardVTXOParams(signTemplate)
	require.NoError(t, err)

	ownerLeafScript, ownerLeafPolicy := testOwnerLeaf(
		t, ownerKey.PubKey(), policy.OperatorKey,
	)
	inputs := []clientoor.TransferInput{
		buildClientTransferInput(
			t, ownerKey, policy.OperatorKey, signParams.ExitDelay,
			signDesc.Outpoint, btcutil.Amount(testVTXOValue),
			ownerLeafScript, ownerLeafPolicy,
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

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
			t, []*psbt.Packet{submitReq.ArkPSBT},
		)[0]
		attackPsbt.UnsignedTx.LockTime = uint32(i + 1)

		resp := a.Receive(ctx, &SubmitOORRequest{
			ArkPSBT:         attackPsbt,
			CheckpointPSBTs: submitReq.CheckpointPSBTs,
			VTXOSigningDescriptors: submitReq.
				VTXOSigningDescriptors,
		})
		require.True(t, resp.IsErr(),
			"iteration %d should fail", i)
	}

	// All failed sessions must be cleaned up.
	a.sessionsMu.RLock()
	leakedCount := len(a.sessions)
	a.sessionsMu.RUnlock()

	require.Zero(
		t, leakedCount, "expected 0 leaked sessions, got %d",
		leakedCount,
	)
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

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

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

	submitResp := actor1.Receive(ctx, submitReq)
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
	require.True(
		t, finalizeResp.IsErr(),
		"finalize should error due to failed notification",
	)

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
	require.True(
		t, isNotify, "restored state must be "+
			"AwaitingRecipientsNotifyState, got %T", state,
	)
	require.NotNil(
		t, notifyState.FinalCheckpointPSBTs,
		"FinalCheckpointPSBTs must be populated on restart",
	)
	require.Len(t, notifyState.FinalCheckpointPSBTs, 1)
}

// TestToProtoReturnsNonNil verifies that SubmitOORResponse.ToProto() and
// FinalizeOORResponse.ToProto() return non-nil proto messages, which is
// required for the production durable egress delivery path.
func TestToProtoReturnsNonNil(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)
	finalCheckpoint := buildFinalCheckpointPSBT(
		t, submitReq.CheckpointPSBTs[0],
	)

	driver := NewDriver(DriverCfg{})
	a := newTestActor(t, ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	// Get a real SubmitOORResponse.
	submitResp := a.Receive(ctx, submitReq)
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok, "expected *SubmitOORResponse")
	require.NotNil(
		t, submitMsg.ToProto(),
		"SubmitOORResponse.ToProto() must not return nil",
	)

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
	require.NotNil(
		t, finalizeMsg.ToProto(),
		"FinalizeOORResponse.ToProto() must not return nil",
	)
}

// TestClientIDFlowsThroughPushDelivery verifies that when ClientID is set on
// the request, it propagates to the response pushed via ClientsConn.
func TestClientIDFlowsThroughPushDelivery(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

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
		ArkPSBT:         submitReq.ArkPSBT,
		CheckpointPSBTs: submitReq.CheckpointPSBTs,
		VTXOSigningDescriptors: submitReq.
			VTXOSigningDescriptors,
	})
	require.True(t, submitResp.IsOk())
	require.Equal(
		t, expectedClientID, pushedClientID,
		"pushed response must carry the submitting client's ID",
	)
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
func (d *notifyFailingDriver) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

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
type failingOutboxHandler struct {
	// code is the typed reject code surfaced from the simulated
	// validation failure. Defaults to RejectCodeUnspecified when not
	// set; tests that exercise the typed-code-on-the-wire path set
	// this to a specific code (e.g. RejectCodeLineageTooLarge) and
	// assert it round-trips end-to-end through the actor's pushed
	// response.
	code RejectCode
}

// Handle returns a validation failure before any lock event to exercise the
// FailedState cleanup path.
func (f *failingOutboxHandler) Handle(_ context.Context, _ SessionID,
	outbox OutboxEvent) ([]Event, error) {

	switch outbox.(type) {
	case *ValidateSubmitReq:
		return []Event{
			&SubmitFailedEvent{
				Reason: "simulated validation failure",
				Code:   f.code,
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

// TestActorSubmitFailedPushesTypedRejectCode is the H-1 regression
// test: a SubmitFailedEvent carrying a typed RejectCode must produce a
// proto SubmitPackageRejection that carries the same typed code on
// the wire so the client side can recover ErrLineageTooLarge via
// errors.As. Without the actor pushing a SubmitOORResponse with its
// Rejection branch populated, the typed code dies inside the actor's
// FailedState branch and only a generic Go error string reaches the
// client.
func TestActorSubmitFailedPushesTypedRejectCode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	policy, submitReq, _, _ := buildTestSubmitRequest(t, nil)

	// Drive the FSM into FailedState carrying the typed
	// RejectCodeLineageTooLarge.
	failDriver := &failingOutboxHandler{
		code: RejectCodeLineageTooLarge,
	}

	// Capture the pushed response so we can assert (a) it carries a
	// non-nil Rejection branch with the typed code, and (b) its
	// ToProto() emits the proto SubmitPackageResponse_Rejection
	// branch — i.e. the value the client-side helper would parse.
	var (
		pushedResp     *SubmitOORResponse
		pushedProtoMsg interface{}
	)
	mockConn := &mockTellOnlyRef{
		tellFn: func(_ context.Context,
			msg clientconn.ClientConnMsg) error {

			sendReq, ok :=
				msg.(*clientconn.SendServerEventRequest)
			if !ok {
				return nil
			}

			resp, ok := sendReq.Message.(*SubmitOORResponse)
			if ok {
				pushedResp = resp
				pushedProtoMsg = resp.ToProto()
			}

			return nil
		},
	}

	a := newTestActor(t, ActorCfg{
		OutboxHandler:    failDriver,
		CheckpointPolicy: policy,
		ClientsConn:      mockConn,
	})

	const clientID = clientconn.ClientID("test-client-reject")

	submitResp := a.Receive(ctx, &SubmitOORRequest{
		ClientID:        clientID,
		ArkPSBT:         submitReq.ArkPSBT,
		CheckpointPSBTs: submitReq.CheckpointPSBTs,
		VTXOSigningDescriptors: submitReq.
			VTXOSigningDescriptors,
	})
	require.True(
		t, submitResp.IsErr(),
		"FailedState must surface as an error from Receive",
	)

	// The actor must have pushed a SubmitOORResponse to the client
	// with the rejection branch populated; without this, the typed
	// code dies in the actor and never reaches the client.
	require.NotNil(
		t, pushedResp,
		"FailedState must push a SubmitOORResponse via clientconn",
	)
	require.NotNil(
		t, pushedResp.Rejection,
		"pushed response must carry the typed Rejection branch",
	)
	require.Equal(
		t, RejectCodeLineageTooLarge, pushedResp.Rejection.Code,
		"pushed Rejection.Code must equal FailedState.Code",
	)
	require.Equal(
		t, clientID, pushedResp.ClientID(),
		"pushed response must route to the submitting client",
	)

	// ToProto must emit the proto rejection branch with the typed
	// code at the wire so the client-side helper recovers
	// ErrLineageTooLarge via errors.As. This is the load-bearing
	// assertion: a regression that bypasses the rejection branch
	// (e.g. by emitting only the success branch) breaks the typed
	// code contract end-to-end.
	require.NotNil(t, pushedProtoMsg)
	protoResp, ok := pushedProtoMsg.(*oorpb.SubmitPackageResponse)
	require.True(t, ok, "ToProto must emit *SubmitPackageResponse")

	rejBranch, ok := protoResp.Result.(*oorpb.
		SubmitPackageResponse_Rejection)
	require.True(
		t, ok, "FailedState must emit the Rejection oneof branch, "+
			"got %T", protoResp.Result,
	)
	require.NotNil(
		t, rejBranch.Rejection, "Rejection branch must be populated",
	)
	require.Equal(
		t, oorpb.OORRejectCode_OOR_REJECT_LINEAGE_TOO_LARGE,
		rejBranch.Rejection.Code, "proto rejection must carry the "+
			"typed code mapped from the FSM-side RejectCode",
	)

	// Round-trip via the client-side parser to confirm
	// ParseSubmitPackageResponse recovers the typed
	// SubmitRejectedError with the same code; this is what the
	// downstream darepo-client ClassifySubmitError consumes.
	_, _, parseErr := oorpb.ParseSubmitPackageResponse(protoResp)
	require.Error(t, parseErr)

	var rejectErr *oorpb.SubmitRejectedError
	require.True(
		t, errors.As(parseErr, &rejectErr),
		"client-side parse must yield a typed SubmitRejectedError",
	)
	require.Equal(
		t, oorpb.OORRejectCode_OOR_REJECT_LINEAGE_TOO_LARGE,
		rejectErr.Code,
		"client-side typed error must carry the operator's code",
	)
}
