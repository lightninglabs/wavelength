package tlvutil

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// genOutPoint draws an arbitrary outpoint.
func genOutPoint(rt *rapid.T) wire.OutPoint {
	var h chainhash.Hash
	copy(h[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "hash"))

	return wire.OutPoint{
		Hash:  h,
		Index: rapid.Uint32().Draw(rt, "index"),
	}
}

// legacyOutPointBytes reproduces the hand-rolled 36-byte little-endian outpoint
// encoding that the ledger and OOR codecs used before OutPointRecord. It is the
// byte-format oracle the shared record must match exactly.
func legacyOutPointBytes(op wire.OutPoint) []byte {
	var buf bytes.Buffer
	buf.Write(op.Hash[:])

	var idx [4]byte
	binary.LittleEndian.PutUint32(idx[:], op.Index)
	buf.Write(idx[:])

	return buf.Bytes()
}

// TestOutPointRecordMatchesLegacy asserts the OutPointRecord payload is
// byte-identical to the legacy 36-byte little-endian encoding, and round-trips.
func TestOutPointRecordMatchesLegacy(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		op := genOutPoint(rt)

		// The record payload (TLV value, sans type/length) must equal
		// the legacy fixed 36-byte form.
		src := op
		buf, err := EncodeRecordsToBytes(OutPointRecord(1, &src))
		require.NoError(t, err)

		// Strip the 2-byte type+length TLV header (type 1, length 36
		// both fit in a single byte each) to compare the raw value.
		require.Equal(t, byte(1), buf[0])
		require.Equal(t, byte(outPointSize), buf[1])
		require.Equal(t, legacyOutPointBytes(op), buf[2:])

		// Round-trip back to an equal outpoint.
		var got wire.OutPoint
		_, err = DecodeRecordsFromBytes(buf, OutPointRecord(1, &got))
		require.NoError(t, err)
		require.Equal(t, op, got)
	})
}

// TestOutPointRecordRejectsWrongSize asserts the decoder rejects a payload that
// is not exactly 36 bytes.
func TestOutPointRecordRejectsWrongSize(t *testing.T) {
	t.Parallel()

	// Hand-craft a TLV stream: type 1, length 4, four bytes. The static
	// record decoder must reject the short length.
	stream := []byte{0x01, 0x04, 0x00, 0x00, 0x00, 0x00}

	var got wire.OutPoint
	_, err := DecodeRecordsFromBytes(stream, OutPointRecord(1, &got))
	require.Error(t, err)
}

// TestNativePrimitivesReused documents and locks in that the domain types we do
// NOT wrap are handled directly by tlv.MakePrimitiveRecord, so no helper is
// needed. A regression that dropped native support would surface here.
func TestNativePrimitivesReused(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// *btcec.PublicKey via the native **btcec.PublicKey case.
		scalar := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			rt, "scalar",
		)
		var modScalar btcec.ModNScalar
		if modScalar.SetByteSlice(scalar); modScalar.IsZero() {
			modScalar.SetInt(1)
		}
		pub := btcec.PrivKeyFromScalar(&modScalar).PubKey()

		// chainhash.Hash via the native *[32]byte case (array cast).
		var h chainhash.Hash
		copy(h[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "h"))

		srcPub, srcH := pub, h
		buf, err := EncodeRecordsToBytes(
			tlv.MakePrimitiveRecord(1, &srcPub),
			tlv.MakePrimitiveRecord(
				3, (*[32]byte)(&srcH),
			),
			OutPointRecord(
				5, &wire.OutPoint{
					Hash:  h,
					Index: 7,
				},
			),
		)
		require.NoError(t, err)

		var gotPub *btcec.PublicKey
		var gotH chainhash.Hash
		var gotOP wire.OutPoint
		_, err = DecodeRecordsFromBytes(
			buf, tlv.MakePrimitiveRecord(1, &gotPub),
			tlv.MakePrimitiveRecord(
				3, (*[32]byte)(&gotH),
			),
			OutPointRecord(5, &gotOP),
		)
		require.NoError(t, err)
		require.True(t, pub.IsEqual(gotPub))
		require.Equal(t, h, gotH)
		require.Equal(t, wire.OutPoint{Hash: h, Index: 7}, gotOP)
	})
}
