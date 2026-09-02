package unroll

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV record types for a single DeferredCheckpoint entry. Odd values
// per the Lightning convention: unknown odd records are optional and
// must be skipped by older decoders, so additive fields stay
// backward-compatible.
const (
	deferredCheckpointTxidRecordType     tlv.Type = 1
	deferredCheckpointDeadlineRecordType tlv.Type = 3
)

// encodeDeferredCheckpoints serializes deferred checkpoint state as a
// varint count followed by per-entry varint-length-prefixed TLV streams,
// each carrying the entry's fields as primitive TLV records. Per-entry
// TLV framing keeps the encoding forward-compatible: adding a new field
// to DeferredCheckpoint only requires a new odd TLV type; older
// decoders skip it.
func encodeDeferredCheckpoints(checkpoints []DeferredCheckpoint) ([]byte,
	error) {

	sorted := copyDeferredCheckpoints(checkpoints)

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	err := tlv.WriteVarInt(&buf, uint64(len(sorted)), &scratch)
	if err != nil {
		return nil, fmt.Errorf("write deferred checkpoint count: %w",
			err)
	}

	for i := range sorted {
		entryBytes, err := encodeDeferredCheckpointEntry(&sorted[i])
		if err != nil {
			return nil, fmt.Errorf("encode deferred checkpoint "+
				"%d: %w", i, err)
		}

		err = tlv.WriteVarInt(
			&buf,
			uint64(
				len(entryBytes),
			),
			&scratch,
		)
		if err != nil {
			return nil, fmt.Errorf("write deferred checkpoint %d "+
				"length: %w", i, err)
		}

		if _, err := buf.Write(entryBytes); err != nil {
			return nil, fmt.Errorf("write deferred checkpoint %d "+
				"body: %w", i, err)
		}
	}

	return buf.Bytes(), nil
}

// encodeDeferredCheckpointEntry encodes one DeferredCheckpoint as a TLV
// stream of primitive records.
func encodeDeferredCheckpointEntry(c *DeferredCheckpoint) ([]byte, error) {
	txidBytes := c.Txid[:]
	deadline := uint32(c.DeadlineHeight)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			deferredCheckpointTxidRecordType, &txidBytes,
		),
		tlv.MakePrimitiveRecord(
			deferredCheckpointDeadlineRecordType, &deadline,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("new entry stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode entry stream: %w", err)
	}

	return buf.Bytes(), nil
}

// decodeDeferredCheckpoints parses deferred checkpoint state encoded by
// encodeDeferredCheckpoints.
func decodeDeferredCheckpoints(raw []byte) ([]DeferredCheckpoint, error) {
	var (
		reader  = bytes.NewReader(raw)
		scratch [8]byte
	)

	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, fmt.Errorf("read deferred checkpoint count: %w",
			err)
	}

	// Bound the declared count against the bytes physically present
	// before allocating. Each entry is at least a one-byte length prefix,
	// so a count larger than the remaining bytes is a lie that would
	// otherwise drive an unbounded (or panicking) make() from a tampered
	// or truncated durable blob.
	if count > uint64(reader.Len()) {
		return nil, fmt.Errorf("deferred checkpoint count %d exceeds "+
			"%d remaining bytes", count, reader.Len())
	}

	checkpoints := make([]DeferredCheckpoint, 0, count)
	for i := uint64(0); i < count; i++ {
		entryLen, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, fmt.Errorf("read deferred checkpoint %d "+
				"length: %w", i, err)
		}

		// Bound each entry length against the bytes still available so
		// a huge declared length cannot pre-allocate before we discover
		// the truncation.
		if entryLen > uint64(reader.Len()) {
			return nil, fmt.Errorf("deferred checkpoint %d length "+
				"%d exceeds %d remaining bytes", i, entryLen,
				reader.Len())
		}

		// Use io.ReadFull rather than reader.Read: a bare Read can
		// return a short count without error, which would silently
		// decode a truncated entry into a wrong-but-valid value
		// (zero-padded tail) instead of failing closed.
		entry := make([]byte, entryLen)
		if _, err := io.ReadFull(reader, entry); err != nil {
			return nil, fmt.Errorf("read deferred checkpoint %d "+
				"body: %w", i, err)
		}

		decoded, err := decodeDeferredCheckpointEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("decode deferred checkpoint "+
				"%d: %w", i, err)
		}

		checkpoints = append(checkpoints, decoded)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("deferred checkpoints trailing %d bytes",
			reader.Len())
	}

	return copyDeferredCheckpoints(checkpoints), nil
}

// decodeDeferredCheckpointEntry decodes one DeferredCheckpoint from its
// per-entry TLV stream. Unknown odd TLV types are skipped silently for
// forward-compatibility.
func decodeDeferredCheckpointEntry(raw []byte) (DeferredCheckpoint, error) {
	var (
		txidBytes []byte
		deadline  uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			deferredCheckpointTxidRecordType, &txidBytes,
		),
		tlv.MakePrimitiveRecord(
			deferredCheckpointDeadlineRecordType, &deadline,
		),
	)
	if err != nil {
		return DeferredCheckpoint{}, fmt.Errorf("new entry stream: %w",
			err)
	}

	// Validate the inner entry framing before decoding. The txid record
	// decodes through tlv DVarBytes, whose non-P2P path sizes
	// make([]byte, length) directly from the declared length; without this
	// guard a per-entry blob declaring a huge txid length panics or OOMs.
	safeReader, err := safeTLVReader(raw)
	if err != nil {
		return DeferredCheckpoint{}, fmt.Errorf("decode entry "+
			"stream: %w", err)
	}

	if err := stream.Decode(safeReader); err != nil {
		return DeferredCheckpoint{}, fmt.Errorf("decode entry "+
			"stream: %w", err)
	}

	if len(txidBytes) != chainhash.HashSize {
		return DeferredCheckpoint{}, fmt.Errorf("deferred checkpoint "+
			"txid has %d bytes, expected %d", len(txidBytes),
			chainhash.HashSize)
	}

	var txid chainhash.Hash
	copy(txid[:], txidBytes)

	return DeferredCheckpoint{
		Txid:           txid,
		DeadlineHeight: int32(deadline),
	}, nil
}
