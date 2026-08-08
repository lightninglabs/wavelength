package unrollplan

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrTLVRecordTooLarge signals that a TLV record declared a length larger than
// the bytes physically present in the input. The lnd tlv stream decoder's
// non-P2P path sizes make([]byte, length) (both for known DVarBytes records
// and the unknown-record discard buffer) directly from this attacker-controlled
// length without bounding it against the reader, so a tiny payload declaring a
// huge length panics (makeslice) or OOMs. The unrollplan State is persisted
// durably (nested inside the unroll actor checkpoint) and replayed across
// upgrades, so a tampered or truncated blob is a hostile input. We pre-validate
// framing to fail closed instead.
var ErrTLVRecordTooLarge = errors.New("tlv record length exceeds input")

// safeTLVReader validates the (type, length) framing of an in-memory TLV blob
// against the bytes physically present, then returns a reader over the same
// bytes for the real decode. Rejecting an over-long record up front means the
// subsequent tlv.Stream.Decode can never reach the unbounded make([]byte,
// length) site with a length larger than the input.
func safeTLVReader(raw []byte) (*bytes.Reader, error) {
	if err := validateTLVFraming(raw); err != nil {
		return nil, err
	}

	return bytes.NewReader(raw), nil
}

// validateTLVFraming walks the record framing of an in-memory TLV blob and
// rejects any record whose declared length is larger than the remaining bytes.
func validateTLVFraming(raw []byte) error {
	var scratch [8]byte

	reader := bytes.NewReader(raw)
	for reader.Len() > 0 {
		// A short read on the framing varints is a normal truncation
		// the real decoder will also surface, so we stop validating and
		// defer to its canonical error.
		if _, err := tlv.ReadVarInt(reader, &scratch); err != nil {
			return nil
		}

		length, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil
		}

		if length > uint64(reader.Len()) {
			return fmt.Errorf("%w: declared %d, %d remaining",
				ErrTLVRecordTooLarge, length, reader.Len())
		}

		if _, err := reader.Seek(
			int64(length), io.SeekCurrent,
		); err != nil {
			return nil
		}
	}

	return nil
}
