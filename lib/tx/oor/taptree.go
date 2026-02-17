package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
)

var (
	// TapTreePSBTKey is the v0 convention for storing a taproot tree
	// encoding in a PSBT input unknown field.
	//
	// We treat this as part of the OOR PSBT profile so client and server
	// implementations can deterministically attach, validate, and later use
	// the same metadata during finalization.
	//
	// NOTE: PSBT unknown keys are a shared namespace. We use a short,
	// stable key here for v0 tests and in-process wiring.
	//
	// A future version should consider namespacing this (for example,
	// `ark/taptree`) to reduce collision risk with other PSBT extensions.
	TapTreePSBTKey = []byte("taptree")
)

// EncodeTapTree encodes a set of tapscript leaves into a single byte blob.
func EncodeTapTree(leaves [][]byte) ([]byte, error) {
	return checkpoint.EncodeTapTree(leaves)
}

// DecodeTapTree decodes a tap tree encoding produced by EncodeTapTree.
func DecodeTapTree(data []byte) ([][]byte, error) {
	return checkpoint.DecodeTapTree(data)
}

// PutTapTreePSBTInput stores an encoded tap tree blob into the given PSBT input
// unknown fields, using TapTreePSBTKey.
func PutTapTreePSBTInput(pkt *psbt.Packet, inputIndex int,
	encodedTapTree []byte) error {

	switch {
	case pkt == nil:
		return fmt.Errorf("psbt packet must be provided")

	case inputIndex < 0 || inputIndex >= len(pkt.Inputs):
		return fmt.Errorf("input index out of range: %d",
			inputIndex)

	case len(encodedTapTree) == 0:
		return fmt.Errorf("encoded tap tree cannot be empty")
	}

	// Replace any existing entry to keep this idempotent and avoid
	// ambiguous multiple values.
	unknowns := pkt.Inputs[inputIndex].Unknowns
	for _, u := range unknowns {
		if bytes.Equal(u.Key, TapTreePSBTKey) {
			u.Value = encodedTapTree
			return nil
		}
	}

	unknowns = append(unknowns, &psbt.Unknown{
		Key:   TapTreePSBTKey,
		Value: encodedTapTree,
	})
	pkt.Inputs[inputIndex].Unknowns = unknowns

	return nil
}

// GetTapTreePSBTInput retrieves an encoded tap tree blob from a PSBT input's
// unknown fields.
func GetTapTreePSBTInput(input psbt.PInput) ([]byte, error) {
	var (
		tapTreeValue []byte
		found        bool
	)

	for _, u := range input.Unknowns {
		if bytes.Equal(u.Key, TapTreePSBTKey) {
			if found {
				return nil, fmt.Errorf("multiple tap tree " +
					"entries found")
			}

			if len(u.Value) == 0 {
				return nil, fmt.Errorf(
					"tap tree value is empty",
				)
			}

			tapTreeValue = u.Value
			found = true
		}
	}

	if !found {
		return nil, fmt.Errorf("tap tree not found")
	}

	return tapTreeValue, nil
}
