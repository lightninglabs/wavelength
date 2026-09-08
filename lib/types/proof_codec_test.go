package types

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testTxProof builds a small but structurally complete TxProof so the
// codec tests can round-trip a realistic value and seed the fuzzer with
// one.
func testTxProof(t testing.TB) *proof.TxProof {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 1,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: []byte{0x51, 0x20, 0x03, 0x04},
	})

	merkleProof, err := proof.NewTxMerkleProof([]*wire.MsgTx{tx}, 0)
	require.NoError(t, err)

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &proof.TxProof{
		MsgTx: *tx,
		BlockHeader: wire.BlockHeader{
			Version:   4,
			Bits:      0x1d00ffff,
			Timestamp: time.Unix(1_700_000_000, 0),
		},
		BlockHeight: 500,
		MerkleProof: *merkleProof,
		ClaimedOutPoint: wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: 0,
		},
		InternalKey: *priv.PubKey(),
		MerkleRoot: []byte{
			0xaa,
			0xbb,
			0xcc,
			0xdd,
		},
	}
}

// oversizedTxProofRecord is a ten byte TLV payload that declares a
// merkle_root record of 0x3030303030303030 bytes. Found by fuzzing the
// operator's boarding request parser: the trusted-storage Decode path
// hands that length straight to an allocation.
var oversizedTxProofRecord = []byte("\x06\xff00000000")

// TestTxProofCodecRoundTrip verifies that a serialized TxProof decodes
// back to the same value.
func TestTxProofCodecRoundTrip(t *testing.T) {
	t.Parallel()

	original := testTxProof(t)

	raw, err := SerializeTxProof(original)
	require.NoError(t, err)

	decoded, err := DeserializeTxProof(raw)
	require.NoError(t, err)
	require.NotNil(t, decoded)

	require.Equal(t, original.MsgTx.TxHash(), decoded.MsgTx.TxHash())
	require.Equal(
		t, original.BlockHeader.BlockHash(),
		decoded.BlockHeader.BlockHash(),
	)
	require.Equal(t, original.BlockHeight, decoded.BlockHeight)
	require.Equal(t, original.MerkleProof, decoded.MerkleProof)
	require.Equal(t, original.ClaimedOutPoint, decoded.ClaimedOutPoint)
	require.True(t, original.InternalKey.IsEqual(&decoded.InternalKey))
	require.Equal(t, original.MerkleRoot, decoded.MerkleRoot)
}

// TestDeserializeTxProofRejectsOversizedRecord pins the fix for a
// client-reachable panic: a record that declares a length far beyond
// the payload must come back as ErrRecordTooLarge rather than reaching
// the allocator.
func TestDeserializeTxProofRejectsOversizedRecord(t *testing.T) {
	t.Parallel()

	_, err := DeserializeTxProof(oversizedTxProofRecord)
	require.ErrorIs(t, err, tlv.ErrRecordTooLarge)
}

// FuzzDeserializeTxProof feeds arbitrary bytes to the TxProof decoder,
// which the operator runs on every boarding request before any other
// work. The only assertion is that it never panics, and that anything
// it accepts can be serialized again.
func FuzzDeserializeTxProof(f *testing.F) {
	seed, err := SerializeTxProof(testTxProof(f))
	require.NoError(f, err)

	f.Add(seed)
	f.Add(oversizedTxProofRecord)
	f.Add([]byte{})
	f.Add([]byte{0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := DeserializeTxProof(data)
		if err != nil {
			return
		}

		_, err = SerializeTxProof(p)
		require.NoError(t, err)
	})
}
