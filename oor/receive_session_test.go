package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

func TestReceiveSessionNotifiesAndAcks(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	inputs := []oortx.CheckpointInput{{
		Outpoint: wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		},
		WitnessUtxo: &wire.TxOut{
			Value:    int64(inputValue),
			PkScript: []byte{0x51},
		},
		OwnerLeafScript: []byte{0x51},
	}}

	outputs := []oortx.RecipientOutput{{
		PkScript: []byte{0x51},
		Value:    inputValue,
	}}

	// Build a canonical Ark PSBT for the receive notification.
	cp, err := oortx.BuildCheckpointPSBT(policy, inputs[0])
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT([]oortx.CheckpointOutput{{
		Txid:           cp.PSBT.UnsignedTx.TxHash(),
		Output:         cp.PSBT.UnsignedTx.TxOut[0],
		TapTreeEncoded: cp.TapTreeEncoded,
	}}, outputs)
	require.NoError(t, err)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	_, outbox, err := DriveIncomingTransfer(ctx, sessionID, arkPSBT)
	require.NoError(t, err)
	require.Len(t, outbox, 3)

	_, ok := outbox[0].(*IncomingTransferNotification)
	require.True(t, ok)

	_, ok = outbox[1].(*MaterializeIncomingVTXOsRequest)
	require.True(t, ok)

	_, ok = outbox[2].(*SendIncomingAckRequest)
	require.True(t, ok)
}
