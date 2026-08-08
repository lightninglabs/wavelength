package serverconn

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrInvalidTLV is the sentinel wrapped by the bounded TLV decode
// helpers so callers can classify a malformed or oversized payload as
// an external decode failure rather than an internal bug.
var ErrInvalidTLV = errors.New("invalid tlv payload")

// maxServerConnMessageSize bounds the total wire size of an untrusted
// serverconn TLV payload before any individual record length is
// honored. These messages wrap mailbox envelopes / Any-packed proto
// bodies plus a handful of length-prefixed identifier blobs, all of
// which are bounded by the mailbox transport. 4 MiB gives generous
// headroom for the largest legitimate proto body while still
// rejecting the unbounded-allocation DoS where a tiny payload declares
// a multi-gigabyte (or near-2^64) record length and crosses the
// client/server trust boundary into the durable mailbox.
const maxServerConnMessageSize = 4 << 20

// safeTLVReader reads an untrusted TLV payload into a size-capped
// buffer and pre-validates the (type, length, value) framing so the
// downstream tlv.Stream.DecodeWithParsedTypes can never be handed a
// record length larger than the bytes physically present.
//
// This is load-bearing because the tlv library sizes its value buffers
// with make([]byte, declaredLength) BEFORE reading any value bytes:
// stream.go does bytes.NewBuffer(make([]byte, 0, length)) for unknown
// records, and primitive.go's DVarBytes does make([]byte, l) for known
// []byte fields. A producer-declared length near 2^64 panics the
// decoder with "makeslice: cap out of range", and a multi-gigabyte
// length drives an OOM. Both are reachable from a few attacker
// controlled bytes that persist in the durable mailbox and are
// replayed from disk across upgrades. By rejecting any record whose
// declared length exceeds the remaining buffer we cap every allocation
// at the message size, which is itself capped at
// maxServerConnMessageSize.
func safeTLVReader(r io.Reader) (io.Reader, error) {
	// Read at most maxServerConnMessageSize+1 so an over-cap message is
	// detected without buffering an unbounded amount.
	limited := io.LimitReader(r, maxServerConnMessageSize+1)

	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read serverconn message: %v",
			ErrInvalidTLV, err)
	}

	if len(buf) > maxServerConnMessageSize {
		return nil, fmt.Errorf("%w: message %d bytes exceeds max %d",
			ErrInvalidTLV, len(buf), maxServerConnMessageSize)
	}

	if err := validateTLVRecordLengths(buf); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf), nil
}

// validateTLVRecordLengths walks the (type, length, value) framing of
// a buffered TLV stream and rejects any record whose declared length
// exceeds the bytes remaining in the buffer. It deliberately does not
// interpret record contents; it only ensures the framing cannot drive
// an over-sized make() in the real decoder.
func validateTLVRecordLengths(buf []byte) error {
	var scratch [8]byte
	br := bytes.NewReader(buf)

	for br.Len() > 0 {
		// The type varint may legitimately end the stream cleanly.
		if _, err := tlv.ReadVarInt(br, &scratch); err != nil {
			return fmt.Errorf("%w: read tlv type: %v",
				ErrInvalidTLV, err)
		}

		length, err := tlv.ReadVarInt(br, &scratch)
		if err != nil {
			return fmt.Errorf("%w: read tlv length: %v",
				ErrInvalidTLV, err)
		}

		// A record can never carry more value bytes than remain in
		// the buffer; reject before the decoder allocates.
		if length > uint64(br.Len()) {
			return fmt.Errorf("%w: tlv record length %d exceeds "+
				"%d remaining bytes", ErrInvalidTLV, length,
				br.Len())
		}

		if _, err := br.Seek(
			int64(length), io.SeekCurrent,
		); err != nil {
			return fmt.Errorf("%w: skip tlv value: %v",
				ErrInvalidTLV, err)
		}
	}

	return nil
}
