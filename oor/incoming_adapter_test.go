package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestIsIncomingResolveCorrelationID verifies only durable incoming-transfer
// resolution query correlation IDs match the OOR resolve route prefix.
func TestIsIncomingResolveCorrelationID(t *testing.T) {
	t.Parallel()

	var sessionID SessionID
	sessionID[0] = 1

	require.True(
		t,
		IsIncomingResolveCorrelationID(
			IncomingResolveCorrelationID(sessionID, 7),
		),
	)
	require.False(t, IsIncomingResolveCorrelationID(""))
	require.False(
		t, IsIncomingResolveCorrelationID("00aa8bfb11f09881bbd2"),
	)
	require.False(
		t, IsIncomingResolveCorrelationID(
			incomingResolveCorrelationPrefix,
		),
	)
}

// TestIncomingTransferEventFromResponseAcceptsValidCheckpointWitness proves
// full receiver validation preserves the normal finalized OOR package path.
func TestIncomingTransferEventFromResponseAcceptsValidCheckpointWitness(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)

	event, err := IncomingTransferEventFromResponse(
		sessionID, recipientEventID, resp,
	)
	require.NoError(t, err)
	require.Equal(t, sessionID, event.SessionID)
}

// TestIncomingTransferEventFromResponseRejectsInvalidCheckpointWitness proves
// receiver admission executes checkpoint witnesses before the resolved package
// can enter the durable receive state machine. A correctly sized garbage
// key-spend signature satisfies structural validation but cannot spend the
// checkpoint's exact VTXO prevout.
func TestIncomingTransferEventFromResponseRejectsInvalidCheckpointWitness(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)
	checkpoint, err := psbtutil.Parse(resp.Events[0].CheckpointPsbts[0])
	require.NoError(t, err)

	checkpoint.Inputs[0].FinalScriptWitness = nil
	checkpoint.Inputs[0].TaprootScriptSpendSig = nil
	checkpoint.Inputs[0].TaprootKeySpendSig = bytes.Repeat(
		[]byte{0x01}, 64,
	)
	resp.Events[0].CheckpointPsbts[0], err = psbtutil.Serialize(checkpoint)
	require.NoError(t, err)

	_, err = IncomingTransferEventFromResponse(
		sessionID, recipientEventID, resp,
	)
	require.ErrorContains(t, err, "script validation failed")
}

// TestIncomingTransferEventFromResponseRejectsWrongCheckpointPrevout proves
// signature validation commits to the exact witness UTXO value supplied for
// the checkpoint input. Replacing that prevout after signing invalidates the
// package before receiver admission.
func TestIncomingTransferEventFromResponseRejectsWrongCheckpointPrevout(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)
	checkpoint, err := psbtutil.Parse(resp.Events[0].CheckpointPsbts[0])
	require.NoError(t, err)
	require.NotNil(t, checkpoint.Inputs[0].WitnessUtxo)

	checkpoint.Inputs[0].WitnessUtxo.Value++
	resp.Events[0].CheckpointPsbts[0], err = psbtutil.Serialize(checkpoint)
	require.NoError(t, err)

	_, err = IncomingTransferEventFromResponse(
		sessionID, recipientEventID, resp,
	)
	require.ErrorContains(t, err, "script validation failed")
}

// TestIncomingTransferEventFromResponseRejectsInvalidControlBlock proves the
// receiver binds the revealed collaborative leaf and control block to the
// checkpoint's exact taproot prevout before accepting the package.
func TestIncomingTransferEventFromResponseRejectsInvalidControlBlock(
	t *testing.T) {

	t.Parallel()

	resp, sessionID, _, recipientEventID := buildIncomingResolveResponse(t)
	checkpoint, err := psbtutil.Parse(resp.Events[0].CheckpointPsbts[0])
	require.NoError(t, err)
	require.NotEmpty(t, checkpoint.Inputs[0].TaprootLeafScript)
	require.NotEmpty(
		t, checkpoint.Inputs[0].TaprootLeafScript[0].ControlBlock,
	)

	checkpoint.Inputs[0].TaprootLeafScript[0].ControlBlock[0] ^= 0x01
	resp.Events[0].CheckpointPsbts[0], err = psbtutil.Serialize(checkpoint)
	require.NoError(t, err)

	_, err = IncomingTransferEventFromResponse(
		sessionID, recipientEventID, resp,
	)
	require.ErrorContains(t, err, "script validation failed")
}

// TestValidateIncomingPackageGraphAcceptsReorderedCheckpoints proves receiver
// validation matches checkpoints to canonical Ark inputs by transaction ID,
// not by attacker-controlled package order.
func TestValidateIncomingPackageGraphAcceptsReorderedCheckpoints(t *testing.T) {
	t.Parallel()

	ark, checkpoints, _, _, _, _ :=
		buildTestIncomingMaterializationMultiInput(t)
	require.Len(t, checkpoints, 2)

	reordered := []*psbt.Packet{checkpoints[1], checkpoints[0]}
	root := packageArtifactForValidation(
		SessionID(
			ark.UnsignedTx.TxHash(),
		),
		ark,
		reordered,
	)

	require.NoError(t, validateIncomingPackageGraph(root, nil))
}
