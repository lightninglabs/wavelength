package tlvutil

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// manualEncode reproduces the inlined NewStream/Encode form that EncodeRecords
// replaces. It is the byte-format oracle the helpers must match exactly.
func manualEncode(t *testing.T, records ...tlv.Record) []byte {
	t.Helper()

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, stream.Encode(&buf))

	return buf.Bytes()
}

// TestEncodeRecordsMatchesManual asserts that EncodeRecords produces output
// byte-for-byte identical to the hand-written NewStream/Encode form across
// arbitrary record sets. This is the wire-compatibility guarantee that lets us
// migrate existing codecs onto the helper without changing on-disk bytes.
func TestEncodeRecordsMatchesManual(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		records := genRecords(rt)

		got, err := EncodeRecordsToBytes(records...)
		require.NoError(t, err)

		want := manualEncode(t, records...)
		require.Equal(t, want, got)
	})
}

// TestEncodeDecodeRecordsRoundTrip asserts that any record set encoded via the
// helpers decodes back to the same values, and that the parsed-type map reports
// every encoded record as present.
func TestEncodeDecodeRecordsRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Generate source values and the records that view them.
		src := genFields(rt)

		buf, err := EncodeRecordsToBytes(src.records()...)
		require.NoError(t, err)

		// Decode into a fresh field set and compare.
		var dst fields
		typeMap, err := DecodeRecordsFromBytes(buf, dst.records()...)
		require.NoError(t, err)

		require.Equal(t, src, dst)

		// Every encoded record must appear in the parsed-type map.
		for _, rec := range src.records() {
			_, ok := typeMap[rec.Type()]
			require.Truef(t, ok, "type %d missing", rec.Type())
		}
	})
}

// TestDecodeRecordsReaderEquivalence asserts the reader and byte-slice decode
// entry points agree.
func TestDecodeRecordsReaderEquivalence(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := genFields(rt)
		buf, err := EncodeRecordsToBytes(src.records()...)
		require.NoError(t, err)

		var viaReader, viaBytes fields
		_, err = DecodeRecords(
			bytes.NewReader(buf), viaReader.records()...,
		)
		require.NoError(t, err)

		_, err = DecodeRecordsFromBytes(buf, viaBytes.records()...)
		require.NoError(t, err)

		require.Equal(t, viaReader, viaBytes)
	})
}

// fields is a fixed set of typed records exercising the common primitive
// encodings used throughout the message codecs.
type fields struct {
	u64  uint64
	u32  uint32
	blob []byte
}

// records returns the TLV records that view this field set, mirroring how a
// real message Encode/Decode builds its record slice.
func (f *fields) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &f.u64),
		tlv.MakePrimitiveRecord(3, &f.u32),
		tlv.MakePrimitiveRecord(5, &f.blob),
	}
}

// genFields draws an arbitrary field set.
func genFields(rt *rapid.T) fields {
	return fields{
		u64:  rapid.Uint64().Draw(rt, "u64"),
		u32:  rapid.Uint32().Draw(rt, "u32"),
		blob: rapid.SliceOf(rapid.Byte()).Draw(rt, "blob"),
	}
}

// genRecords draws an arbitrary set of distinct-typed primitive records backed
// by stable storage for the duration of the check.
func genRecords(rt *rapid.T) []tlv.Record {
	store := struct {
		a uint64
		b uint32
		c []byte
		d uint8
	}{
		a: rapid.Uint64().Draw(rt, "a"),
		b: rapid.Uint32().Draw(rt, "b"),
		c: rapid.SliceOf(rapid.Byte()).Draw(rt, "c"),
		d: rapid.Uint8().Draw(rt, "d"),
	}

	return []tlv.Record{
		tlv.MakePrimitiveRecord(2, &store.a),
		tlv.MakePrimitiveRecord(4, &store.b),
		tlv.MakePrimitiveRecord(6, &store.c),
		tlv.MakePrimitiveRecord(8, &store.d),
	}
}
