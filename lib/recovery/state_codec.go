package recovery

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// SessionStateCodecVersion is the on-disk version byte written by Encode and
// accepted by Decode. Bumping this value lets us migrate the wire format in a
// later release while still rejecting unknown versions clearly.
const SessionStateCodecVersion uint8 = 1

const (
	// sessionStateVersionRecordType carries the single-byte codec
	// version. It MUST come first so decoders that want to fast-fail on
	// version mismatch can do so without parsing further records.
	sessionStateVersionRecordType tlv.Type = 1

	// sessionStateTxStatesRecordType carries the (txid, TxState) list.
	sessionStateTxStatesRecordType tlv.Type = 3

	// sessionStateConfirmHeightsRecordType carries the
	// (txid, confirmHeight) list.
	sessionStateConfirmHeightsRecordType tlv.Type = 5

	// sessionStateFailedTxidRecordType is the optional failed-txid record.
	// Absent when the session has no terminal failure.
	sessionStateFailedTxidRecordType tlv.Type = 7

	// sessionStateLastErrorRecordType is the optional terminal error
	// string. Absent when the session has no terminal failure.
	sessionStateLastErrorRecordType tlv.Type = 9
)

// EncodeSessionState serializes a SessionState into a length-prefix-free TLV
// byte slice. The returned bytes can be concatenated with other TLV records
// by the caller if needed; for standalone persistence, the caller should
// length-prefix the blob at the outer layer.
func EncodeSessionState(state *SessionState) ([]byte, error) {
	if state == nil {
		return nil, fmt.Errorf("session state cannot be nil")
	}

	version := SessionStateCodecVersion
	txStates, err := encodeTxStateMap(state.TxStates)
	if err != nil {
		return nil, fmt.Errorf("encode tx states: %w", err)
	}
	confirmHeights, err := encodeConfirmHeightMap(state.ConfirmHeights)
	if err != nil {
		return nil, fmt.Errorf("encode confirm heights: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			sessionStateVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			sessionStateTxStatesRecordType, &txStates,
		),
		tlv.MakePrimitiveRecord(
			sessionStateConfirmHeightsRecordType, &confirmHeights,
		),
	}

	state.FailedTxid.WhenSome(func(hash chainhash.Hash) {
		failedTxid := hash[:]
		records = append(
			records, tlv.MakePrimitiveRecord(
				sessionStateFailedTxidRecordType, &failedTxid,
			),
		)
	})

	if state.LastError != "" {
		lastError := []byte(state.LastError)
		records = append(
			records, tlv.MakePrimitiveRecord(
				sessionStateLastErrorRecordType, &lastError,
			),
		)
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create session state stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode session state: %w", err)
	}

	return buf.Bytes(), nil
}

// DecodeSessionState parses a TLV-encoded SessionState. Only the current
// codec version is accepted; forward/backward compatibility must go through
// an explicit version bump plus migration.
func DecodeSessionState(raw []byte) (*SessionState, error) {
	var (
		version        uint8
		txStatesRaw    []byte
		heightsRaw     []byte
		failedTxid     []byte
		lastErrorBytes []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			sessionStateVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			sessionStateTxStatesRecordType, &txStatesRaw,
		),
		tlv.MakePrimitiveRecord(
			sessionStateConfirmHeightsRecordType, &heightsRaw,
		),
		tlv.MakePrimitiveRecord(
			sessionStateFailedTxidRecordType, &failedTxid,
		),
		tlv.MakePrimitiveRecord(
			sessionStateLastErrorRecordType, &lastErrorBytes,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create session state stream: %w", err)
	}

	// Pre-validate the framing so a record declaring a length larger than
	// the bytes present cannot drive an unbounded make() inside the tlv
	// decoder (the DVarBytes-backed txStates, confirmHeights, failedTxid,
	// and lastError records, plus the unknown-record discard buffer).
	// Session state is read back from durable storage on restart.
	reader, err := safeRecoveryTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode session state: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode session state: %w", err)
	}

	if _, ok := parsed[sessionStateVersionRecordType]; !ok {
		return nil, fmt.Errorf("session state missing version record")
	}
	if version != SessionStateCodecVersion {
		return nil, fmt.Errorf("unsupported session state codec "+
			"version %d (expected %d)", version,
			SessionStateCodecVersion)
	}

	state := &SessionState{}

	state.TxStates, err = decodeTxStateMap(txStatesRaw)
	if err != nil {
		return nil, fmt.Errorf("decode tx states: %w", err)
	}

	state.ConfirmHeights, err = decodeConfirmHeightMap(heightsRaw)
	if err != nil {
		return nil, fmt.Errorf("decode confirm heights: %w", err)
	}

	if _, ok := parsed[sessionStateFailedTxidRecordType]; ok {
		if len(failedTxid) != chainhash.HashSize {
			return nil, fmt.Errorf("failed txid length %d invalid",
				len(failedTxid))
		}

		var hash chainhash.Hash
		copy(hash[:], failedTxid)
		state.FailedTxid = fn.Some(hash)
	}

	if _, ok := parsed[sessionStateLastErrorRecordType]; ok {
		state.LastError = string(lastErrorBytes)
	}

	return state, nil
}

// encodeTxStateMap serializes the tx-state map as a length-prefixed list of
// (hash || uint8 state) entries. Entries are emitted in ascending hash byte
// order so the encoded form is deterministic for a given logical state.
func encodeTxStateMap(states map[chainhash.Hash]TxState) ([]byte, error) {
	keys := sortedHashKeys(states)

	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(keys)))
	if _, err := buf.Write(lenBuf[:]); err != nil {
		return nil, err
	}

	for _, key := range keys {
		state := states[key]
		if state < 0 || state > 255 {
			return nil, fmt.Errorf("tx state %d out of range",
				state)
		}

		if _, err := buf.Write(key[:]); err != nil {
			return nil, err
		}
		if err := buf.WriteByte(byte(state)); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeTxStateMap reverses encodeTxStateMap and rejects duplicate keys. A
// duplicate key would make the resulting Go map non-deterministic because
// last-write-wins, so we fail loudly instead.
func decodeTxStateMap(raw []byte) (map[chainhash.Hash]TxState, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("truncated tx state map")
	}

	count := binary.BigEndian.Uint32(raw[:4])
	raw = raw[4:]

	const entrySize = chainhash.HashSize + 1
	if uint64(len(raw)) != uint64(count)*uint64(entrySize) {
		return nil, fmt.Errorf("tx state map length mismatch: "+
			"count=%d payload=%d", count, len(raw))
	}

	out := make(map[chainhash.Hash]TxState, count)
	for i := uint32(0); i < count; i++ {
		var hash chainhash.Hash
		copy(hash[:], raw[:chainhash.HashSize])
		state := TxState(raw[chainhash.HashSize])
		raw = raw[entrySize:]

		if _, exists := out[hash]; exists {
			return nil, fmt.Errorf("duplicate tx state key %s",
				hash)
		}

		out[hash] = state
	}

	return out, nil
}

// encodeConfirmHeightMap serializes the confirm-height map as a
// length-prefixed list of (hash || big-endian int32) entries in sorted hash
// order.
func encodeConfirmHeightMap(heights map[chainhash.Hash]int32) ([]byte, error) {
	keys := sortedHashKeys(heights)

	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(keys)))
	if _, err := buf.Write(lenBuf[:]); err != nil {
		return nil, err
	}

	for _, key := range keys {
		if _, err := buf.Write(key[:]); err != nil {
			return nil, err
		}
		var heightBuf [4]byte
		binary.BigEndian.PutUint32(
			heightBuf[:], uint32(heights[key]),
		)
		if _, err := buf.Write(heightBuf[:]); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeConfirmHeightMap reverses encodeConfirmHeightMap and rejects
// duplicate keys for the same reason as decodeTxStateMap.
func decodeConfirmHeightMap(raw []byte) (map[chainhash.Hash]int32, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("truncated confirm height map")
	}

	count := binary.BigEndian.Uint32(raw[:4])
	raw = raw[4:]

	const entrySize = chainhash.HashSize + 4
	if uint64(len(raw)) != uint64(count)*uint64(entrySize) {
		return nil, fmt.Errorf("confirm height map length mismatch: "+
			"count=%d payload=%d", count, len(raw))
	}

	out := make(map[chainhash.Hash]int32, count)
	for i := uint32(0); i < count; i++ {
		var hash chainhash.Hash
		copy(hash[:], raw[:chainhash.HashSize])
		height := int32(
			binary.BigEndian.Uint32(raw[chainhash.HashSize:]),
		)
		raw = raw[entrySize:]

		if _, exists := out[hash]; exists {
			return nil, fmt.Errorf("duplicate confirm "+
				"height key %s", hash)
		}

		out[hash] = height
	}

	return out, nil
}

// sortedHashKeys returns the keys of a map keyed by chainhash.Hash sorted in
// ascending byte order. Centralized so encoders do not need to re-implement
// the same sort and so the determinism invariant holds across map types.
func sortedHashKeys[V any](m map[chainhash.Hash]V) []chainhash.Hash {
	keys := make([]chainhash.Hash, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	return keys
}

// assertWriter is a compile-time check that *bytes.Buffer implements
// io.Writer. Kept as an anchor so future refactors that replace the buffer
// type still satisfy the TLV Writer contract.
var _ io.Writer = (*bytes.Buffer)(nil)
