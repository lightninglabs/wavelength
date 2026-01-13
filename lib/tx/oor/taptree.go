package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	// TapTreePSBTKey is the v0 convention for storing a taproot tree
	// encoding in a PSBT input unknown field.
	//
	// We treat this as part of the OOR PSBT profile so client and server
	// implementations can deterministically attach, validate, and later use
	// the same metadata during finalization.
	TapTreePSBTKey = []byte("taptree")
)

// EncodeTapTree encodes a set of tapscript leaves into a single byte blob.
//
// EncodeTapTree intentionally uses a simple leaf list representation that is
// sufficient for v0 OOR transfers. Each leaf is encoded at depth 1 with the
// base tapscript leaf version. The encoding uses Bitcoin varint (compact size)
// lengths and is compatible with how many BIP-371 encodings represent tap
// trees.
//
// This encoding is part of the PSBT profile for OOR transfers. If we ever need
// to support richer trees (multiple depths), this function must become
// versioned rather than changing behavior silently.
func EncodeTapTree(leaves [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	err := wire.WriteVarInt(&buf, 0, uint64(len(leaves)))
	if err != nil {
		return nil, fmt.Errorf("unable to write leaf count: %w",
			err)
	}

	for _, leaf := range leaves {
		err := buf.WriteByte(1)
		if err != nil {
			return nil, fmt.Errorf("unable to write depth: %w",
				err)
		}

		err = buf.WriteByte(byte(txscript.BaseLeafVersion))
		if err != nil {
			return nil, fmt.Errorf("unable to write leaf "+
				"version: %w", err)
		}

		err = wire.WriteVarInt(&buf, 0, uint64(len(leaf)))
		if err != nil {
			return nil, fmt.Errorf("unable to write leaf "+
				"length: %w", err)
		}

		_, err = buf.Write(leaf)
		if err != nil {
			return nil, fmt.Errorf("unable to write leaf "+
				"script: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// DecodeTapTree decodes a tap tree encoding produced by EncodeTapTree.
//
// DecodeTapTree is intentionally lenient about leaf depth and version in v0:
// it reads and ignores them. The returned value is the list of raw script
// bytes for each leaf.
func DecodeTapTree(data []byte) ([][]byte, error) {
	buf := bytes.NewReader(data)

	leafCount, err := wire.ReadVarInt(buf, 0)
	if err != nil {
		return nil, fmt.Errorf("unable to read leaf count: %w",
			err)
	}

	leaves := make([][]byte, 0, leafCount)
	for i := uint64(0); i < leafCount; i++ {
		_, err := buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("unable to read depth: %w",
				err)
		}

		_, err = buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("unable to read leaf "+
				"version: %w", err)
		}

		scriptLen, err := wire.ReadVarInt(buf, 0)
		if err != nil {
			return nil, fmt.Errorf("unable to read script "+
				"length: %w", err)
		}

		scriptBytes := make([]byte, scriptLen)
		_, err = buf.Read(scriptBytes)
		if err != nil {
			return nil, fmt.Errorf(
				"unable to read script bytes: %w", err,
			)
		}

		leaves = append(leaves, scriptBytes)
	}

	if buf.Len() != 0 {
		return nil, fmt.Errorf("trailing bytes in tap tree "+
			"encoding (%d bytes)", buf.Len())
	}

	return leaves, nil
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

	// Replace any existing entry to keep this idempotent and avoid ambiguous
	// multiple values.
	unknowns := pkt.Inputs[inputIndex].Unknowns
	for _, u := range unknowns {
		if bytes.Equal(u.Key, TapTreePSBTKey) {
			u.Value = encodedTapTree
			return nil
		}
	}

	pkt.Inputs[inputIndex].Unknowns = append(unknowns, &psbt.Unknown{
		Key:   TapTreePSBTKey,
		Value: encodedTapTree,
	})

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
