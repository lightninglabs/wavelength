package actor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// maxActorMessageSize bounds the total wire size of an untrusted actor TLV
// message (the codec envelope and every message payload decoded from a
// durable mailbox) before any individual record length is honored. Actor
// payloads can embed domain snapshots and PSBT-bearing blobs, so the cap is
// generous at 16 MiB while still rejecting the unbounded-allocation DoS
// where a tiny payload declares a multi-gigabyte (or near-2^64) length.
// Durable-mailbox bytes are replayed from disk across rolling upgrades and
// can cross the cross-actor trust boundary, so the bound is load-bearing on
// both the live and replay paths.
const maxActorMessageSize = 16 << 20

// validateTLVRecordLengths walks the (type, length, value) framing of a
// buffered TLV stream and rejects any record whose declared length exceeds
// the bytes remaining in the buffer. It deliberately does not interpret
// record contents; it only ensures the framing cannot drive an over-sized
// make([]byte, declaredLength) in the real tlv decoder before any value
// bytes are read.
//
// The tlv library sizes its value buffers from the declared length before
// reading any value bytes (stream.go's unknown-record path and
// primitive.go's DVarBytes), and the non-P2P Decode path applies no length
// bound. A producer-declared length near 2^64 panics the decoder with
// "makeslice: cap out of range" and a multi-gigabyte length drives an OOM.
func validateTLVRecordLengths(buf []byte) error {
	var scratch [8]byte
	br := bytes.NewReader(buf)

	for br.Len() > 0 {
		if _, err := tlv.ReadVarInt(br, &scratch); err != nil {
			return fmt.Errorf("read tlv type: %w", err)
		}

		length, err := tlv.ReadVarInt(br, &scratch)
		if err != nil {
			return fmt.Errorf("read tlv length: %w", err)
		}

		// A record can never carry more value bytes than remain in the
		// buffer; reject before the decoder allocates.
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

// safeActorTLVReader reads an untrusted actor TLV payload into a size-capped
// buffer and pre-validates its framing so the downstream
// tlv.Stream.DecodeWithParsedTypes can never be handed a record length
// larger than the bytes physically present. See validateTLVRecordLengths
// for why this is required.
func safeActorTLVReader(r io.Reader) (*bytes.Reader, error) {
	// Read at most maxActorMessageSize+1 so an over-cap message is detected
	// without buffering an unbounded amount.
	limited := io.LimitReader(r, maxActorMessageSize+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read actor message: %w", err)
	}

	if len(buf) > maxActorMessageSize {
		return nil, fmt.Errorf("actor message %d bytes exceeds max %d",
			len(buf), maxActorMessageSize)
	}

	if err := validateTLVRecordLengths(buf); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf), nil
}
