package db

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type constants for VTXODescriptor.
const (
	vtxoDescPkScriptType    tlv.Type = 0
	vtxoDescAmountType      tlv.Type = 1
	vtxoDescCoSignerKeyType tlv.Type = 2
	vtxoDescPolicyType      tlv.Type = 3
)

// TLV type constants for ClientRegistration.
const (
	clientRegVersionType         tlv.Type = 0
	clientRegClientIDType        tlv.Type = 1
	clientRegBoardingInputsType  tlv.Type = 2
	clientRegLeaveOutputsType    tlv.Type = 3
	clientRegVTXODescriptorsType tlv.Type = 4
	clientRegForfeitInputsType   tlv.Type = 5
)

// TLV type constants for BoardingInput.
const (
	boardingOutpointType        tlv.Type = 0
	boardingTapscriptType       tlv.Type = 1
	boardingValueType           tlv.Type = 2
	boardingPkScriptType        tlv.Type = 3
	boardingClientKeyType       tlv.Type = 4
	boardingOperatorKeyDescType tlv.Type = 5
)

// TLV type constants for ForfeitInput.
const (
	forfeitOutpointType tlv.Type = 0 //nolint:unused
	forfeitVTXOType     tlv.Type = 1 //nolint:unused
)

// TLV type constants for VTXO (rounds.VTXO).
const (
	vtxoRoundIDType          tlv.Type = 0
	vtxoOutpointType         tlv.Type = 1
	vtxoBatchOutputIndexType tlv.Type = 2
	vtxoDescriptorType       tlv.Type = 3
	vtxoStatusType           tlv.Type = 4
)

// TLV type constants for Tapscript (waddrmgr.Tapscript).
const (
	tapscriptTypeType           tlv.Type = 0
	tapscriptControlBlockType   tlv.Type = 1
	tapscriptLeavesType         tlv.Type = 2
	tapscriptRevealedScriptType tlv.Type = 3
	tapscriptRootHashType       tlv.Type = 4
)

// TLV type constants for KeyDescriptor.
const (
	keyDescKeyLocatorType tlv.Type = 0
	keyDescPubKeyType     tlv.Type = 1
)

// TLV type constants for KeyLocator.
const (
	keyLocatorFamilyType tlv.Type = 0
	keyLocatorIndexType  tlv.Type = 1
)

// SerializeVTXODescriptor serializes a tree.VTXODescriptor using TLV encoding.
func SerializeVTXODescriptor(desc *tree.VTXODescriptor) ([]byte, error) {
	if desc == nil {
		return nil, fmt.Errorf("cannot serialize nil VTXODescriptor")
	}

	var (
		buf            bytes.Buffer
		amount         = uint64(desc.Amount)
		policyTemplate = bytes.Clone(desc.PolicyTemplate)
	)

	// Serialize cosigner key as compressed 33-byte format.
	cosignerKeyBytes := desc.CoSignerKey.SerializeCompressed()

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(vtxoDescPkScriptType, &desc.PkScript,
			func() uint64 {
				return uint64(len(desc.PkScript))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakePrimitiveRecord(vtxoDescAmountType, &amount),
		tlv.MakeDynamicRecord(vtxoDescCoSignerKeyType,
			&cosignerKeyBytes, func() uint64 {
				return 33
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(vtxoDescPolicyType, &policyTemplate,
			func() uint64 {
				return uint64(len(policyTemplate))
			}, tlv.EVarBytes, tlv.DVarBytes),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DeserializeVTXODescriptor deserializes a tree.VTXODescriptor from
// TLV-encoded bytes.
func DeserializeVTXODescriptor(data []byte) (*tree.VTXODescriptor, error) {
	var (
		pkScript         []byte
		amount           uint64
		cosignerKeyBytes []byte
		policyTemplate   []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			vtxoDescPkScriptType, &pkScript, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(vtxoDescAmountType, &amount),
		tlv.MakeDynamicRecord(
			vtxoDescCoSignerKeyType, &cosignerKeyBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			vtxoDescPolicyType, &policyTemplate, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	// Parse cosigner key.
	cosignerKey, err := btcec.ParsePubKey(cosignerKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse cosigner key: %w", err)
	}

	return &tree.VTXODescriptor{
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Amount:         btcutil.Amount(amount),
		CoSignerKey:    cosignerKey,
	}, nil
}

// serializeTxOut serializes a wire.TxOut.
func serializeTxOut(w io.Writer, txOut *wire.TxOut) error {
	// Write value (8 bytes).
	if err := binary.Write(w, binary.BigEndian, txOut.Value); err != nil {
		return err
	}

	// Write script length as varint.
	scriptLen := uint64(len(txOut.PkScript))
	if err := tlv.WriteVarInt(w, scriptLen, &[8]byte{}); err != nil {
		return err
	}

	// Write script.
	if _, err := w.Write(txOut.PkScript); err != nil {
		return err
	}

	return nil
}

// deserializeTxOut deserializes a wire.TxOut.
func deserializeTxOut(r io.Reader) (*wire.TxOut, error) {
	// Read value (8 bytes).
	var value int64
	if err := binary.Read(r, binary.BigEndian, &value); err != nil {
		return nil, err
	}

	// Read script length as varint.
	scriptLen, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return nil, err
	}

	// Read script.
	script := make([]byte, scriptLen)
	if _, err := io.ReadFull(r, script); err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    value,
		PkScript: script,
	}, nil
}

// serializeOutpoint serializes a wire.OutPoint (36 bytes fixed).
func serializeOutpoint(w io.Writer, outpoint *wire.OutPoint) error {
	if outpoint == nil {
		return fmt.Errorf("cannot serialize nil outpoint")
	}

	// Write hash (32 bytes).
	if _, err := w.Write(outpoint.Hash[:]); err != nil {
		return err
	}

	// Write index (4 bytes).
	return binary.Write(w, binary.BigEndian, outpoint.Index)
}

// deserializeOutpoint deserializes a wire.OutPoint (36 bytes fixed).
func deserializeOutpoint(r io.Reader) (*wire.OutPoint, error) {
	var outpoint wire.OutPoint

	// Read hash (32 bytes).
	_, err := io.ReadFull(r, outpoint.Hash[:])
	if err != nil {
		return nil, err
	}

	// Read index (4 bytes).
	err = binary.Read(r, binary.BigEndian, &outpoint.Index)
	if err != nil {
		return nil, err
	}

	return &outpoint, nil
}

// serializeTapscript serializes a waddrmgr.Tapscript using TLV encoding.
// Returns empty bytes for nil Tapscript.
func serializeTapscript(tapscript *waddrmgr.Tapscript) ([]byte, error) {
	if tapscript == nil {
		return []byte{}, nil
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			tapscriptTypeType, (*uint8)(&tapscript.Type),
		),
	}

	// Serialize ControlBlock if present.
	if tapscript.ControlBlock != nil {
		ctrlBytes, err := tapscript.ControlBlock.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("control block to bytes: %w",
				err)
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				tapscriptControlBlockType, &ctrlBytes,
			),
		)
	}

	// Serialize TapLeaves if present.
	if len(tapscript.Leaves) > 0 {
		leavesBytes, err := serializeTapLeaves(tapscript.Leaves)
		if err != nil {
			return nil, fmt.Errorf("serialize leaves: %w", err)
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				tapscriptLeavesType, &leavesBytes,
			),
		)
	}

	// Serialize RevealedScript if present.
	if len(tapscript.RevealedScript) > 0 {
		records = append(
			records, tlv.MakePrimitiveRecord(
				tapscriptRevealedScriptType,
				&tapscript.RevealedScript,
			),
		)
	}

	// Serialize RootHash if present.
	if tapscript.RootHash != nil {
		rootHashBytes := tapscript.RootHash
		records = append(
			records, tlv.MakePrimitiveRecord(
				tapscriptRootHashType, &rootHashBytes,
			),
		)
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create tlv stream: %w", err)
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode tapscript: %w", err)
	}

	return buf.Bytes(), nil
}

// deserializeTapscript deserializes a waddrmgr.Tapscript from TLV-encoded
// bytes. Returns nil for empty data.
func deserializeTapscript(data []byte) (*waddrmgr.Tapscript, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var (
		tapscriptType     uint8
		controlBlockBytes []byte
		leavesBytes       []byte
		revealedScript    []byte
		rootHashBytes     []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(tapscriptTypeType, &tapscriptType),
		tlv.MakePrimitiveRecord(
			tapscriptControlBlockType, &controlBlockBytes,
		),
		tlv.MakePrimitiveRecord(tapscriptLeavesType, &leavesBytes),
		tlv.MakePrimitiveRecord(
			tapscriptRevealedScriptType, &revealedScript,
		),
		tlv.MakePrimitiveRecord(tapscriptRootHashType, &rootHashBytes),
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create tlv stream: %w", err)
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("decode tapscript: %w", err)
	}

	tapscript := &waddrmgr.Tapscript{
		Type:           waddrmgr.TapscriptType(tapscriptType),
		RevealedScript: revealedScript,
	}

	// Parse ControlBlock if present.
	if len(controlBlockBytes) > 0 {
		ctrlBlock, err := txscript.ParseControlBlock(controlBlockBytes)
		if err != nil {
			return nil, fmt.Errorf("parse control block: %w", err)
		}

		tapscript.ControlBlock = ctrlBlock
	}

	// Deserialize TapLeaves if present.
	if len(leavesBytes) > 0 {
		leaves, err := deserializeTapLeaves(leavesBytes)
		if err != nil {
			return nil, fmt.Errorf("deserialize leaves: %w", err)
		}

		tapscript.Leaves = leaves
	}

	// Parse RootHash if present.
	if len(rootHashBytes) == 32 {
		var rootHash [32]byte
		copy(rootHash[:], rootHashBytes)
		tapscript.RootHash = rootHash[:]
	}

	return tapscript, nil
}

// serializeTapLeaves serializes a slice of TapLeaf.
func serializeTapLeaves(leaves []txscript.TapLeaf) ([]byte, error) {
	var buf bytes.Buffer

	// Write number of leaves.
	err := binary.Write(&buf, binary.BigEndian, uint32(len(leaves)))
	if err != nil {
		return nil, fmt.Errorf("write leaf count: %w", err)
	}

	// Write each leaf.
	for _, leaf := range leaves {
		// Write leaf version (1 byte).
		if err := buf.WriteByte(byte(leaf.LeafVersion)); err != nil {
			return nil, fmt.Errorf("write leaf version: %w", err)
		}

		// Write script length and script.
		scriptLen := uint32(len(leaf.Script))
		err := binary.Write(&buf, binary.BigEndian, scriptLen)
		if err != nil {
			return nil, fmt.Errorf("write script length: %w", err)
		}

		if _, err := buf.Write(leaf.Script); err != nil {
			return nil, fmt.Errorf("write script: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// deserializeTapLeaves deserializes a slice of TapLeaf.
func deserializeTapLeaves(data []byte) ([]txscript.TapLeaf, error) {
	buf := bytes.NewReader(data)

	// Read number of leaves.
	var count uint32
	if err := binary.Read(buf, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read leaf count: %w", err)
	}

	leaves := make([]txscript.TapLeaf, count)
	for i := range leaves {
		// Read leaf version.
		version, err := buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read leaf version: %w", err)
		}

		leaves[i].LeafVersion = txscript.TapscriptLeafVersion(version)

		// Read script length and script.
		var scriptLen uint32
		err = binary.Read(buf, binary.BigEndian, &scriptLen)
		if err != nil {
			return nil, fmt.Errorf("read script length: %w", err)
		}

		leaves[i].Script = make([]byte, scriptLen)
		if _, err := io.ReadFull(buf, leaves[i].Script); err != nil {
			return nil, fmt.Errorf("read script: %w", err)
		}
	}

	return leaves, nil
}

// serializeKeyDescriptor serializes a keychain.KeyDescriptor using TLV
// encoding.
func serializeKeyDescriptor(keyDesc *keychain.KeyDescriptor) ([]byte, error) {
	if keyDesc == nil {
		return nil, fmt.Errorf("cannot serialize nil KeyDescriptor")
	}

	var buf bytes.Buffer

	// Serialize KeyLocator.
	keyLocatorBytes, err := serializeKeyLocator(&keyDesc.KeyLocator)
	if err != nil {
		return nil, err
	}

	// Serialize public key.
	var pubKeyBytes []byte
	if keyDesc.PubKey != nil {
		pubKeyBytes = keyDesc.PubKey.SerializeCompressed()
	}

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(keyDescKeyLocatorType, &keyLocatorBytes,
			func() uint64 {
				return uint64(len(keyLocatorBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(keyDescPubKeyType, &pubKeyBytes,
			func() uint64 {
				return uint64(len(pubKeyBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// deserializeKeyDescriptor deserializes a keychain.KeyDescriptor from
// TLV-encoded bytes.
func deserializeKeyDescriptor(data []byte) (*keychain.KeyDescriptor, error) {
	var (
		keyLocatorBytes []byte
		pubKeyBytes     []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			keyDescKeyLocatorType, &keyLocatorBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			keyDescPubKeyType, &pubKeyBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	// Deserialize KeyLocator.
	keyLocator, err := deserializeKeyLocator(keyLocatorBytes)
	if err != nil {
		return nil, err
	}

	// Parse public key if present.
	var pubKey *btcec.PublicKey
	if len(pubKeyBytes) > 0 {
		pubKey, err = btcec.ParsePubKey(pubKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse pub key: %w", err)
		}
	}

	return &keychain.KeyDescriptor{
		KeyLocator: *keyLocator,
		PubKey:     pubKey,
	}, nil
}

// serializeKeyLocator serializes a keychain.KeyLocator using TLV encoding.
func serializeKeyLocator(keyLoc *keychain.KeyLocator) ([]byte, error) {
	var buf bytes.Buffer
	family := uint64(keyLoc.Family)
	index := uint64(keyLoc.Index)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(keyLocatorFamilyType, &family),
		tlv.MakePrimitiveRecord(keyLocatorIndexType, &index),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// deserializeKeyLocator deserializes a keychain.KeyLocator from TLV-encoded
// bytes.
func deserializeKeyLocator(data []byte) (*keychain.KeyLocator, error) {
	var (
		family uint64
		index  uint64
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(keyLocatorFamilyType, &family),
		tlv.MakePrimitiveRecord(keyLocatorIndexType, &index),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	return &keychain.KeyLocator{
		Family: keychain.KeyFamily(family),
		Index:  uint32(index),
	}, nil
}

// serializeBoardingInput serializes a rounds.BoardingInput using TLV encoding.
func serializeBoardingInput(input *rounds.BoardingInput) ([]byte, error) {
	if input == nil {
		return nil, fmt.Errorf("cannot serialize nil BoardingInput")
	}

	var buf bytes.Buffer

	// Serialize outpoint.
	var outpointBuf bytes.Buffer
	if err := serializeOutpoint(&outpointBuf, input.Outpoint); err != nil {
		return nil, err
	}
	outpointBytes := outpointBuf.Bytes()

	// Serialize tapscript.
	tapscriptBytes, err := serializeTapscript(input.Tapscript)
	if err != nil {
		return nil, err
	}

	// Serialize value.
	value := uint64(input.Value)

	// Serialize client key.
	clientKeyBytes := input.ClientKey.SerializeCompressed()

	// Serialize operator key descriptor.
	operatorKeyDescBytes, err := serializeKeyDescriptor(
		input.OperatorKeyDesc,
	)
	if err != nil {
		return nil, err
	}

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(boardingOutpointType, &outpointBytes,
			func() uint64 {
				return uint64(len(outpointBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(boardingTapscriptType, &tapscriptBytes,
			func() uint64 {
				return uint64(len(tapscriptBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakePrimitiveRecord(boardingValueType, &value),
		tlv.MakeDynamicRecord(boardingPkScriptType, &input.PkScript,
			func() uint64 {
				return uint64(len(input.PkScript))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(boardingClientKeyType, &clientKeyBytes,
			func() uint64 {
				return 33
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(boardingOperatorKeyDescType,
			&operatorKeyDescBytes, func() uint64 {
				return uint64(len(operatorKeyDescBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// deserializeBoardingInput deserializes a rounds.BoardingInput from
// TLV-encoded bytes.
func deserializeBoardingInput(data []byte) (*rounds.BoardingInput, error) {
	var (
		outpointBytes        []byte
		tapscriptBytes       []byte
		value                uint64
		pkScript             []byte
		clientKeyBytes       []byte
		operatorKeyDescBytes []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			boardingOutpointType, &outpointBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			boardingTapscriptType, &tapscriptBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(boardingValueType, &value),
		tlv.MakeDynamicRecord(
			boardingPkScriptType, &pkScript, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			boardingClientKeyType, &clientKeyBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			boardingOperatorKeyDescType, &operatorKeyDescBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	// Deserialize outpoint.
	outpoint, err := deserializeOutpoint(bytes.NewReader(outpointBytes))
	if err != nil {
		return nil, err
	}

	// Deserialize tapscript.
	tapscript, err := deserializeTapscript(tapscriptBytes)
	if err != nil {
		return nil, err
	}

	// Parse client key.
	clientKey, err := btcec.ParsePubKey(clientKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse client key: %w", err)
	}

	// Deserialize operator key descriptor.
	operatorKeyDesc, err := deserializeKeyDescriptor(operatorKeyDescBytes)
	if err != nil {
		return nil, err
	}

	return &rounds.BoardingInput{
		Outpoint:        outpoint,
		Tapscript:       tapscript,
		Value:           btcutil.Amount(value),
		PkScript:        pkScript,
		ClientKey:       clientKey,
		OperatorKeyDesc: operatorKeyDesc,
	}, nil
}

// SerializeClientRegistration serializes a rounds.ClientRegistration using TLV
// encoding.
func SerializeClientRegistration(reg *rounds.ClientRegistration) ([]byte,
	error) {

	if reg == nil {
		return nil, fmt.Errorf("cannot serialize nil " +
			"ClientRegistration")
	}

	var buf bytes.Buffer

	// Encode a version tag to allow forward-compatible format changes.
	regVersion := uint16(1)

	// Serialize client ID (string).
	clientIDBytes := []byte(reg.ClientID)

	// Serialize boarding inputs (slice).
	var boardingInputsBuf bytes.Buffer
	if err := binary.Write(
		&boardingInputsBuf, binary.BigEndian,
		uint32(
			len(reg.BoardingInputs),
		),
	); err != nil {
		return nil, err
	}
	for _, input := range reg.BoardingInputs {
		inputBytes, err := serializeBoardingInput(input)
		if err != nil {
			return nil, err
		}
		// Write length prefix.
		if err := binary.Write(
			&boardingInputsBuf, binary.BigEndian,
			uint32(
				len(inputBytes),
			),
		); err != nil {
			return nil, err
		}
		// Write data.
		if _, err := boardingInputsBuf.Write(inputBytes); err != nil {
			return nil, err
		}
	}
	boardingInputsBytes := boardingInputsBuf.Bytes()

	// Serialize leave outputs (slice).
	var leaveOutputsBuf bytes.Buffer
	if err := binary.Write(
		&leaveOutputsBuf, binary.BigEndian,
		uint32(
			len(reg.LeaveOutputs),
		),
	); err != nil {
		return nil, err
	}
	for _, output := range reg.LeaveOutputs {
		if err := serializeTxOut(&leaveOutputsBuf, output); err != nil {
			return nil, err
		}
	}
	leaveOutputsBytes := leaveOutputsBuf.Bytes()

	// Serialize VTXO descriptors (map).
	var vtxoDescsBuf bytes.Buffer
	if err := binary.Write(
		&vtxoDescsBuf, binary.BigEndian,
		uint32(
			len(reg.VTXODescriptors),
		),
	); err != nil {
		return nil, err
	}
	vtxoDescKeys := make(
		[]rounds.SigningKeyHex, 0, len(reg.VTXODescriptors),
	)
	for signingKeyHex := range reg.VTXODescriptors {
		vtxoDescKeys = append(vtxoDescKeys, signingKeyHex)
	}
	sort.Slice(vtxoDescKeys, func(i, j int) bool {
		return bytes.Compare(
			vtxoDescKeys[i][:], vtxoDescKeys[j][:],
		) < 0
	})
	for _, signingKeyHex := range vtxoDescKeys {
		desc := reg.VTXODescriptors[signingKeyHex]

		// Write signing key (33 bytes).
		keyBytes := signingKeyHex[:]
		if _, err := vtxoDescsBuf.Write(keyBytes); err != nil {
			return nil, err
		}

		// Serialize descriptor.
		descBytes, err := SerializeVTXODescriptor(desc)
		if err != nil {
			return nil, err
		}

		// Write length prefix.
		if err := binary.Write(
			&vtxoDescsBuf, binary.BigEndian,
			uint32(
				len(descBytes),
			),
		); err != nil {
			return nil, err
		}

		// Write data.
		if _, err := vtxoDescsBuf.Write(descBytes); err != nil {
			return nil, err
		}
	}
	vtxoDescsBytes := vtxoDescsBuf.Bytes()

	// Serialize forfeit inputs (slice).
	var forfeitInputsBuf bytes.Buffer
	if err := binary.Write(
		&forfeitInputsBuf, binary.BigEndian,
		uint32(
			len(reg.ForfeitInputs),
		),
	); err != nil {
		return nil, err
	}
	for _, input := range reg.ForfeitInputs {
		// Serialize outpoint.
		if err := serializeOutpoint(
			&forfeitInputsBuf, input.Outpoint,
		); err != nil {
			return nil, err
		}

		// Serialize VTXO.
		vtxoBytes, err := serializeRoundsVTXO(input.VTXO)
		if err != nil {
			return nil, err
		}

		// Write length prefix.
		if err := binary.Write(
			&forfeitInputsBuf, binary.BigEndian,
			uint32(
				len(vtxoBytes),
			),
		); err != nil {
			return nil, err
		}

		// Write data.
		if _, err := forfeitInputsBuf.Write(vtxoBytes); err != nil {
			return nil, err
		}
	}
	forfeitInputsBytes := forfeitInputsBuf.Bytes()

	// Create TLV stream.
	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(clientRegVersionType, &regVersion),
		tlv.MakeDynamicRecord(clientRegClientIDType, &clientIDBytes,
			func() uint64 {
				return uint64(len(clientIDBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegBoardingInputsType,
			&boardingInputsBytes, func() uint64 {
				return uint64(len(boardingInputsBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegLeaveOutputsType,
			&leaveOutputsBytes, func() uint64 {
				return uint64(len(leaveOutputsBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegVTXODescriptorsType,
			&vtxoDescsBytes, func() uint64 {
				return uint64(len(vtxoDescsBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegForfeitInputsType,
			&forfeitInputsBytes, func() uint64 {
				return uint64(len(forfeitInputsBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DeserializeClientRegistration deserializes a rounds.ClientRegistration from
// TLV-encoded bytes.
func DeserializeClientRegistration(data []byte) (*rounds.ClientRegistration,
	error) {

	var (
		regVersion          uint16
		clientIDBytes       []byte
		boardingInputsBytes []byte
		leaveOutputsBytes   []byte
		vtxoDescsBytes      []byte
		forfeitInputsBytes  []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(clientRegVersionType, &regVersion),
		tlv.MakeDynamicRecord(
			clientRegClientIDType, &clientIDBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			clientRegBoardingInputsType, &boardingInputsBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			clientRegLeaveOutputsType, &leaveOutputsBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			clientRegVTXODescriptorsType, &vtxoDescsBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			clientRegForfeitInputsType, &forfeitInputsBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	if regVersion != 1 {
		return nil, fmt.Errorf("unsupported client registration "+
			"version %d", regVersion)
	}

	// Deserialize client ID.
	clientID := clientconn.ClientID(clientIDBytes)

	// Deserialize boarding inputs.
	boardingInputsReader := bytes.NewReader(boardingInputsBytes)
	var boardingInputsCount uint32
	if err := binary.Read(
		boardingInputsReader, binary.BigEndian, &boardingInputsCount,
	); err != nil {
		return nil, err
	}

	boardingInputs := make([]*rounds.BoardingInput, boardingInputsCount)
	for i := range boardingInputs {
		var length uint32
		if err := binary.Read(
			boardingInputsReader, binary.BigEndian, &length,
		); err != nil {
			return nil, err
		}

		inputBytes := make([]byte, length)
		if _, err := io.ReadFull(
			boardingInputsReader, inputBytes,
		); err != nil {
			return nil, err
		}

		input, err := deserializeBoardingInput(inputBytes)
		if err != nil {
			return nil, err
		}
		boardingInputs[i] = input
	}

	// Deserialize leave outputs.
	leaveOutputsReader := bytes.NewReader(leaveOutputsBytes)
	var leaveOutputsCount uint32
	if err := binary.Read(
		leaveOutputsReader, binary.BigEndian, &leaveOutputsCount,
	); err != nil {
		return nil, err
	}

	leaveOutputs := make([]*wire.TxOut, leaveOutputsCount)
	for i := range leaveOutputs {
		output, err := deserializeTxOut(leaveOutputsReader)
		if err != nil {
			return nil, err
		}
		leaveOutputs[i] = output
	}

	// Deserialize VTXO descriptors.
	vtxoDescsReader := bytes.NewReader(vtxoDescsBytes)
	var vtxoDescsCount uint32
	if err := binary.Read(
		vtxoDescsReader, binary.BigEndian, &vtxoDescsCount,
	); err != nil {
		return nil, err
	}

	vtxoDescriptors := make(
		map[route.Vertex]*tree.VTXODescriptor, vtxoDescsCount,
	)
	for i := uint32(0); i < vtxoDescsCount; i++ {
		// Read signing key (33 bytes).
		var keyBytes [33]byte
		_, err = io.ReadFull(vtxoDescsReader, keyBytes[:])
		if err != nil {
			return nil, err
		}
		var signingKey route.Vertex
		copy(signingKey[:], keyBytes[:])

		// Read length.
		var length uint32
		if err := binary.Read(
			vtxoDescsReader, binary.BigEndian, &length,
		); err != nil {
			return nil, err
		}

		// Read descriptor bytes.
		descBytes := make([]byte, length)
		_, err = io.ReadFull(vtxoDescsReader, descBytes)
		if err != nil {
			return nil, err
		}

		// Deserialize descriptor.
		desc, err := DeserializeVTXODescriptor(descBytes)
		if err != nil {
			return nil, err
		}

		vtxoDescriptors[signingKey] = desc
	}

	// Deserialize forfeit inputs.
	forfeitInputsReader := bytes.NewReader(forfeitInputsBytes)
	var forfeitInputsCount uint32
	if err := binary.Read(
		forfeitInputsReader, binary.BigEndian, &forfeitInputsCount,
	); err != nil {
		return nil, err
	}

	forfeitInputs := make([]*rounds.ForfeitInput, forfeitInputsCount)
	for i := range forfeitInputs {
		// Deserialize outpoint.
		outpoint, err := deserializeOutpoint(forfeitInputsReader)
		if err != nil {
			return nil, err
		}

		// Read length.
		var length uint32
		if err := binary.Read(
			forfeitInputsReader, binary.BigEndian, &length,
		); err != nil {
			return nil, err
		}

		// Read VTXO bytes.
		vtxoBytes := make([]byte, length)
		_, err = io.ReadFull(forfeitInputsReader, vtxoBytes)
		if err != nil {
			return nil, err
		}

		// Deserialize VTXO.
		vtxo, err := deserializeRoundsVTXO(vtxoBytes)
		if err != nil {
			return nil, err
		}

		forfeitInputs[i] = &rounds.ForfeitInput{
			Outpoint: outpoint,
			VTXO:     vtxo,
		}
	}

	return &rounds.ClientRegistration{
		ClientID:        clientID,
		BoardingInputs:  boardingInputs,
		LeaveOutputs:    leaveOutputs,
		VTXODescriptors: vtxoDescriptors,
		ForfeitInputs:   forfeitInputs,
	}, nil
}

// serializeRoundsVTXO serializes a rounds.VTXO using TLV encoding.
func serializeRoundsVTXO(vtxo *rounds.VTXO) ([]byte, error) {
	if vtxo == nil {
		return nil, fmt.Errorf("cannot serialize nil rounds.VTXO")
	}

	var buf bytes.Buffer

	// Serialize round ID (16 bytes).
	roundIDBytes := vtxo.RoundID[:]

	// Serialize outpoint.
	var outpointBuf bytes.Buffer
	if err := serializeOutpoint(&outpointBuf, &vtxo.Outpoint); err != nil {
		return nil, err
	}
	outpointBytes := outpointBuf.Bytes()

	// Serialize batch output index.
	batchOutputIndex := uint32(vtxo.BatchOutputIndex)

	// Serialize descriptor.
	descriptorBytes, err := SerializeVTXODescriptor(vtxo.Descriptor)
	if err != nil {
		return nil, err
	}

	// Serialize status.
	statusBytes := []byte(vtxo.Status)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(vtxoRoundIDType, &roundIDBytes,
			func() uint64 {
				return 16
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(vtxoOutpointType, &outpointBytes,
			func() uint64 {
				return uint64(len(outpointBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakePrimitiveRecord(
			vtxoBatchOutputIndexType, &batchOutputIndex,
		),
		tlv.MakeDynamicRecord(vtxoDescriptorType, &descriptorBytes,
			func() uint64 {
				return uint64(len(descriptorBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(vtxoStatusType, &statusBytes,
			func() uint64 {
				return uint64(len(statusBytes))
			}, tlv.EVarBytes, tlv.DVarBytes),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// deserializeRoundsVTXO deserializes a rounds.VTXO from TLV-encoded bytes.
func deserializeRoundsVTXO(data []byte) (*rounds.VTXO, error) {
	var (
		roundIDBytes     []byte
		outpointBytes    []byte
		batchOutputIndex uint32
		descriptorBytes  []byte
		statusBytes      []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			vtxoRoundIDType, &roundIDBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			vtxoOutpointType, &outpointBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			vtxoBatchOutputIndexType, &batchOutputIndex,
		),
		tlv.MakeDynamicRecord(
			vtxoDescriptorType, &descriptorBytes, nil,
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			vtxoStatusType, &statusBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	if err != nil {
		return nil, err
	}

	if err := tlvStream.Decode(bytes.NewReader(data)); err != nil {
		return nil, err
	}

	// Deserialize round ID.
	var roundID rounds.RoundID
	copy(roundID[:], roundIDBytes)

	outpoint, err := deserializeOutpoint(bytes.NewReader(outpointBytes))
	if err != nil {
		return nil, err
	}

	// Deserialize descriptor.
	descriptor, err := DeserializeVTXODescriptor(descriptorBytes)
	if err != nil {
		return nil, err
	}

	return &rounds.VTXO{
		Outpoint:         *outpoint,
		RoundID:          roundID,
		BatchOutputIndex: int(batchOutputIndex),
		Descriptor:       descriptor,
		Status:           rounds.VTXOStatus(statusBytes),
	}, nil
}
