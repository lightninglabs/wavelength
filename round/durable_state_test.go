package round

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestDurableClientStateKinds keeps the complete state union decodable and
// verifies that only states backed by external signer sessions require
// recovery.
func TestDurableClientStateKinds(t *testing.T) {
	states := []ClientState{
		&Idle{}, &PendingRoundAssembly{}, &IntentSentState{},
		&QuoteReceivedState{}, &RoundJoinedState{},
		&CommitmentTxReceivedState{}, &CommitmentTxValidatedState{},
		&ForfeitSignaturesCollectingState{}, &NoncesSentState{},
		&NoncesAggregatedState{}, &PartialSigsSentState{},
		&InputSigSentState{}, &ConfirmedState{},
		&ClientFailedState{
			Reason: "failed",
			Error:  fmt.Errorf("cause"),
		},
		&RecoveryInitiatedState{}, &ServiceReconcileState{
			Cold: true,
		},
	}
	for _, state := range states {
		t.Run(fmt.Sprintf("%T", state), func(t *testing.T) {
			raw, err := encodeClientState(state)
			require.NoError(t, err)
			restored, lostSessions, err := decodeClientState(
				raw, nil,
			)
			require.NoError(t, err)
			require.IsType(t, state, restored)
			expectedLoss := false
			switch state.(type) {
			case *NoncesSentState, *NoncesAggregatedState,
				*PartialSigsSentState:

				expectedLoss = true
			}
			require.Equal(t, expectedLoss, lostSessions)
			again, err := encodeClientState(restored)
			require.NoError(t, err)
			require.Equal(t, raw, again)
		})
	}
}

// TestDurableClientStateSignedArtifacts retains every tree and the exact
// signatures already produced, so replay never requires signing them again.
func TestDurableClientStateSignedArtifacts(t *testing.T) {
	private, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{7}, 32))
	signature, err := schnorr.Sign(private, bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(
			validPSBTBytes(t),
		),
		false,
	)
	require.NoError(t, err)
	point := wire.OutPoint{Hash: chainhash.Hash{
		8,
	},
		Index: 3}
	secondPoint := wire.OutPoint{Hash: chainhash.Hash{
		9,
	},
		Index: 4}
	first := &tree.Tree{
		BatchOutpoint: point,
		BatchOutput: wire.NewTxOut(
			800, []byte{0x51},
		),
		Root: &tree.Node{
			Input:     point,
			Amount:    700,
			FinalKey:  key,
			Signature: signature,
			CoSigners: []*btcec.PublicKey{
				key,
			},
			Outputs: []*wire.TxOut{
				wire.NewTxOut(700, []byte{0x52}),
			},
		},
	}
	second := &tree.Tree{
		BatchOutpoint: secondPoint,
		BatchOutput:   wire.NewTxOut(600, []byte{0x53}),
		Root: &tree.Node{
			Input:    secondPoint,
			Amount:   500,
			FinalKey: key,
			Outputs: []*wire.TxOut{
				wire.NewTxOut(500, []byte{0x54}),
			},
		},
	}
	id := testRoundID("durable-state-signatures")
	signerKey := NewSignerKey(key)
	original := &InputSigSentState{
		RoundID:      id,
		CommitmentTx: packet,
		VTXOTreePaths: map[int]*tree.Tree{
			3: first,
			4: second,
		},
		ClientTrees: map[SignerKey]*tree.Tree{
			signerKey: first,
		},
		Intents: Intents{
			QuotedLeaveAmounts: []int64{
				31,
				29,
			},
		},
		SweepDelay:  144,
		FlowVersion: roundpb.FlowVersion(1),
		ForfeitKey:  key,
		InputSigs: []*types.BoardingInputSignature{{
			InputIndex:      2,
			Outpoint:        point,
			ClientSignature: signature,
		}},
		ForfeitedVTXOs: []wire.OutPoint{
			point,
			secondPoint,
		},
		PendingFailure: &BoardingFailed{
			RoundID:     fn.Some(id),
			Reason:      "uncertain",
			Recoverable: true,
			Error:       fmt.Errorf("transport outcome unknown"),
		},
		ReconcileProbes: 2,
	}
	raw, err := encodeClientState(original)
	require.NoError(t, err)
	decoded, lost, err := decodeClientState(raw, nil)
	require.NoError(t, err)
	require.False(t, lost)
	restored, ok := decoded.(*InputSigSentState)
	require.True(t, ok)
	require.Equal(t, original.RoundID, restored.RoundID)
	require.Equal(t, original.InputSigs, restored.InputSigs)
	require.Equal(t, original.ForfeitedVTXOs, restored.ForfeitedVTXOs)
	require.Equal(t, original.ReconcileProbes, restored.ReconcileProbes)
	require.Equal(t, original.SweepDelay, restored.SweepDelay)
	require.Equal(t, original.FlowVersion, restored.FlowVersion)
	require.True(t, key.IsEqual(restored.ForfeitKey))
	require.Len(t, restored.VTXOTreePaths, 2)
	require.EqualValues(t, 700, restored.VTXOTreePaths[3].Root.Amount)
	require.EqualValues(t, 500, restored.VTXOTreePaths[4].Root.Amount)
	require.True(
		t, key.IsEqual(restored.ClientTrees[signerKey].Root.FinalKey),
	)
	require.Equal(t, signature, restored.VTXOTreePaths[3].Root.Signature)
	require.Equal(
		t, original.PendingFailure.Reason,
		restored.PendingFailure.Reason,
	)
	require.Equal(
		t, original.PendingFailure.Error.Error(),
		restored.PendingFailure.Error.Error(),
	)
	again, err := encodeClientState(restored)
	require.NoError(t, err)
	require.Equal(t, raw, again)
}

// TestDurableClientStateRejectsMissingData rejects a partial checkpoint instead
// of restoring a default state that could lose ownership or positional data.
func TestDurableClientStateRejectsMissingData(t *testing.T) {
	_, _, err := decodeClientState(nil, nil)
	require.Error(t, err)
	raw, err := encodeClientState(&IntentSentState{})
	require.NoError(t, err)
	_, _, err = decodeClientState(raw[:len(raw)-1], nil)
	require.Error(t, err)
}
