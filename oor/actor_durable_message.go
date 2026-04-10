package oor

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV record type constants for signing descriptor fields. These are used
// by encodeSigningDescriptor/decodeSigningDescriptor which serialize
// VTXOSigningDescriptor values within actor message TLV streams.
const (
	signingDescriptorOutpointRecordType  tlv.Type = 1
	signingDescriptorIndexRecordType     tlv.Type = 2
	signingDescriptorVTXOPolicyType      tlv.Type = 3
	signingDescriptorSpendPathType       tlv.Type = 4
	signingDescriptorOwnerLeafPolicyType tlv.Type = 5
)

const (
	recipientOutputPkScriptRecordType   tlv.Type = 1
	recipientOutputValueRecordType      tlv.Type = 2
	recipientOutputVTXOPolicyRecordType tlv.Type = 3
)

// serializePSBTList serializes a list of PSBT packets to raw byte blobs.
func serializePSBTList(packets []*psbt.Packet) ([][]byte, error) {
	serialized := make([][]byte, 0, len(packets))

	for i, packet := range packets {
		blob, err := serializePSBT(packet)
		if err != nil {
			return nil, fmt.Errorf("serialize psbt %d: %w", i, err)
		}

		serialized = append(serialized, blob)
	}

	return serialized, nil
}

// deserializePSBTList deserializes a list of raw PSBT byte blobs.
func deserializePSBTList(blobs [][]byte) ([]*psbt.Packet, error) {
	packets := make([]*psbt.Packet, 0, len(blobs))

	for i, blob := range blobs {
		packet, err := deserializePSBT(blob)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize psbt %d: %w", i, err,
			)
		}

		packets = append(packets, packet)
	}

	return packets, nil
}

// encodeSigningDescriptor encodes one signing descriptor as TLV records.
func encodeSigningDescriptor(desc VTXOSigningDescriptor) ([]byte, error) {
	var outpointHash [chainhash.HashSize]byte
	copy(outpointHash[:], desc.Outpoint.Hash[:])

	outpointIndex := desc.Outpoint.Index
	vtxoPolicyTemplate := desc.VTXOPolicyTemplate
	spendPath := desc.SpendPath
	ownerLeafPolicy := desc.OwnerLeafPolicy

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			signingDescriptorOutpointRecordType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorIndexRecordType, &outpointIndex,
		),
	}

	if len(vtxoPolicyTemplate) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			signingDescriptorVTXOPolicyType, &vtxoPolicyTemplate,
		))
	}

	if len(spendPath) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			signingDescriptorSpendPathType, &spendPath,
		))
	}
	if len(ownerLeafPolicy) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			signingDescriptorOwnerLeafPolicyType,
			&ownerLeafPolicy,
		))
	}

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

// decodeSigningDescriptor decodes one signing descriptor from TLV records.
func decodeSigningDescriptor(blob []byte) (VTXOSigningDescriptor, error) {
	var (
		outpointHash       [chainhash.HashSize]byte
		outpointIndex      uint32
		vtxoPolicyTemplate []byte
		spendPath          []byte
		ownerLeafPolicy    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			signingDescriptorOutpointRecordType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorIndexRecordType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorVTXOPolicyType, &vtxoPolicyTemplate,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorSpendPathType, &spendPath,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorOwnerLeafPolicyType,
			&ownerLeafPolicy,
		),
	)
	if err != nil {
		return VTXOSigningDescriptor{}, err
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(blob))
	if err != nil {
		return VTXOSigningDescriptor{}, err
	}

	if _, ok := parsed[signingDescriptorOutpointRecordType]; !ok {
		return VTXOSigningDescriptor{}, fmt.Errorf(
			"outpoint hash must be provided",
		)
	}
	if _, ok := parsed[signingDescriptorIndexRecordType]; !ok {
		return VTXOSigningDescriptor{}, fmt.Errorf(
			"outpoint index must be provided",
		)
	}
	if _, ok := parsed[signingDescriptorVTXOPolicyType]; !ok {
		return VTXOSigningDescriptor{}, fmt.Errorf(
			"vtxo policy template must be provided",
		)
	}
	if _, ok := parsed[signingDescriptorSpendPathType]; !ok {
		return VTXOSigningDescriptor{}, fmt.Errorf(
			"spend path must be provided",
		)
	}

	var hash chainhash.Hash
	copy(hash[:], outpointHash[:])

	return VTXOSigningDescriptor{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: outpointIndex,
		},
		VTXOPolicyTemplate: vtxoPolicyTemplate,
		SpendPath:          spendPath,
		OwnerLeafPolicy:    ownerLeafPolicy,
	}, nil
}

// encodeRecipientOutput encodes one recipient output as TLV records.
func encodeRecipientOutput(rec oorlib.RecipientOutput) ([]byte, error) {
	if rec.Value < 0 {
		return nil, fmt.Errorf("recipient value must be non-negative")
	}

	pkScript := rec.PkScript
	value := uint64(rec.Value)
	vtxoPolicyTemplate := rec.VTXOPolicyTemplate

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			recipientOutputPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			recipientOutputValueRecordType, &value,
		),
	}

	if len(vtxoPolicyTemplate) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			recipientOutputVTXOPolicyRecordType,
			&vtxoPolicyTemplate,
		))
	}

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

// decodeRecipientOutput decodes one recipient output from TLV records.
func decodeRecipientOutput(blob []byte) (oorlib.RecipientOutput, error) {
	var (
		pkScript           []byte
		value              uint64
		vtxoPolicyTemplate []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			recipientOutputPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			recipientOutputValueRecordType, &value,
		),
		tlv.MakePrimitiveRecord(
			recipientOutputVTXOPolicyRecordType,
			&vtxoPolicyTemplate,
		),
	)
	if err != nil {
		return oorlib.RecipientOutput{}, err
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(blob))
	if err != nil {
		return oorlib.RecipientOutput{}, err
	}

	if _, ok := parsed[recipientOutputPkScriptRecordType]; !ok {
		return oorlib.RecipientOutput{}, fmt.Errorf(
			"recipient pkScript must be provided",
		)
	}
	if _, ok := parsed[recipientOutputValueRecordType]; !ok {
		return oorlib.RecipientOutput{}, fmt.Errorf(
			"recipient value must be provided",
		)
	}

	return oorlib.RecipientOutput{
		PkScript:           pkScript,
		Value:              btcutil.Amount(value),
		VTXOPolicyTemplate: vtxoPolicyTemplate,
	}, nil
}

// encodeTLVByteList encodes a byte-slice list into canonical sequential TLV
// records (0..N-1).
func encodeTLVByteList(items [][]byte) ([]byte, error) {
	if len(items) == 0 {
		return nil, nil
	}

	itemsCopy := make([][]byte, len(items))
	records := make([]tlv.Record, 0, len(items))

	for i := range items {
		itemsCopy[i] = append([]byte(nil), items[i]...)
		records = append(records, tlv.MakePrimitiveRecord(
			tlv.Type(i), &itemsCopy[i],
		))
	}

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

// decodeTLVByteList decodes a TLV-encoded byte-slice list and enforces the
// canonical sequential record ordering.
func decodeTLVByteList(blob []byte) ([][]byte, error) {
	if len(blob) == 0 {
		return nil, nil
	}

	stream, err := tlv.NewStream()
	if err != nil {
		return nil, err
	}

	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}

	if len(parsed) == 0 {
		return nil, nil
	}

	types := make([]tlv.Type, 0, len(parsed))
	for typ := range parsed {
		types = append(types, typ)
	}

	sort.Slice(types, func(i, j int) bool {
		return types[i] < types[j]
	})

	items := make([][]byte, 0, len(types))
	for i, typ := range types {
		if typ != tlv.Type(i) {
			return nil, fmt.Errorf(
				"non-canonical list record type: %d", typ,
			)
		}

		item := append([]byte(nil), parsed[typ]...)
		items = append(items, item)
	}

	return items, nil
}
