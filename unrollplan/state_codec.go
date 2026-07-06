package unrollplan

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// StateCodecVersion is the on-disk version byte for the unrollplan state
// codec. It lives in its own constant rather than being shared with the
// recovery-side codec because the two states are independent on the wire.
const StateCodecVersion uint8 = 1

const (
	// stateVersionRecordType carries the codec version byte.
	stateVersionRecordType tlv.Type = 1

	// stateConfirmedTxidsRecordType carries the confirmed-txid list.
	stateConfirmedTxidsRecordType tlv.Type = 3

	// stateInFlightTxidsRecordType carries the in-flight txid list.
	stateInFlightTxidsRecordType tlv.Type = 5

	// stateTargetConfirmHeightRecordType is optional; present only when
	// TargetConfirmHeight is non-nil.
	stateTargetConfirmHeightRecordType tlv.Type = 7

	// stateSweepRecordType carries the nested sweep encoding.
	stateSweepRecordType tlv.Type = 9
)

const (
	// sweepStatusRecordType carries the SweepStatus byte.
	sweepStatusRecordType tlv.Type = 1

	// sweepTxidRecordType is optional; present only when Txid is
	// non-nil.
	sweepTxidRecordType tlv.Type = 3

	// sweepConfirmHeightRecordType is optional; present only when
	// ConfirmHeight is non-nil.
	sweepConfirmHeightRecordType tlv.Type = 5
)

// EncodeState serializes a State to a TLV byte slice. The returned bytes are
// deterministic for a given logical state: txid lists are sorted in ascending
// byte order and duplicate entries are rejected eagerly.
func EncodeState(state *State) ([]byte, error) {
	if state == nil {
		return nil, fmt.Errorf("state cannot be nil")
	}

	version := StateCodecVersion
	confirmed, err := encodeHashList(state.ConfirmedTxids, "confirmed")
	if err != nil {
		return nil, err
	}
	inflight, err := encodeHashList(state.InFlightTxids, "in-flight")
	if err != nil {
		return nil, err
	}
	sweep, err := encodeSweepState(state.Sweep)
	if err != nil {
		return nil, fmt.Errorf("encode sweep: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(stateVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			stateConfirmedTxidsRecordType, &confirmed,
		),
		tlv.MakePrimitiveRecord(
			stateInFlightTxidsRecordType, &inflight,
		),
	}

	state.TargetConfirmHeight.WhenSome(func(h int32) {
		height := uint32(h)
		records = append(
			records, tlv.MakePrimitiveRecord(
				stateTargetConfirmHeightRecordType, &height,
			),
		)
	})

	records = append(
		records, tlv.MakePrimitiveRecord(
			stateSweepRecordType, &sweep,
		),
	)

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create state stream: %w", err)
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}

	return buf.Bytes(), nil
}

// DecodeState parses a TLV-encoded State and rejects unknown codec versions.
func DecodeState(raw []byte) (*State, error) {
	var (
		version             uint8
		confirmedRaw        []byte
		inflightRaw         []byte
		targetConfirmHeight uint32
		sweepRaw            []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(stateVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			stateConfirmedTxidsRecordType, &confirmedRaw,
		),
		tlv.MakePrimitiveRecord(
			stateInFlightTxidsRecordType, &inflightRaw,
		),
		tlv.MakePrimitiveRecord(
			stateTargetConfirmHeightRecordType,
			&targetConfirmHeight,
		),
		tlv.MakePrimitiveRecord(stateSweepRecordType, &sweepRaw),
	)
	if err != nil {
		return nil, fmt.Errorf("create state stream: %w", err)
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}

	if _, ok := parsed[stateVersionRecordType]; !ok {
		return nil, fmt.Errorf("state missing version record")
	}
	if version != StateCodecVersion {
		return nil, fmt.Errorf("unsupported state codec version %d "+
			"(expected %d)", version, StateCodecVersion)
	}

	state := &State{}

	state.ConfirmedTxids, err = decodeHashList(confirmedRaw, "confirmed")
	if err != nil {
		return nil, err
	}

	state.InFlightTxids, err = decodeHashList(inflightRaw, "in-flight")
	if err != nil {
		return nil, err
	}

	if _, ok := parsed[stateTargetConfirmHeightRecordType]; ok {
		state.TargetConfirmHeight = fn.Some(int32(targetConfirmHeight))
	}

	if _, ok := parsed[stateSweepRecordType]; ok {
		sweep, err := decodeSweepState(sweepRaw)
		if err != nil {
			return nil, fmt.Errorf("decode sweep: %w", err)
		}
		state.Sweep = sweep
	}

	return state, nil
}

// encodeHashList serializes a slice of chainhash.Hash as a 4-byte big-endian
// count followed by sorted raw 32-byte hashes. Duplicate entries are rejected
// at encode time so the persisted file is never self-contradictory.
func encodeHashList(hashes []chainhash.Hash, label string) ([]byte, error) {
	seen := make(map[chainhash.Hash]struct{}, len(hashes))
	for _, h := range hashes {
		if _, ok := seen[h]; ok {
			return nil, fmt.Errorf("duplicate %s txid %s", label, h)
		}
		seen[h] = struct{}{}
	}

	sorted := append([]chainhash.Hash(nil), hashes...)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	var buf bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(sorted)))
	if _, err := buf.Write(lenBuf[:]); err != nil {
		return nil, err
	}

	for _, h := range sorted {
		if _, err := buf.Write(h[:]); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeHashList reverses encodeHashList and rejects duplicates again — a
// malformed or tampered blob could otherwise reintroduce the very collision
// the encoder guards against.
func decodeHashList(raw []byte, label string) ([]chainhash.Hash, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("truncated %s txid list", label)
	}

	count := binary.BigEndian.Uint32(raw[:4])
	raw = raw[4:]

	if uint64(len(raw)) != uint64(count)*chainhash.HashSize {
		return nil, fmt.Errorf("%s txid list length mismatch: "+
			"count=%d payload=%d", label, count, len(raw))
	}

	out := make([]chainhash.Hash, 0, count)
	seen := make(map[chainhash.Hash]struct{}, count)
	for i := uint32(0); i < count; i++ {
		var h chainhash.Hash
		copy(h[:], raw[:chainhash.HashSize])
		raw = raw[chainhash.HashSize:]

		if _, ok := seen[h]; ok {
			return nil, fmt.Errorf("duplicate %s txid %s", label, h)
		}
		seen[h] = struct{}{}

		out = append(out, h)
	}

	return out, nil
}

// encodeSweepState serializes a SweepState as a TLV sub-stream. The sweep is
// nested rather than flattened into the outer state stream so a future codec
// change to the sweep shape does not require re-numbering the outer record
// types.
func encodeSweepState(sweep SweepState) ([]byte, error) {
	status := uint8(sweep.Status)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(sweepStatusRecordType, &status),
	}

	sweep.Txid.WhenSome(func(hash chainhash.Hash) {
		txid := hash[:]
		records = append(
			records, tlv.MakePrimitiveRecord(
				sweepTxidRecordType, &txid,
			),
		)
	})

	sweep.ConfirmHeight.WhenSome(func(h int32) {
		height := uint32(h)
		records = append(
			records, tlv.MakePrimitiveRecord(
				sweepConfirmHeightRecordType, &height,
			),
		)
	})

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeSweepState reverses encodeSweepState.
func decodeSweepState(raw []byte) (SweepState, error) {
	var (
		statusByte    uint8
		txid          []byte
		confirmHeight uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(sweepStatusRecordType, &statusByte),
		tlv.MakePrimitiveRecord(sweepTxidRecordType, &txid),
		tlv.MakePrimitiveRecord(
			sweepConfirmHeightRecordType, &confirmHeight,
		),
	)
	if err != nil {
		return SweepState{}, err
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(raw))
	if err != nil {
		return SweepState{}, err
	}

	sweep := SweepState{Status: SweepStatus(statusByte)}

	if _, ok := parsed[sweepTxidRecordType]; ok {
		if len(txid) != chainhash.HashSize {
			return SweepState{}, fmt.Errorf("sweep txid length "+
				"%d invalid", len(txid))
		}
		var hash chainhash.Hash
		copy(hash[:], txid)
		sweep.Txid = fn.Some(hash)
	}

	if _, ok := parsed[sweepConfirmHeightRecordType]; ok {
		sweep.ConfirmHeight = fn.Some(int32(confirmHeight))
	}

	return sweep, nil
}
