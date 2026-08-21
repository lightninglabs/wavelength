package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
)

const (
	// outgoingReplayVersion is the normalized recipient-proof encoding
	// written before an outgoing submit can be dispatched.
	outgoingReplayVersion byte = 1

	// maxOutgoingReplayPkScriptSize is Bitcoin's maximum script size. OOR
	// construction validates scripts before this proof is written.
	maxOutgoingReplayPkScriptSize = 10_000

	outgoingReplayHeaderSize = 5
	outgoingReplayEntrySize  = 16
)

// NewOutgoingReplayData binds the caller-requested recipients to their
// positions in the full canonical Ark output list. The full list may contain a
// wallet change output that was not part of the caller request.
func NewOutgoingReplayData(recipients, requested []oortx.RecipientOutput) (
	[]byte, error) {

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

	return encodeOutgoingReplayData(proof), nil
}

// newOutgoingReplayData serializes a canonical recipient list. It remains a
// convenient default for direct OOR callers whose full output list is also the
// caller-requested list.
func newOutgoingReplayData(recipients []oortx.RecipientOutput) ([]byte, error) {
	return NewOutgoingReplayData(recipients, recipients)
}

func encodeOutgoingReplayData(recipients []ArkRecipientOutput) []byte {
	if len(recipients) == 0 {
		return nil
	}

	size := outgoingReplayHeaderSize
	for i := range recipients {
		size += outgoingReplayEntrySize + len(recipients[i].PkScript)
	}

	raw := make([]byte, outgoingReplayHeaderSize, size)
	raw[0] = outgoingReplayVersion
	binary.BigEndian.PutUint32(raw[1:], uint32(len(recipients)))

	var scratch [8]byte
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
	if len(raw) < outgoingReplayHeaderSize {
		return nil, fmt.Errorf("outgoing replay proof is truncated")
	}
	if raw[0] != outgoingReplayVersion {
		return nil, fmt.Errorf("unknown outgoing replay proof "+
			"version: %d", raw[0])
	}

	count := binary.BigEndian.Uint32(raw[1:outgoingReplayHeaderSize])
	maxCount := (len(raw) - outgoingReplayHeaderSize) /
		(outgoingReplayEntrySize + 1)
	if count == 0 || uint64(count) > uint64(maxCount) {
		return nil, fmt.Errorf("invalid outgoing replay recipient "+
			"count: %d", count)
	}

	offset := outgoingReplayHeaderSize
	recipients := make([]ArkRecipientOutput, 0, int(count))
	for i := uint32(0); i < count; i++ {
		if len(raw)-offset < outgoingReplayEntrySize {
			return nil, fmt.Errorf("outgoing replay recipient %d "+
				"is truncated", i)
		}

		outputIndex := binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		if i > 0 && outputIndex <= recipients[i-1].OutputIndex {
			return nil, fmt.Errorf("outgoing replay recipient %d "+
				"has non-increasing output index %d", i,
				outputIndex)
		}

		value := binary.BigEndian.Uint64(raw[offset:])
		offset += 8
		if value == 0 || value > uint64(btcutil.MaxSatoshi) {
			return nil, fmt.Errorf("outgoing replay recipient %d "+
				"has invalid value %d", i, value)
		}

		scriptSize := binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		if scriptSize == 0 ||
			scriptSize > maxOutgoingReplayPkScriptSize {
			return nil, fmt.Errorf("outgoing replay recipient %d "+
				"has invalid pkScript size %d", i, scriptSize)
		}
		if uint32(len(raw)-offset) < scriptSize {
			return nil, fmt.Errorf("outgoing replay recipient %d "+
				"pkScript is truncated", i)
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
		return nil, fmt.Errorf("outgoing replay proof has %d "+
			"trailing bytes", len(raw)-offset)
	}

	return recipients, nil
}
