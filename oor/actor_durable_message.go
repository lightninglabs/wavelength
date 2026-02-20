package oor

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// Top-level durable message types use the 0x7xxx range to avoid collision
// with inner record types that use small sequential values.
const (
	submitDurableTLVType   tlv.Type = 0x7001
	finalizeDurableTLVType tlv.Type = 0x7002

	submitArkPSBTRecordType            tlv.Type = 1
	submitCheckpointPSBTsRecordType    tlv.Type = 2
	submitSigningDescriptorsRecordType tlv.Type = 3

	finalizeSessionIDRecordType          tlv.Type = 1
	finalizeCheckpointPSBTsRecordType    tlv.Type = 2
	signingDescriptorOutpointRecordType  tlv.Type = 1
	signingDescriptorIndexRecordType     tlv.Type = 2
	signingDescriptorOwnerKeyRecordType  tlv.Type = 3
	signingDescriptorExitDelayRecordType tlv.Type = 4
)

// submitDurableMessage is the durable actor envelope for SubmitOORRequest.
//
// Fields are explicit (not embedded) so TLV encode/decode boundaries are
// unambiguous and inner record types don't leak through embedding.
type submitDurableMessage struct {
	actor.BaseMessage

	// ArkPSBT is the submitted Ark package transaction.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs is the candidate checkpoint set from submit.
	CheckpointPSBTs []*psbt.Packet

	// VTXOSigningDescriptors provides operator-signing metadata per VTXO.
	VTXOSigningDescriptors []VTXOSigningDescriptor
}

// MessageType returns a stable runtime identifier for the durable submit
// envelope.
func (*submitDurableMessage) MessageType() string {
	return "oor.SubmitDurable"
}

// TLVType returns the top-level TLV type used for submit durable messages.
func (*submitDurableMessage) TLVType() tlv.Type {
	return submitDurableTLVType
}

// Encode serializes the submit durable payload as canonical TLV records.
func (m *submitDurableMessage) Encode(w io.Writer) error {
	if m.ArkPSBT == nil {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkPSBTBytes, err := serializePSBT(m.ArkPSBT)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := serializePSBTList(m.CheckpointPSBTs)
	if err != nil {
		return err
	}

	checkpointPSBTsBlob, err := encodeTLVByteList(checkpointPSBTs)
	if err != nil {
		return err
	}

	signingDescs := make([][]byte, 0, len(m.VTXOSigningDescriptors))
	for i, desc := range m.VTXOSigningDescriptors {
		descBlob, err := encodeSigningDescriptor(desc)
		if err != nil {
			return fmt.Errorf(
				"encode signing descriptor %d: %w", i, err,
			)
		}

		signingDescs = append(signingDescs, descBlob)
	}

	signingDescsBlob, err := encodeTLVByteList(signingDescs)
	if err != nil {
		return err
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(submitArkPSBTRecordType, &arkPSBTBytes),
		tlv.MakePrimitiveRecord(
			submitCheckpointPSBTsRecordType, &checkpointPSBTsBlob,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType, &signingDescsBlob,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode parses and validates a submit durable payload from TLV bytes.
func (m *submitDurableMessage) Decode(r io.Reader) error {
	var (
		arkPSBTBytes       []byte
		checkpointPSBTsTLV []byte
		signingDescsTLV    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(submitArkPSBTRecordType, &arkPSBTBytes),
		tlv.MakePrimitiveRecord(
			submitCheckpointPSBTsRecordType, &checkpointPSBTsTLV,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType, &signingDescsTLV,
		),
	)
	if err != nil {
		return err
	}

	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := parsed[submitArkPSBTRecordType]; !ok {
		return fmt.Errorf("ark psbt must be provided")
	}

	arkPSBT, err := deserializePSBT(arkPSBTBytes)
	if err != nil {
		return err
	}

	checkpointPSBTBytes, err := decodeTLVByteList(checkpointPSBTsTLV)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := deserializePSBTList(checkpointPSBTBytes)
	if err != nil {
		return err
	}

	signingDescBytes, err := decodeTLVByteList(signingDescsTLV)
	if err != nil {
		return err
	}

	signingDescs := make(
		[]VTXOSigningDescriptor, 0, len(signingDescBytes),
	)
	for i, descBlob := range signingDescBytes {
		desc, err := decodeSigningDescriptor(descBlob)
		if err != nil {
			return fmt.Errorf(
				"decode signing descriptor %d: %w", i, err,
			)
		}

		signingDescs = append(signingDescs, desc)
	}

	m.ArkPSBT = arkPSBT
	m.CheckpointPSBTs = checkpointPSBTs
	m.VTXOSigningDescriptors = signingDescs

	return nil
}

// finalizeDurableMessage is the durable actor envelope for FinalizeOORRequest.
type finalizeDurableMessage struct {
	actor.BaseMessage

	// SessionID identifies the session being finalized.
	SessionID SessionID

	// FinalCheckpointPSBTs is the finalized checkpoint set from the client.
	FinalCheckpointPSBTs []*psbt.Packet
}

// MessageType returns a stable runtime identifier for the durable finalize
// envelope.
func (*finalizeDurableMessage) MessageType() string {
	return "oor.FinalizeDurable"
}

// TLVType returns the top-level TLV type used for finalize durable messages.
func (*finalizeDurableMessage) TLVType() tlv.Type {
	return finalizeDurableTLVType
}

// Encode serializes the finalize durable payload as canonical TLV records.
func (m *finalizeDurableMessage) Encode(w io.Writer) error {
	sessionID := [chainhash.HashSize]byte(m.SessionID)

	finalCheckpointPSBTs, err := serializePSBTList(m.FinalCheckpointPSBTs)
	if err != nil {
		return err
	}

	finalCheckpointPSBTsBlob, err := encodeTLVByteList(finalCheckpointPSBTs)
	if err != nil {
		return err
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			finalizeSessionIDRecordType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			finalizeCheckpointPSBTsRecordType,
			&finalCheckpointPSBTsBlob,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode parses and validates a finalize durable payload from TLV bytes.
func (m *finalizeDurableMessage) Decode(r io.Reader) error {
	var (
		sessionID            [chainhash.HashSize]byte
		finalCheckpointPSBTs []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			finalizeSessionIDRecordType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			finalizeCheckpointPSBTsRecordType,
			&finalCheckpointPSBTs,
		),
	)
	if err != nil {
		return err
	}

	parsed, err := stream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := parsed[finalizeSessionIDRecordType]; !ok {
		return fmt.Errorf("session id must be provided")
	}

	checkpointPSBTBytes, err := decodeTLVByteList(finalCheckpointPSBTs)
	if err != nil {
		return err
	}

	checkpointPSBTs, err := deserializePSBTList(checkpointPSBTBytes)
	if err != nil {
		return err
	}

	var hash chainhash.Hash
	copy(hash[:], sessionID[:])

	m.SessionID = SessionID(hash)
	m.FinalCheckpointPSBTs = checkpointPSBTs

	return nil
}

// newSubmitDurableMessage adapts the public submit request into the durable
// actor envelope.
func newSubmitDurableMessage(req *SubmitOORRequest) *submitDurableMessage {
	if req == nil {
		return nil
	}

	return &submitDurableMessage{
		ArkPSBT:                req.ArkPSBT,
		CheckpointPSBTs:        req.CheckpointPSBTs,
		VTXOSigningDescriptors: req.VTXOSigningDescriptors,
	}
}

// newFinalizeDurableMessage adapts the public finalize request into the
// durable actor envelope.
func newFinalizeDurableMessage(
	req *FinalizeOORRequest) *finalizeDurableMessage {

	if req == nil {
		return nil
	}

	return &finalizeDurableMessage{
		SessionID:            req.SessionID,
		FinalCheckpointPSBTs: req.FinalCheckpointPSBTs,
	}
}

// newActorCodec builds the durable mailbox codec used by the coordinator.
func newActorCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(submitDurableTLVType, func() actor.TLVMessage {
		return &submitDurableMessage{}
	})
	codec.MustRegister(finalizeDurableTLVType, func() actor.TLVMessage {
		return &finalizeDurableMessage{}
	})
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})

	return codec
}

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
