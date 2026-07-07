package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// buildCheckpointForSignValidationTest constructs a checkpoint PSBT for a
// single transfer input to use in operator-signature validation tests.
func buildCheckpointForSignValidationTest(t *testing.T,
	in TransferInput) *psbt.Packet {

	t.Helper()

	require.NotNil(t, in.VTXO)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: in.VTXO.OperatorKey,
		CSVDelay:    in.VTXO.RelativeExpiry,
	}

	res, err := oortx.BuildCheckpointPSBT(policy, oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: in.VTXO.Outpoint,
			Output: &wire.TxOut{
				Value:    int64(in.VTXO.Amount),
				PkScript: in.VTXO.PkScript,
			},
		},
		OwnerLeafScript: in.OwnerLeafScript,
	})
	require.NoError(t, err)

	return res.PSBT
}

// TestSignCheckpointPSBTsRejectsMissingOperatorSignature asserts client
// signing refuses checkpoints without operator signatures.
func TestSignCheckpointPSBTsRejectsMissingOperatorSignature(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)

	in := newTestTransferInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		},
		btcutil.Amount(50_000),
	)
	checkpoint := buildCheckpointForSignValidationTest(t, in)

	err = SignCheckpointPSBTs(
		clientSigner, []TransferInput{in}, []*psbt.Packet{checkpoint},
	)
	require.ErrorContains(
		t, err, "checkpoint missing collaborative tap leaf",
	)
}

// TestSignCheckpointPSBTsRejectsInvalidOperatorSignature asserts client
// signing refuses checkpoints with invalid operator signatures.
func TestSignCheckpointPSBTsRejectsInvalidOperatorSignature(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	in := newTestTransferInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x02},
			Index: 0,
		},
		btcutil.Amount(50_000),
	)
	checkpoint := buildCheckpointForSignValidationTest(t, in)

	err = coSignCheckpointPSBTsForTest(
		operatorSigner, []TransferInput{in}, []*psbt.Packet{checkpoint},
	)
	require.NoError(t, err)

	require.NotEmpty(t, checkpoint.Inputs[0].TaprootScriptSpendSig)
	operatorSig := checkpoint.Inputs[0].TaprootScriptSpendSig[0]
	require.NotNil(t, operatorSig)
	require.NotEmpty(t, operatorSig.Signature)
	operatorSig.Signature[0] ^= 0x01

	err = SignCheckpointPSBTs(
		clientSigner, []TransferInput{in}, []*psbt.Packet{checkpoint},
	)
	require.ErrorContains(t, err, "invalid taproot script signature")
}
