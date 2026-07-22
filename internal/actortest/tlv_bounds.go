package actortest

import (
	"bytes"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// maxCounterMessageSize bounds the wire size of an untrusted counter test
// message before any record length is honored. Counter messages are tiny
// (a scalar plus a small target/payload), so 1 MiB is generous while
// rejecting the unbounded-allocation DoS where a tiny payload declares a
// multi-gigabyte (or near-2^64) record length.
const maxCounterMessageSize = 1 << 20

// safeCounterTLVReader reads an untrusted counter TLV payload into a
// size-capped buffer and pre-validates its (type, length) framing so the
// downstream tlv.Stream.DecodeWithParsedTypes can never be handed a record
// length larger than the bytes physically present. The tlv library sizes
// its value buffers from the declared length before reading any value bytes
// and applies no length bound on the non-P2P Decode path, so a crafted
// payload would otherwise panic or OOM the decoder.
func safeCounterTLVReader(r io.Reader) (*bytes.Reader, error) {
	limited := io.LimitReader(r, maxCounterMessageSize+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read counter message: %w", err)
	}

	if len(buf) > maxCounterMessageSize {
		return nil, fmt.Errorf("counter message %d bytes exceeds max %d",
			len(buf), maxCounterMessageSize)
	}

	var scratch [8]byte
	br := bytes.NewReader(buf)
	for br.Len() > 0 {
		if _, err := tlv.ReadVarInt(br, &scratch); err != nil {
			return nil, fmt.Errorf("read tlv type: %w", err)
		}

		length, err := tlv.ReadVarInt(br, &scratch)
		if err != nil {
			return nil, fmt.Errorf("read tlv length: %w", err)
		}

		if length > uint64(br.Len()) {
			return nil, fmt.Errorf("tlv record length %d exceeds %d "+
				"remaining bytes", length, br.Len())
		}

		if _, err := br.Seek(int64(length), io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("skip tlv value: %w", err)
		}
	}

	return bytes.NewReader(buf), nil
}
