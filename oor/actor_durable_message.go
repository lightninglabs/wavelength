package oor

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV record type constants for signing descriptor fields. These are used
// by encodeSigningDescriptor/decodeSigningDescriptor which serialize
// VTXOSigningDescriptor values within actor message TLV streams.
const (
	signingDescriptorOutpointRecordType  tlv.Type = 1
	signingDescriptorIndexRecordType     tlv.Type = 2
	signingDescriptorOwnerKeyRecordType  tlv.Type = 3
	signingDescriptorExitDelayRecordType tlv.Type = 4
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
	exitDelay := desc.ExitDelay
	ownerKey := []byte(nil)
	if desc.OwnerKey != nil {
		ownerKey = desc.OwnerKey.SerializeCompressed()
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			signingDescriptorOutpointRecordType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorIndexRecordType, &outpointIndex,
		),
	}

	if len(ownerKey) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			signingDescriptorOwnerKeyRecordType, &ownerKey,
		))
	}

	records = append(records, tlv.MakePrimitiveRecord(
		signingDescriptorExitDelayRecordType, &exitDelay,
	))

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
		outpointHash  [chainhash.HashSize]byte
		outpointIndex uint32
		ownerKey      []byte
		exitDelay     uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			signingDescriptorOutpointRecordType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorIndexRecordType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorOwnerKeyRecordType, &ownerKey,
		),
		tlv.MakePrimitiveRecord(
			signingDescriptorExitDelayRecordType, &exitDelay,
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
	if _, ok := parsed[signingDescriptorExitDelayRecordType]; !ok {
		return VTXOSigningDescriptor{}, fmt.Errorf(
			"exit delay must be provided",
		)
	}

	var ownerPubKey *btcec.PublicKey
	if len(ownerKey) > 0 {
		ownerPubKey, err = btcec.ParsePubKey(ownerKey)
		if err != nil {
			return VTXOSigningDescriptor{}, err
		}
	}

	var hash chainhash.Hash
	copy(hash[:], outpointHash[:])

	return VTXOSigningDescriptor{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: outpointIndex,
		},
		OwnerKey:  ownerPubKey,
		ExitDelay: exitDelay,
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
