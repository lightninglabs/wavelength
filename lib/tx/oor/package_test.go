package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/stretchr/testify/require"
)

// TestSubmitPackageMarshalRoundTrip asserts the submit package JSON encoding is
// stable and round-trippable.
func TestSubmitPackageMarshalRoundTrip(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{})
	checkpointTx.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	checkpointPSBT, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	arkTx.AddTxOut(scripts.AnchorOutput())
	arkPSBT, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	pkg := &SubmitPackage{
		ArkPSBT:         arkPSBT,
		CheckpointPSBTs: []*psbt.Packet{checkpointPSBT},
	}

	b, err := MarshalSubmitPackage(pkg)
	require.NoError(t, err)

	parsed, err := UnmarshalSubmitPackage(b)
	require.NoError(t, err)
	require.NotNil(t, parsed)

	id1, err := pkg.SessionID()
	require.NoError(t, err)

	id2, err := parsed.SessionID()
	require.NoError(t, err)
	require.Equal(t, id1, id2)
}
