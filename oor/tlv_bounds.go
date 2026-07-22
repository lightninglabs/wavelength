package oor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// maxOORMessageSize bounds the total wire size of an untrusted OOR TLV
// message before any individual record length is honored. OOR messages
// carry PSBT-bearing payloads (Ark + checkpoint transactions, ancestor
// packages), so the cap is generous at 16 MiB while still rejecting the
// unbounded-allocation DoS where a tiny payload declares a multi-gigabyte
// (or near-2^64) record length. These payloads cross the client/server
// trust boundary and persist in durable actor mailboxes replayed across
// upgrades, so the bound is load-bearing on both the live and replay path.
const maxOORMessageSize = 16 << 20

// safeOORTLVReader reads an untrusted OOR TLV payload into a size-capped
// buffer and pre-validates the (type, length) framing so the downstream
// tlv.Stream.DecodeWithParsedTypes can never be handed a record length
// larger than the bytes physically present in the message.
//
// This is required because the tlv library sizes its value buffers with
// make([]byte, declaredLength) BEFORE reading any value bytes: stream.go's
// unknown-record path does bytes.NewBuffer(make([]byte, 0, length)) and
// primitive.go's DVarBytes does make([]byte, l). Neither bounds length
// against the input on the non-P2P Decode path. A producer-declared length
// near 2^64 panics the decoder with "makeslice: cap out of range", and a
// multi-gigabyte length drives an OOM. By rejecting any record whose
// declared length exceeds the remaining buffer (a record can never
// legitimately carry more bytes than the message contains), we cap every
// allocation at the message size, which is itself capped at
// maxOORMessageSize.
func safeOORTLVReader(r io.Reader) (*bytes.Reader, error) {
	// Read at most maxOORMessageSize+1 so an over-cap message is detected
	// without buffering an unbounded amount.
	limited := io.LimitReader(r, maxOORMessageSize+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read oor message: %w", err)
	}

	if len(buf) > maxOORMessageSize {
		return nil, fmt.Errorf("oor message %d bytes exceeds max %d",
			len(buf), maxOORMessageSize)
	}

	if err := validateTLVRecordLengths(buf); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf), nil
}

// safeOORTLVBytes validates an in-memory OOR TLV blob and returns a reader
// positioned at its start. It is the []byte-input analog of
// safeOORTLVReader for the many nested decoders that already receive a
// fully-buffered payload extracted from a parent record.
func safeOORTLVBytes(raw []byte) (*bytes.Reader, error) {
	if len(raw) > maxOORMessageSize {
		return nil, fmt.Errorf("oor message %d bytes exceeds max %d",
			len(raw), maxOORMessageSize)
	}

	if err := validateTLVRecordLengths(raw); err != nil {
		return nil, err
	}

	return bytes.NewReader(raw), nil
}

// decodeBoundedStream pre-validates the (type, length) framing of an
// in-memory OOR TLV blob and only then decodes it through the supplied
// stream. Pre-validation guarantees the tlv decoder is never handed a
// record length larger than the bytes present, so its internal
// make([]byte, declaredLength) allocations are bounded by the blob size
// (itself capped at maxOORMessageSize). It returns the parsed type map so
// callers that inspect record presence keep their existing behavior.
//
// This is the shared choke point every oor TLV decode routes through so
// the bound holds uniformly across the many heterogeneous decoders without
// duplicating the validation at each call site.
func decodeBoundedStream(stream *tlv.Stream, raw []byte) (tlv.TypeMap,
	error) {

	reader, err := safeOORTLVBytes(raw)
	if err != nil {
		return nil, err
	}

	return stream.DecodeWithParsedTypes(reader)
}

// validateTLVRecordLengths walks the (type, length, value) framing of a
// buffered TLV stream and rejects any record whose declared length exceeds
// the bytes remaining in the buffer. It deliberately does not interpret
// record contents; it only ensures the framing cannot drive an over-sized
// make() in the real decoder before any value bytes are read.
func validateTLVRecordLengths(buf []byte) error {
	var scratch [8]byte
	br := bytes.NewReader(buf)

	for br.Len() > 0 {
		// A clean end of stream is signaled by the type varint read
		// reaching EOF, which only happens when br.Len() == 0 above;
		// any read failure here is a malformed stream.
		if _, err := tlv.ReadVarInt(br, &scratch); err != nil {
			return fmt.Errorf("read tlv type: %w", err)
		}

		length, err := tlv.ReadVarInt(br, &scratch)
		if err != nil {
			return fmt.Errorf("read tlv length: %w", err)
		}

		// A record can never carry more value bytes than remain in the
		// buffer; reject before the decoder allocates make([]byte,
		// length).
		if length > uint64(br.Len()) {
			return fmt.Errorf("tlv record length %d exceeds %d "+
				"remaining bytes", length, br.Len())
		}

		if _, err := br.Seek(int64(length), io.SeekCurrent); err != nil {
			return fmt.Errorf("skip tlv value: %w", err)
		}
	}

	return nil
}

// checkElemCount rejects a decoded element count that cannot possibly be
// backed by the bytes physically present in buf, before any make([]T,
// count) allocation. minBytesPerElem is the smallest on-wire size a single
// element can occupy; a count claiming more elements than buf could hold is
// a corrupt or malicious framing and is rejected. This guards the
// hand-rolled length-prefixed list decoders that size their output slice
// from an attacker-controlled count varint.
func checkElemCount(bufLen int, count uint64, minBytesPerElem uint64) error {
	if minBytesPerElem == 0 {
		minBytesPerElem = 1
	}

	// max is the largest element count buf could physically back. Computed
	// in uint64 so the division cannot overflow.
	max := uint64(bufLen) / minBytesPerElem
	if count > max {
		return fmt.Errorf("element count %d exceeds %d backable by "+
			"%d bytes", count, max, bufLen)
	}

	return nil
}
