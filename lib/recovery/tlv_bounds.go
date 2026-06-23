package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrInvalidTLV is the sentinel wrapped by the bounded TLV decode helpers so
// callers can classify a malformed or oversized payload as an external decode
// failure rather than an internal bug.
var ErrInvalidTLV = errors.New("invalid tlv payload")

// maxRecoveryMessageSize bounds the total wire size of an untrusted recovery
// TLV payload before any individual record length is honored. A proof can
// embed up to MaxProofNodes (100_000) recovery transactions, each carrying a
// serialized wire.MsgTx, so the cap is generous at 64 MiB while still
// rejecting the unbounded-allocation DoS where a tiny payload declares a
// multi-gigabyte (or near-2^64) record length.
//
// Recovery proofs and session state persist durably and are rebuilt on
// restart, so the bound is load-bearing on the replay path: a corrupt or
// hostile on-disk blob must decode to an error rather than crash the daemon at
// boot.
const maxRecoveryMessageSize = 64 << 20

// safeRecoveryTLVBytes validates an in-memory TLV blob and returns a reader
// positioned at its start. It pre-validates the (type, length) framing so the
// downstream tlv.Stream decoders can never be handed a record length larger
// than the bytes physically present.
//
// This is required because the tlv library sizes its value buffers with
// make([]byte, declaredLength) BEFORE reading any value bytes: stream.go's
// unknown-record path does bytes.NewBuffer(make([]byte, 0, length)) and
// primitive.go's DVarBytes does make([]byte, l). Neither bounds length against
// the input on the non-P2P DecodeWithParsedTypes path. A producer-declared
// length near 2^64 panics the decoder with "makeslice: cap out of range", and
// a multi-gigabyte length drives an OOM. By rejecting any record whose declared
// length exceeds the remaining buffer (a record can never legitimately carry
// more bytes than the message contains), we cap every allocation at the
// message size, which is itself capped at maxRecoveryMessageSize.
func safeRecoveryTLVBytes(raw []byte) (*bytes.Reader, error) {
	if len(raw) > maxRecoveryMessageSize {
		return nil, fmt.Errorf("%w: message %d bytes exceeds max %d",
			ErrInvalidTLV, len(raw), maxRecoveryMessageSize)
	}

	if err := validateTLVRecordLengths(raw); err != nil {
		return nil, err
	}

	return bytes.NewReader(raw), nil
}

// validateTLVRecordLengths walks the (type, length, value) framing of a
// buffered TLV stream and rejects any record whose declared length exceeds the
// bytes remaining in the buffer. It deliberately does not interpret record
// contents; it only ensures the framing cannot drive an over-sized make() in
// the real decoder before any value bytes are read.
func validateTLVRecordLengths(buf []byte) error {
	var scratch [8]byte
	br := bytes.NewReader(buf)

	for br.Len() > 0 {
		// The type varint read only reaches EOF when br.Len() == 0
		// above; any failure here is a malformed stream.
		if _, err := tlv.ReadVarInt(br, &scratch); err != nil {
			return fmt.Errorf("%w: read tlv type: %v",
				ErrInvalidTLV, err)
		}

		length, err := tlv.ReadVarInt(br, &scratch)
		if err != nil {
			return fmt.Errorf("%w: read tlv length: %v",
				ErrInvalidTLV, err)
		}

		// A record can never carry more value bytes than remain in the
		// buffer; reject before the decoder allocates make([]byte,
		// length).
		if length > uint64(br.Len()) {
			return fmt.Errorf("%w: tlv record length %d exceeds %d "+
				"remaining bytes", ErrInvalidTLV, length,
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
