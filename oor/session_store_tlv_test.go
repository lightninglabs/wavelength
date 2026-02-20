package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// makeSessionSnapshotPSBT builds a compact PSBT fixture with one input/output.
func makeSessionSnapshotPSBT(t *testing.T, seed byte) *psbt.Packet {
	t.Helper()

	tx := wire.NewMsgTx(2)

	var hash chainhash.Hash
	hash[0] = seed

	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(seed),
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(1000 + int(seed)),
		PkScript: []byte{0x51},
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return pkt
}

// TestSerializePSBTRoundTrip verifies the serializePSBT/deserializePSBT
// helper pair produces byte-identical packets.
func TestSerializePSBTRoundTrip(t *testing.T) {
	t.Parallel()

	pkt := makeSessionSnapshotPSBT(t, 42)

	b, err := serializePSBT(pkt)
	require.NoError(t, err)

	restored, err := deserializePSBT(b)
	require.NoError(t, err)

	// Re-serialize to verify byte equality.
	b2, err := serializePSBT(restored)
	require.NoError(t, err)

	require.Equal(t, b, b2)
}

// TestSerializePSBTNilError ensures serializePSBT rejects nil packets.
func TestSerializePSBTNilError(t *testing.T) {
	t.Parallel()

	_, err := serializePSBT(nil)
	require.ErrorContains(t, err, "psbt must be provided")
}

// TestDeserializePSBTEmptyError ensures deserializePSBT rejects empty bytes.
func TestDeserializePSBTEmptyError(t *testing.T) {
	t.Parallel()

	_, err := deserializePSBT(nil)
	require.ErrorContains(t, err, "psbt bytes must be provided")
}
