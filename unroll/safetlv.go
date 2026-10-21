package unroll

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightninglabs/wavelength/baselib/tlvutil"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrTLVRecordTooLarge signals that a TLV record declared a length larger than
// the bytes physically present in the input. The lnd tlv stream decoder's
// non-P2P path sizes make([]byte, length) (both for known DVarBytes records
// and the unknown-record discard buffer) directly from this attacker-controlled
// length without bounding it against the reader, so a tiny payload declaring a
// huge length panics (makeslice) or OOMs. The durable mailbox replays these
// message and checkpoint payloads from disk across rolling upgrades, so a
// tampered or truncated blob is a hostile input. We pre-validate framing to
// fail closed instead.
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

// boundedReader drains r into memory and validates its framing. It serves the
// mailbox message decoders, which receive an io.Reader (the outer codec frames
// each message) rather than a byte slice. Reading the message fully into
// memory is safe: the outer durable mailbox codec has already size-capped the
// per-message payload before handing it to us.
func boundedReader(r io.Reader) (*bytes.Reader, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	return safeTLVReader(raw)
}

// safeDecodeRecords decodes r into the passed records after validating the
// record framing, returning the parsed-type map.
func safeDecodeRecords(r io.Reader,
	records ...tlv.Record) (tlv.TypeMap, error) {

	safeReader, err := boundedReader(r)
	if err != nil {
		return nil, err
	}

	return tlvutil.DecodeRecords(safeReader, records...)
}

// safeDecodeRecordsDiscardUnknown is safeDecodeRecords for codecs that do not
// need the parsed-type map: unknown values are streamed to io.Discard.
func safeDecodeRecordsDiscardUnknown(r io.Reader, records ...tlv.Record) error {
	safeReader, err := boundedReader(r)
	if err != nil {
		return err
	}

	return tlvutil.DecodeRecordsDiscardUnknown(safeReader, records...)
}

// validateTLVFraming walks the record framing of an in-memory TLV blob and
// rejects any record whose declared length is larger than the remaining bytes.
func validateTLVFraming(raw []byte) error {
	var scratch [8]byte

	reader := bytes.NewReader(raw)
	for reader.Len() > 0 {
		// A short read on the framing varints means the payload is
		// truncated, so reject it rather than hand it to the decoder.
		if _, err := tlv.ReadVarInt(reader, &scratch); err != nil {
			return fmt.Errorf("malformed tlv framing: %w", err)
		}

		length, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return fmt.Errorf("malformed tlv framing: %w", err)
		}

		if length > uint64(reader.Len()) {
			return fmt.Errorf("%w: declared %d, %d remaining",
				ErrTLVRecordTooLarge, length, reader.Len())
		}

		if _, err := reader.Seek(
			int64(length), io.SeekCurrent,
		); err != nil {
			return fmt.Errorf("malformed tlv framing: %w", err)
		}
	}

	return nil
}
