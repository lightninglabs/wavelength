package unroll

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestDeferredCheckpointCodecRoundTrip verifies the encoder and decoder
// are inverses across the empty case, the canonical sort order, and a
// representative mix of txids and deadlines.
func TestDeferredCheckpointCodecRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []DeferredCheckpoint
	}{
		{
			name: "empty",
			in:   nil,
		},
		{
			name: "single",
			in: []DeferredCheckpoint{
				{
					Txid: chainhash.Hash{
						0xaa,
					},
					DeadlineHeight: 100,
				},
			},
		},
		{
			name: "multiple sorted by encoder",
			in: []DeferredCheckpoint{
				{
					Txid: chainhash.Hash{
						0xcc,
					},
					DeadlineHeight: 220,
				},
				{
					Txid: chainhash.Hash{
						0xaa,
					},
					DeadlineHeight: 100,
				},
				{
					Txid: chainhash.Hash{
						0xbb,
					},
					DeadlineHeight: 200,
				},
			},
		},
		{
			name: "negative deadline survives uint32 round-trip",
			in: []DeferredCheckpoint{
				{
					Txid: chainhash.Hash{
						0xff,
					},
					DeadlineHeight: -1,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeDeferredCheckpoints(tc.in)
			require.NoError(t, err)

			got, err := decodeDeferredCheckpoints(encoded)
			require.NoError(t, err)

			// Encoder applies canonical sort; compare against the
			// expected sorted form so the test does not depend on
			// input order.
			require.Equal(t, copyDeferredCheckpoints(tc.in), got)
		})
	}
}

// TestDeferredCheckpointCodecForwardCompat verifies that a per-entry TLV
// stream carrying an unknown odd TLV record decodes cleanly. This is
// the load-bearing forward-compat guarantee that Roasbeef asked for: a
// future client can add an extra field (odd type) and older clients
// must skip it instead of failing the whole checkpoint decode.
func TestDeferredCheckpointCodecForwardCompat(t *testing.T) {
	t.Parallel()

	original := DeferredCheckpoint{
		Txid: chainhash.Hash{
			0xab,
			0xcd,
		},
		DeadlineHeight: 7,
	}

	// Splice an unknown odd-type TLV record into the per-entry stream.
	// Layout: outer = varint(count=1) + varint(entryLen) + entry. We
	// rebuild the buffer by re-encoding the entry with the extra
	// record appended.
	txidBytes := original.Txid[:]
	deadline := uint32(original.DeadlineHeight)
	extra := []byte{0xde, 0xad, 0xbe, 0xef}

	const futureRecordType tlv.Type = 5

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			deferredCheckpointTxidRecordType, &txidBytes,
		),
		tlv.MakePrimitiveRecord(
			deferredCheckpointDeadlineRecordType, &deadline,
		),
		tlv.MakePrimitiveRecord(futureRecordType, &extra),
	)
	require.NoError(t, err)

	var entry bytes.Buffer
	require.NoError(t, stream.Encode(&entry))

	var (
		outer   bytes.Buffer
		scratch [8]byte
	)
	require.NoError(t, tlv.WriteVarInt(&outer, 1, &scratch))
	require.NoError(
		t,
		tlv.WriteVarInt(
			&outer,
			uint64(
				entry.Len(),
			),
			&scratch,
		),
	)
	_, err = outer.Write(entry.Bytes())
	require.NoError(t, err)

	// Older decoder (knows only txid + deadline) must still recover
	// the entry; the unknown record is silently skipped because its
	// TLV type is odd.
	got, err := decodeDeferredCheckpoints(outer.Bytes())
	require.NoError(t, err)
	require.Equal(t, []DeferredCheckpoint{original}, got)
}

// TestDeferredCheckpointCodecRejectsShortTxid verifies the decoder
// rejects an entry whose txid record is not exactly 32 bytes, instead
// of silently producing a truncated or zero-padded chainhash.
func TestDeferredCheckpointCodecRejectsShortTxid(t *testing.T) {
	t.Parallel()

	short := []byte{0x01, 0x02, 0x03}
	deadline := uint32(42)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			deferredCheckpointTxidRecordType, &short,
		),
		tlv.MakePrimitiveRecord(
			deferredCheckpointDeadlineRecordType, &deadline,
		),
	)
	require.NoError(t, err)

	var entry bytes.Buffer
	require.NoError(t, stream.Encode(&entry))

	var (
		outer   bytes.Buffer
		scratch [8]byte
	)
	require.NoError(t, tlv.WriteVarInt(&outer, 1, &scratch))
	require.NoError(
		t,
		tlv.WriteVarInt(
			&outer,
			uint64(
				entry.Len(),
			),
			&scratch,
		),
	)
	_, err = outer.Write(entry.Bytes())
	require.NoError(t, err)

	_, err = decodeDeferredCheckpoints(outer.Bytes())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected 32")
}
