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
	// written before an outgoing Bitcoin OOR submit can be dispatched.
	outgoingReplayVersion byte = 1

	// outgoingAssetReplayVersion is the normalized request-identity and
	// summary proof encoding written before an outgoing asset OOR submit
	// can be dispatched. Using a distinct version byte isolates Bitcoin
	// and asset idempotency namespaces cleanly.
	outgoingAssetReplayVersion byte = 2

	// maxOutgoingReplayPkScriptSize is Bitcoin's maximum script size. OOR
	// construction validates scripts before this proof is written.
	maxOutgoingReplayPkScriptSize = 10_000

	// maxOutgoingAssetRefSize bounds canonical asset references encoded in
	// outgoing asset replay proofs.
	maxOutgoingAssetRefSize = 256

	outgoingReplayHeaderSize      = 5
	outgoingReplayEntrySize       = 16
	outgoingAssetReplayHeaderSize = 33
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

				//nolint:gosec // G602: both are range indexes.
				canonical[j].Value != requested[i].Value ||

				!bytes.Equal(
					canonical[j].PkScript,
					//nolint:gosec // G602: i is ranged.
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
			//nolint:gosec // G602: i is ranged.
			Value: requested[i].Value,
			PkScript: append(
				[]byte(nil),
				//nolint:gosec // G602: i is ranged.
				requested[i].PkScript...,
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

// OutgoingAssetReplay binds an asset OOR idempotency attempt to its canonical
// request parameters and response summary so replays verify identity and return
// the original figures instead of newly built or blank values.
type OutgoingAssetReplay struct {
	OutputIndex        uint32
	SelectedInputCount uint32
	AssetRef           string
	AssetAmount        uint64
	AssetChangeAmount  uint64
	SenderBtcChangeSat int64
	RecipientPolicy    []byte
}

// IsOutgoingAssetReplayData reports whether raw is a v2 asset OOR replay proof.
func IsOutgoingAssetReplayData(raw []byte) bool {
	return len(raw) > 0 && raw[0] == outgoingAssetReplayVersion
}

// NewOutgoingAssetReplayData validates and serializes an asset OOR replay
// proof.
func NewOutgoingAssetReplayData(r OutgoingAssetReplay) ([]byte, error) {
	switch {
	case r.AssetRef == "" || len(r.AssetRef) > maxOutgoingAssetRefSize:
		return nil, fmt.Errorf("invalid asset replay ref length %d",
			len(r.AssetRef))

	case r.AssetAmount == 0:
		return nil, fmt.Errorf("asset replay amount must be positive")

	case r.SenderBtcChangeSat < 0 ||
		r.SenderBtcChangeSat > btcutil.MaxSatoshi:
		return nil, fmt.Errorf("invalid asset replay sender BTC "+
			"change %d", r.SenderBtcChangeSat)

	case len(r.RecipientPolicy) == 0 ||
		len(r.RecipientPolicy) > maxOutgoingReplayPkScriptSize:
		return nil, fmt.Errorf("invalid asset replay recipient policy "+
			"length %d", len(r.RecipientPolicy))
	}

	totalLen := outgoingAssetReplayHeaderSize + 4 + len(r.AssetRef) +
		4 + len(r.RecipientPolicy)
	raw := make([]byte, 0, totalLen)
	raw = append(raw, outgoingAssetReplayVersion)

	var scratch [8]byte
	binary.BigEndian.PutUint32(scratch[:4], r.OutputIndex)
	raw = append(raw, scratch[:4]...)

	binary.BigEndian.PutUint32(scratch[:4], r.SelectedInputCount)
	raw = append(raw, scratch[:4]...)

	binary.BigEndian.PutUint64(scratch[:], r.AssetAmount)
	raw = append(raw, scratch[:]...)

	binary.BigEndian.PutUint64(scratch[:], r.AssetChangeAmount)
	raw = append(raw, scratch[:]...)

	binary.BigEndian.PutUint64(scratch[:], uint64(r.SenderBtcChangeSat))
	raw = append(raw, scratch[:]...)

	binary.BigEndian.PutUint32(scratch[:4], uint32(len(r.AssetRef)))
	raw = append(raw, scratch[:4]...)
	raw = append(raw, r.AssetRef...)

	binary.BigEndian.PutUint32(scratch[:4], uint32(len(r.RecipientPolicy)))
	raw = append(raw, scratch[:4]...)
	raw = append(raw, r.RecipientPolicy...)

	return raw, nil
}

// DecodeOutgoingAssetReplayData decodes a v2 asset OOR replay proof.
func DecodeOutgoingAssetReplayData(raw []byte) (OutgoingAssetReplay, error) {
	var out OutgoingAssetReplay
	if len(raw) < outgoingAssetReplayHeaderSize+8 {
		return out, fmt.Errorf("asset outgoing replay proof is " +
			"truncated")
	}
	if raw[0] != outgoingAssetReplayVersion {
		return out, fmt.Errorf("unknown asset outgoing replay proof "+
			"version: %d", raw[0])
	}

	offset := 1
	out.OutputIndex = binary.BigEndian.Uint32(raw[offset:])
	offset += 4

	out.SelectedInputCount = binary.BigEndian.Uint32(raw[offset:])
	offset += 4

	out.AssetAmount = binary.BigEndian.Uint64(raw[offset:])
	offset += 8
	if out.AssetAmount == 0 {
		return OutgoingAssetReplay{}, fmt.Errorf("asset replay " +
			"amount must be positive")
	}

	out.AssetChangeAmount = binary.BigEndian.Uint64(raw[offset:])
	offset += 8

	senderChange := binary.BigEndian.Uint64(raw[offset:])
	offset += 8
	if senderChange > uint64(btcutil.MaxSatoshi) {
		return OutgoingAssetReplay{}, fmt.Errorf("invalid asset "+
			"replay sender BTC change %d", senderChange)
	}
	out.SenderBtcChangeSat = int64(senderChange)

	if len(raw)-offset < 4 {
		return OutgoingAssetReplay{}, fmt.Errorf("asset replay ref " +
			"length is truncated")
	}
	refLen := binary.BigEndian.Uint32(raw[offset:])
	offset += 4
	if refLen == 0 || refLen > maxOutgoingAssetRefSize ||
		uint32(len(raw)-offset) < refLen {
		return OutgoingAssetReplay{}, fmt.Errorf("invalid asset "+
			"replay ref length %d", refLen)
	}
	out.AssetRef = string(raw[offset : offset+int(refLen)])
	offset += int(refLen)

	if len(raw)-offset < 4 {
		return OutgoingAssetReplay{}, fmt.Errorf("asset replay " +
			"policy length is truncated")
	}
	policyLen := binary.BigEndian.Uint32(raw[offset:])
	offset += 4
	if policyLen == 0 || policyLen > maxOutgoingReplayPkScriptSize ||
		uint32(len(raw)-offset) < policyLen {
		return OutgoingAssetReplay{}, fmt.Errorf("invalid asset "+
			"replay policy length %d", policyLen)
	}
	out.RecipientPolicy = append(
		[]byte(nil), raw[offset:offset+int(policyLen)]...,
	)
	offset += int(policyLen)

	if offset != len(raw) {
		return OutgoingAssetReplay{}, fmt.Errorf("asset outgoing "+
			"replay proof has %d trailing bytes", len(raw)-offset)
	}

	return out, nil
}
