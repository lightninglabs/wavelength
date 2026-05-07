package indexer

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// TestRPCVTXOFromDBIncludesOperatorKey verifies ListVTXOsByScripts response
// builders carry the round operator key needed by incoming materialization.
func TestRPCVTXOFromDBIncludesOperatorKey(t *testing.T) {
	t.Parallel()

	batchIndex := int32(2)
	roundID := rounds.RoundID{1, 2, 3}
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey := operatorPriv.PubKey().SerializeCompressed()

	out, err := rpcVTXOFromDB(VTXORow{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{9, 9, 9},
			Index: 1,
		},
		BatchOutputIndex: &batchIndex,
		Amount:           1000,
		PkScript:         []byte{0x51},
		Status:           "live",
		RoundID:          &roundID,
	}, &RoundRow{
		RoundID:        roundID,
		CommitmentTxid: chainhash.Hash{8, 8, 8},
		CsvDelay:       144,
		OperatorPubKey: operatorKey,
	})
	require.NoError(t, err)
	require.Equal(t, operatorKey, out.GetOperatorPubkey())

	operatorKey[0] ^= 0x01
	require.NotEqual(t, operatorKey, out.GetOperatorPubkey())
}
