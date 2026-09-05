package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

const (
	// outgoingReplayVersionV1 is the legacy recipient-only proof.
	outgoingReplayVersionV1 byte = 1

	// outgoingReplayVersionV2 additionally binds the wallet input-selection
	// constraints that governed admission.
	outgoingReplayVersionV2 byte = 2

	// maxOutgoingReplayPkScriptSize is Bitcoin's maximum script size. OOR
	// construction validates scripts before this proof is written.
	maxOutgoingReplayPkScriptSize = 10_000

	outgoingReplayV1HeaderSize = 5
	outgoingReplayV2HeaderSize = 13
	outgoingReplayEntrySize    = 16
	outgoingReplayInputSize    = 36
)

// OutgoingReplayRequestData is the canonical request identity persisted
// before an outgoing OOR submit. Version-one records decode with a zero,
// unbounded age limit and no exact input constraint.
type OutgoingReplayRequestData struct {
	// Recipients are the caller-requested outputs bound to canonical Ark
	// transaction indices.
	Recipients []ArkRecipientOutput

	// MaxVTXOAgeBlocks is the wallet input-age policy used for admission.
	MaxVTXOAgeBlocks uint32

	// ExactInputOutpoints is the canonical managed input constraint.
	// An empty set leaves wallet selection unconstrained.
	ExactInputOutpoints []wire.OutPoint
}

// NewOutgoingReplayData binds the caller-requested recipients to their
// positions in the full canonical Ark output list. The full list may contain a
// wallet change output that was not part of the caller request.
func NewOutgoingReplayData(recipients, requested []oortx.RecipientOutput) (
	[]byte, error) {

	proof, err := outgoingReplayRecipientProof(recipients, requested)
	if err != nil {
		return nil, err
	}

	return encodeOutgoingReplayData(
		proof, outgoingReplayVersionV1, 0, nil,
	), nil
}

// NewOutgoingReplayDataWithMaxVTXOAgeBlocks binds both the caller-requested
// recipients and wallet input-selection constraints to a version-two replay
// record. Exact input order is not part of the identity because Ark inputs are
// canonically ordered during transaction construction.
func NewOutgoingReplayDataWithMaxVTXOAgeBlocks(
	recipients, requested []oortx.RecipientOutput, maxVTXOAgeBlocks uint32,
	exactInputOutpoints []wire.OutPoint) ([]byte, error) {

	proof, err := outgoingReplayRecipientProof(recipients, requested)
	if err != nil {
		return nil, err
	}
	exactInputs, err := canonicalOutgoingReplayInputs(exactInputOutpoints)
	if err != nil {
		return nil, err
	}

	return encodeOutgoingReplayData(
		proof, outgoingReplayVersionV2, maxVTXOAgeBlocks, exactInputs,
	), nil
}

// canonicalOutgoingReplayInputs returns a stable, duplicate-free copy of an
// exact managed input set. Ark transaction construction does not preserve the
// caller's input order, so that order is not part of the replay identity.
func canonicalOutgoingReplayInputs(inputs []wire.OutPoint) ([]wire.OutPoint,
	error) {

	canonical := append([]wire.OutPoint(nil), inputs...)
	sort.Slice(canonical, func(i, j int) bool {
		return outgoingReplayInputLess(canonical[i], canonical[j])
	})

	for i := 1; i < len(canonical); i++ {
		if canonical[i] == canonical[i-1] {
			return nil, fmt.Errorf("duplicate outgoing replay "+
				"input: %s", canonical[i])
		}
	}

	return canonical, nil
}

// outgoingReplayInputLess orders outpoints by their internal transaction hash
// bytes and then output index to produce one stable set representation.
func outgoingReplayInputLess(a, b wire.OutPoint) bool {
	cmp := bytes.Compare(a.Hash[:], b.Hash[:])
	if cmp != 0 {
		return cmp < 0
	}

	return a.Index < b.Index
}

// outgoingReplayRecipientProof binds requested recipients to their canonical
// positions in the full Ark output list.
func outgoingReplayRecipientProof(
	recipients, requested []oortx.RecipientOutput) ([]ArkRecipientOutput,
	error) {

	canonical := oortx.CanonicalRecipientOutputs(recipients)
	used := make([]bool, len(canonical))
	proof := make([]ArkRecipientOutput, 0, len(requested))
	for i := range requested {
		match := -1
		for j := range canonical {
			if used[j] ||
				canonical[j].Value != requested[i].Value ||
				!bytes.Equal(
					canonical[j].PkScript,
					requested[i].PkScript,
				) {

				continue
			}

			match = j

			break
		}
		if match == -1 {
			return nil, fmt.Errorf("requested OOR recipient %d is "+
				"not present in the canonical output list", i)
		}

		used[match] = true
		proof = append(proof, ArkRecipientOutput{
			OutputIndex: uint32(match),
			Value:       requested[i].Value,
			PkScript: append(
				[]byte(nil), requested[i].PkScript...,
			),
		})
	}

	sort.Slice(proof, func(i, j int) bool {
		return proof[i].OutputIndex < proof[j].OutputIndex
	})

	return proof, nil
}

// newOutgoingReplayData serializes a canonical recipient list. It remains a
// convenient default for direct OOR callers whose full output list is also the
// caller-requested list.
func newOutgoingReplayData(recipients []oortx.RecipientOutput) ([]byte, error) {
	return NewOutgoingReplayData(recipients, recipients)
}

// encodeOutgoingReplayData serializes one already-normalized replay proof.
func encodeOutgoingReplayData(recipients []ArkRecipientOutput, version byte,
	maxVTXOAgeBlocks uint32, exactInputs []wire.OutPoint) []byte {

	if len(recipients) == 0 {
		return nil
	}

	headerSize := outgoingReplayV1HeaderSize
	if version == outgoingReplayVersionV2 {
		headerSize = outgoingReplayV2HeaderSize
	}

	size := headerSize
	if version == outgoingReplayVersionV2 {
		size += outgoingReplayInputSize * len(exactInputs)
	}
	for i := range recipients {
		size += outgoingReplayEntrySize + len(recipients[i].PkScript)
	}

	raw := make([]byte, headerSize, size)
	raw[0] = version
	binary.BigEndian.PutUint32(raw[1:5], uint32(len(recipients)))
	if version == outgoingReplayVersionV2 {
		binary.BigEndian.PutUint32(raw[5:9], maxVTXOAgeBlocks)
		binary.BigEndian.PutUint32(raw[9:13], uint32(len(exactInputs)))
	}

	var scratch [8]byte
	if version == outgoingReplayVersionV2 {
		for i := range exactInputs {
			raw = append(raw, exactInputs[i].Hash[:]...)
			binary.BigEndian.PutUint32(
				scratch[:4], exactInputs[i].Index,
			)
			raw = append(raw, scratch[:4]...)
		}
	}

	for i := range recipients {
		binary.BigEndian.PutUint32(
			scratch[:4], recipients[i].OutputIndex,
		)
		raw = append(raw, scratch[:4]...)

		binary.BigEndian.PutUint64(
			scratch[:], uint64(recipients[i].Value),
		)
		raw = append(raw, scratch[:]...)

		binary.BigEndian.PutUint32(
			scratch[:4],
			uint32(
				len(recipients[i].PkScript),
			),
		)
		raw = append(raw, scratch[:4]...)
		raw = append(raw, recipients[i].PkScript...)
	}

	return raw
}

// OutgoingReplayRecipients decodes a normalized outgoing recipient proof. It
// fails closed on unknown versions, malformed positional data, invalid values,
// and trailing bytes so a replay never returns an unproven outpoint.
func OutgoingReplayRecipients(raw []byte) ([]ArkRecipientOutput, error) {
	request, err := DecodeOutgoingReplayData(raw)
	if err != nil {
		return nil, err
	}

	return request.Recipients, nil
}

// DecodeOutgoingReplayData decodes a normalized outgoing request proof. It
// accepts legacy recipient-only records as an unbounded age request and fails
// closed on unknown versions or malformed data.
func DecodeOutgoingReplayData(raw []byte) (OutgoingReplayRequestData, error) {
	if len(raw) < outgoingReplayV1HeaderSize {
		return OutgoingReplayRequestData{}, fmt.Errorf("outgoing " +
			"replay proof is truncated")
	}

	var (
		headerSize          int
		maxVTXOAgeBlocks    uint32
		exactInputCount     uint32
		exactInputOutpoints []wire.OutPoint
	)
	switch raw[0] {
	case outgoingReplayVersionV1:
		headerSize = outgoingReplayV1HeaderSize

	case outgoingReplayVersionV2:
		if len(raw) < outgoingReplayV2HeaderSize {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay proof is truncated")
		}
		headerSize = outgoingReplayV2HeaderSize
		maxVTXOAgeBlocks = binary.BigEndian.Uint32(raw[5:9])
		exactInputCount = binary.BigEndian.Uint32(raw[9:13])

	default:
		return OutgoingReplayRequestData{}, fmt.Errorf("unknown "+
			"outgoing replay proof version: %d", raw[0])
	}

	offset := headerSize
	maxInputCount := (len(raw) - offset) / outgoingReplayInputSize
	if uint64(exactInputCount) > uint64(maxInputCount) {
		return OutgoingReplayRequestData{}, fmt.Errorf("invalid "+
			"outgoing replay input count: %d", exactInputCount)
	}

	exactInputOutpoints = make(
		[]wire.OutPoint, 0, int(exactInputCount),
	)
	for i := uint32(0); i < exactInputCount; i++ {
		var input wire.OutPoint
		copy(input.Hash[:], raw[offset:offset+32])
		offset += 32
		input.Index = binary.BigEndian.Uint32(raw[offset:])
		offset += 4

		if i > 0 && !outgoingReplayInputLess(
			exactInputOutpoints[i-1], input,
		) {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay input %d is not in "+
					"canonical order", i)
		}

		exactInputOutpoints = append(exactInputOutpoints, input)
	}

	count := binary.BigEndian.Uint32(raw[1:5])
	maxCount := (len(raw) - offset) /
		(outgoingReplayEntrySize + 1)
	if count == 0 || uint64(count) > uint64(maxCount) {
		return OutgoingReplayRequestData{}, fmt.Errorf("invalid "+
			"outgoing replay recipient count: %d", count)
	}

	recipients := make([]ArkRecipientOutput, 0, int(count))
	for i := uint32(0); i < count; i++ {
		if len(raw)-offset < outgoingReplayEntrySize {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay recipient %d is truncated", i)
		}

		outputIndex := binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		if i > 0 && outputIndex <= recipients[i-1].OutputIndex {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay recipient %d has "+
					"non-increasing output index %d", i,
				outputIndex)
		}

		value := binary.BigEndian.Uint64(raw[offset:])
		offset += 8
		if value == 0 || value > uint64(btcutil.MaxSatoshi) {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay recipient %d has invalid "+
					"value %d", i, value)
		}

		scriptSize := binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		if scriptSize == 0 ||
			scriptSize > maxOutgoingReplayPkScriptSize {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay recipient %d has invalid "+
					"pkScript size %d", i, scriptSize)
		}
		if uint32(len(raw)-offset) < scriptSize {
			return OutgoingReplayRequestData{}, fmt.Errorf(
				"outgoing replay recipient %d pkScript is "+
					"truncated", i)
		}

		pkScript := append(
			[]byte(nil), raw[offset:offset+int(scriptSize)]...,
		)
		offset += int(scriptSize)
		recipients = append(recipients, ArkRecipientOutput{
			OutputIndex: outputIndex,
			Value:       btcutil.Amount(value),
			PkScript:    pkScript,
		})
	}

	if offset != len(raw) {
		return OutgoingReplayRequestData{}, fmt.Errorf("outgoing "+
			"replay proof has %d trailing bytes", len(raw)-offset)
	}

	return OutgoingReplayRequestData{
		Recipients:          recipients,
		MaxVTXOAgeBlocks:    maxVTXOAgeBlocks,
		ExactInputOutpoints: exactInputOutpoints,
	}, nil
}
