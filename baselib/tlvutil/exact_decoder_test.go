package tlvutil

import (
	"bytes"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// readNDecoder returns a decoder that reads exactly n bytes into the *[]byte
// value, regardless of the framed length.
func readNDecoder(n int) tlv.Decoder {
	return func(r io.Reader, val interface{}, _ *[8]byte, _ uint64) error {
		out := make([]byte, n)
		if _, err := io.ReadFull(r, out); err != nil {
			return err
		}

		*(val.(*[]byte)) = out

		return nil
	}
}

// TestExactDecoder checks that a wrapped decoder is held to the framed length
// of its record and fails closed on a mismatch.
func TestExactDecoder(t *testing.T) {
	t.Parallel()

	// A record of type 1 framing 3 value bytes, followed by a record of
	// type 3 framing 1 value byte.
	stream := []byte{0x01, 0x03, 0xaa, 0xbb, 0xcc, 0x03, 0x01, 0xdd}

	decode := func(n int) error {
		var first, second []byte
		_, err := DecodeRecords(
			bytes.NewReader(stream),
			tlv.MakeDynamicRecord(
				1, &first,
				func() uint64 { return 3 }, tlv.EVarBytes,
				ExactDecoder(
					readNDecoder(n),
				),
			),
			tlv.MakePrimitiveRecord(3, &second),
		)

		return err
	}

	// A decoder that reads the whole record is accepted.
	require.NoError(t, decode(3))

	// An under-reading decoder would leave value bytes to be parsed as the
	// next record header.
	require.ErrorIs(t, decode(2), ErrRecordLengthMismatch)

	// An over-reading decoder is cut off at the record boundary rather
	// than swallowing the next record.
	require.Error(t, decode(4))
}
