package arkscript

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
)

// PSBTKeyPrefix is the namespace prefix for Ark PSBT keys.
const PSBTKeyPrefix = "ark/"

// PSBTKeyTapTree is the PSBT key for tap tree encoding.
const PSBTKeyTapTree = PSBTKeyPrefix + "taptree"

// PSBTKeyConditionWitness is the PSBT key for hashlock preimage witnesses.
const PSBTKeyConditionWitness = PSBTKeyPrefix + "condition"

var (
	// ErrConditionWitnessNotFound indicates that a PSBT input does not
	// include Ark condition witness metadata.
	ErrConditionWitnessNotFound = errors.New(
		"condition witness not found",
	)
)

// EncodedLeaf represents a single leaf in the PSBT tap tree encoding.
type EncodedLeaf struct {
	// Depth is the depth of this leaf in the tree (root is depth 0).
	Depth uint8

	// LeafVersion is the BIP-341 leaf version (typically 0xc0).
	LeafVersion uint8

	// Script is the tapscript leaf script bytes.
	Script []byte
}

// maxTapTreeLeaves is the sanity upper bound for leaf counts in the
// Ark-specific tap tree encoding. Ark policies use at most ~10 leaves.
const maxTapTreeLeaves = 256

// maxPreimageLen is the maximum preimage length accepted by both
// encode and decode for condition witnesses.
const maxPreimageLen = 520

// EncodeTapTree serializes a compiled policy's leaves into an
// Ark-specific tap tree encoding. This is NOT the BIP-371
// PSBT_OUT_TAP_TREE format (which uses depth-first tuples without a
// leading count). The format is:
// - leaf count (compact size uint)
// - for each leaf:
//   - depth (1 byte)
//   - leaf version (1 byte)
//   - script length (compact size uint)
//   - script bytes
//
// Leaves are encoded in canonical order.
func EncodeTapTree(policy *CompiledPolicy) ([]byte, error) {
	if policy == nil || len(policy.Leaves) == 0 {
		return nil, fmt.Errorf("psbt: cannot encode empty policy")
	}

	// Calculate depths for each leaf from the proof lengths.
	depths := make([]uint8, len(policy.Leaves))
	for i := range policy.Leaves {
		depths[i] = uint8(len(policy.merkleProofs[i]))
	}

	var buf bytes.Buffer

	// Write leaf count.
	err := wire.WriteVarInt(&buf, 0, uint64(len(policy.Leaves)))
	if err != nil {
		return nil, fmt.Errorf("psbt: failed to write leaf count: %w",
			err)
	}

	// Write each leaf.
	for i, leaf := range policy.Leaves {
		// Write depth (1 byte).
		if err := buf.WriteByte(depths[i]); err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to write depth for leaf %d: %w",
				i, err,
			)
		}

		// Write leaf version (1 byte).
		leafVer := byte(leaf.Leaf.LeafVersion)
		if err := buf.WriteByte(leafVer); err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to write version for leaf %d: %w",
				i, err,
			)
		}

		// Write script length.
		scriptLen := uint64(len(leaf.Leaf.Script))
		err := wire.WriteVarInt(&buf, 0, scriptLen)
		if err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to write script "+
					"length for leaf %d: %w",
				i, err,
			)
		}

		// Write script bytes.
		if _, err := buf.Write(leaf.Leaf.Script); err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to write script for leaf %d: %w",
				i, err,
			)
		}
	}

	return buf.Bytes(), nil
}

// DecodeTapTree deserializes a PSBT tap tree encoding back into leaf data.
// This can be used during PSBT finalization to reconstruct the tap tree.
func DecodeTapTree(data []byte) ([]EncodedLeaf, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("psbt: empty tap tree data")
	}

	r := bytes.NewReader(data)

	// Read leaf count.
	leafCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("psbt: failed to read leaf count: %w",
			err)
	}

	if leafCount == 0 {
		return nil, fmt.Errorf("psbt: zero leaves in tap tree")
	}

	if leafCount > maxTapTreeLeaves {
		return nil, fmt.Errorf(
			"psbt: leaf count %d exceeds maximum %d",
			leafCount, maxTapTreeLeaves,
		)
	}

	leaves := make([]EncodedLeaf, leafCount)

	for i := uint64(0); i < leafCount; i++ {
		// Read depth (1 byte).
		depth, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to read depth for leaf %d: %w",
				i, err,
			)
		}

		// Read leaf version (1 byte).
		version, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to read version for leaf %d: %w",
				i, err,
			)
		}

		// Read script length.
		scriptLen, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to read script "+
					"length for leaf %d: %w",
				i, err,
			)
		}

		// Read script bytes.
		script := make([]byte, scriptLen)
		if _, err := io.ReadFull(r, script); err != nil {
			return nil, fmt.Errorf(
				"psbt: failed to read script for leaf %d: %w",
				i, err,
			)
		}

		leaves[i] = EncodedLeaf{
			Depth:       depth,
			LeafVersion: version,
			Script:      script,
		}
	}

	// Verify we consumed all data.
	if r.Len() > 0 {
		return nil, fmt.Errorf("psbt: %d extra bytes after tap tree",
			r.Len())
	}

	return leaves, nil
}

// EncodeConditionWitness serializes a hashlock preimage for PSBT storage.
// Format: standard Bitcoin witness serialization (length + data).
// The preimage must not exceed maxPreimageLen (520) bytes.
func EncodeConditionWitness(preimage []byte) ([]byte, error) {
	if len(preimage) > maxPreimageLen {
		return nil, fmt.Errorf(
			"psbt: preimage length %d exceeds maximum %d",
			len(preimage), maxPreimageLen,
		)
	}

	var buf bytes.Buffer

	// Write preimage length + data.
	err := wire.WriteVarBytes(&buf, 0, preimage)
	if err != nil {
		return nil, fmt.Errorf("psbt: failed to write "+
			"preimage: %w", err)
	}

	return buf.Bytes(), nil
}

// DecodeConditionWitness deserializes a hashlock preimage from PSBT storage.
func DecodeConditionWitness(data []byte) ([]byte, error) {
	r := bytes.NewReader(data)

	preimage, err := wire.ReadVarBytes(
		r, 0, maxPreimageLen, "preimage",
	)
	if err != nil {
		return nil, fmt.Errorf("psbt: failed to read preimage: %w", err)
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf(
			"psbt: unexpected %d trailing bytes in "+
				"condition witness", r.Len(),
		)
	}

	return preimage, nil
}

// PutConditionWitnessPSBTInput stores the given hashlock preimage into the
// specified PSBT input's unknown fields using PSBTKeyConditionWitness.
func PutConditionWitnessPSBTInput(pkt *psbt.Packet, inputIndex int,
	preimage []byte) error {

	switch {
	case pkt == nil:
		return fmt.Errorf("psbt packet must be provided")

	case inputIndex < 0 || inputIndex >= len(pkt.Inputs):
		return fmt.Errorf("input index out of range: %d",
			inputIndex)

	case len(preimage) == 0:
		return fmt.Errorf("preimage cannot be empty")
	}

	encoded, err := EncodeConditionWitness(preimage)
	if err != nil {
		return err
	}

	unknowns := pkt.Inputs[inputIndex].Unknowns

	for _, u := range unknowns {
		if string(u.Key) == PSBTKeyConditionWitness {
			u.Value = encoded
			return nil
		}
	}

	unknowns = append(unknowns, &psbt.Unknown{
		Key:   []byte(PSBTKeyConditionWitness),
		Value: encoded,
	})
	pkt.Inputs[inputIndex].Unknowns = unknowns

	return nil
}

// GetConditionWitnessPSBTInput retrieves the hashlock preimage stored in the
// given PSBT input's unknown fields using PSBTKeyConditionWitness.
func GetConditionWitnessPSBTInput(input psbt.PInput) ([]byte, error) {
	var (
		encoded []byte
		found   bool
	)

	for _, u := range input.Unknowns {
		if string(u.Key) != PSBTKeyConditionWitness {
			continue
		}

		if found {
			return nil, fmt.Errorf("multiple condition witness " +
				"entries found")
		}

		if len(u.Value) == 0 {
			return nil, fmt.Errorf("condition witness value is " +
				"empty")
		}

		encoded = u.Value
		found = true
	}

	if !found {
		return nil, ErrConditionWitnessNotFound
	}

	return DecodeConditionWitness(encoded)
}
