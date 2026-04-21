package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestTransferInputSnapshotRoundTrip asserts that transfer input snapshots
// contain enough information to rebuild the VTXO signing descriptor.
func TestTransferInputSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	exitDelay := uint32(10)

	tapScript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	tapKey, err := arkscript.VTXOTapKey(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	ownerLeaf, err := arkscript.MultiSigCollabTapLeaf(
		clientKey.PubKey(), operatorKey.PubKey(),
	)
	require.NoError(t, err)

	ownerLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				clientKey.PubKey(),
				operatorKey.PubKey(),
			},
		},
	}.Encode()
	require.NoError(t, err)

	vtxoPolicyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	in := &TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash:  [32]byte{1},
				Index: 2,
			},
			Amount:   btcutil.Amount(5000),
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:    operatorKey.PubKey(),
			TapScript:      tapScript,
			RelativeExpiry: exitDelay,
			Status:         vtxo.VTXOStatusLive,
		},
		VTXOPolicyTemplate: vtxoPolicyTemplate,
		OwnerLeafScript:    ownerLeaf.Script,
		OwnerLeafPolicy:    ownerLeafPolicy,
		CustomSpend: &arkscript.SpendPath{
			SpendInfo: &arkscript.SpendInfo{
				WitnessScript: []byte{txscript.OP_TRUE},
				ControlBlock:  []byte{0xc0, 0x01},
			},
			RequiredSequence: 12,
			RequiredLockTime: 500,
			Conditions: [][]byte{
				{0xaa, 0xbb},
			},
		},
	}

	snap, err := in.ToSnapshot()
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Equal(t, in.VTXO.Outpoint, snap.Outpoint)
	require.Equal(t, int64(in.VTXO.Amount), snap.AmountSat)
	require.Equal(t, int32(in.VTXO.ClientKey.KeyLocator.Family),
		snap.ClientKeyFamily)
	require.Equal(t, in.VTXO.ClientKey.KeyLocator.Index,
		snap.ClientKeyIndex)
	require.Equal(t, in.VTXO.ClientKey.PubKey.SerializeCompressed(),
		snap.ClientPubKey)
	require.Equal(t, in.VTXO.OperatorKey.SerializeCompressed(),
		snap.OperatorPubKey)
	require.Equal(t, in.VTXO.RelativeExpiry, snap.ExitDelay)
	require.Equal(t, in.OwnerLeafScript, snap.OwnerLeafScript)
	require.Equal(t, in.OwnerLeafPolicy, snap.OwnerLeafPolicy)
	require.Equal(t, in.VTXOPolicyTemplate, snap.VTXOPolicyTemplate)
	require.Equal(t, in.CustomSpend.RequiredSequence,
		snap.SpendRequiredSequence)
	require.Equal(t, in.CustomSpend.RequiredLockTime,
		snap.SpendRequiredLockTime)

	rebuilt, err := TransferInputFromSnapshot(snap)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.VTXO)
	require.Equal(t, in.VTXO.Outpoint, rebuilt.VTXO.Outpoint)
	require.Equal(t, in.VTXO.Amount, rebuilt.VTXO.Amount)
	require.Equal(t, in.VTXO.PkScript, rebuilt.VTXO.PkScript)
	require.Equal(t, in.VTXO.ClientKey.KeyLocator,
		rebuilt.VTXO.ClientKey.KeyLocator)
	require.Equal(t, in.VTXO.ClientKey.PubKey.SerializeCompressed(),
		rebuilt.VTXO.ClientKey.PubKey.SerializeCompressed())
	require.Equal(t, in.VTXO.OperatorKey.SerializeCompressed(),
		rebuilt.VTXO.OperatorKey.SerializeCompressed())
	require.Equal(t, in.VTXO.RelativeExpiry, rebuilt.VTXO.RelativeExpiry)
	require.NotNil(t, rebuilt.VTXO.TapScript)
	require.Equal(t, in.OwnerLeafScript, rebuilt.OwnerLeafScript)
	require.Equal(t, in.OwnerLeafPolicy, rebuilt.OwnerLeafPolicy)
	require.Equal(t, in.VTXOPolicyTemplate, rebuilt.VTXOPolicyTemplate)
	require.NotNil(t, rebuilt.CustomSpend)
	require.Equal(t, in.CustomSpend.RequiredSequence,
		rebuilt.CustomSpend.RequiredSequence)
	require.Equal(t, in.CustomSpend.RequiredLockTime,
		rebuilt.CustomSpend.RequiredLockTime)
}

// TestTransferInputValidateRejectsNil asserts nil receivers are rejected.
func TestTransferInputValidateRejectsNil(t *testing.T) {
	t.Parallel()

	var in *TransferInput
	err := in.Validate()
	require.Error(t, err)
}

// TestTransferInputFromSnapshotRejectsMissingFields asserts malformed snapshots
// are rejected early.
func TestTransferInputFromSnapshotRejectsMissingFields(t *testing.T) {
	t.Parallel()

	_, err := TransferInputFromSnapshot(nil)
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{})
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{
		AmountSat: 1,
	})
	require.Error(t, err)

	_, err = TransferInputFromSnapshot(&TransferInputSnapshot{
		AmountSat:       1,
		ClientPubKey:    []byte{0x02},
		OperatorPubKey:  []byte{0x02},
		ExitDelay:       1,
		OwnerLeafScript: []byte{0x51},
	})
	require.Error(t, err)
}

// TestTransferInputToSnapshotRejectsMissingVTXO asserts we require a full VTXO
// descriptor before snapshotting.
func TestTransferInputToSnapshotRejectsMissingVTXO(t *testing.T) {
	t.Parallel()

	in := &TransferInput{}
	_, err := in.ToSnapshot()
	require.Error(t, err)
}
