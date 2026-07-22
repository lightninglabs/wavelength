package actortest

import (
	"io"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type constants for counter messages. These are stable identifiers used
// for message serialization and dispatch.
const (
	IncrementMsgType tlv.Type = 1000
	DecrementMsgType tlv.Type = 1001
	GetCountMsgType  tlv.Type = 1002
	ForwardMsgType   tlv.Type = 1003
)

// TLV record type constants for fields within messages.
const (
	amountRecordType  tlv.Type = 1
	targetRecordType  tlv.Type = 2
	msgTypeRecordType tlv.Type = 3
	payloadRecordType tlv.Type = 4
)

// CounterMessage is the base interface for all counter-related messages.
// All counter messages implement TLVMessage for durable serialization.
type CounterMessage interface {
	actor.TLVMessage
}

// CounterResult is the response type for Ask messages to the counter.
type CounterResult = int64

// IncrementMsg is a Tell message that increments the counter by a given amount.
type IncrementMsg struct {
	actor.BaseMessage

	Amount int64
}

// MessageType returns a human-readable type name for logging.
func (m *IncrementMsg) MessageType() string {
	return "counter.Increment"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *IncrementMsg) TLVType() tlv.Type {
	return IncrementMsgType
}

// Encode serializes the message to the provided writer.
func (m *IncrementMsg) Encode(w io.Writer) error {
	// TLV MakePrimitiveRecord requires uint64, not int64.
	amount := uint64(m.Amount)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(amountRecordType, &amount),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *IncrementMsg) Decode(r io.Reader) error {
	var amount uint64

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(amountRecordType, &amount),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Bound the framing before decoding. Even though this message only
	// holds a fixed-width scalar, the tlv decoder's unknown-record path
	// allocates make([]byte, declaredLength) for any unrecognized odd
	// type, so a crafted payload with a huge unknown-record length would
	// otherwise panic the decoder.
	safe, err := safeCounterTLVReader(r)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(safe); err != nil {
		return err
	}

	m.Amount = int64(amount)

	return nil
}

// DecrementMsg is a Tell message that decrements the counter by a given amount.
type DecrementMsg struct {
	actor.BaseMessage

	Amount int64
}

// MessageType returns a human-readable type name for logging.
func (m *DecrementMsg) MessageType() string {
	return "counter.Decrement"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *DecrementMsg) TLVType() tlv.Type {
	return DecrementMsgType
}

// Encode serializes the message to the provided writer.
func (m *DecrementMsg) Encode(w io.Writer) error {
	// TLV MakePrimitiveRecord requires uint64, not int64.
	amount := uint64(m.Amount)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(amountRecordType, &amount),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *DecrementMsg) Decode(r io.Reader) error {
	var amount uint64

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(amountRecordType, &amount),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Bound the framing before decoding so a crafted unknown-record length
	// cannot drive an unbounded make() inside the tlv decoder (see
	// IncrementMsg.Decode for the unknown-record-path rationale).
	safe, err := safeCounterTLVReader(r)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(safe); err != nil {
		return err
	}

	m.Amount = int64(amount)

	return nil
}

// GetCountMsg is an Ask message that retrieves the current counter value.
type GetCountMsg struct {
	actor.BaseMessage
}

// MessageType returns a human-readable type name for logging.
func (m *GetCountMsg) MessageType() string {
	return "counter.GetCount"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *GetCountMsg) TLVType() tlv.Type {
	return GetCountMsgType
}

// Encode serializes the message to the provided writer.
// GetCountMsg has no fields, so this is a no-op.
func (m *GetCountMsg) Encode(w io.Writer) error {
	return nil
}

// Decode deserializes the message from the provided reader.
// GetCountMsg has no fields, so this is a no-op.
func (m *GetCountMsg) Decode(r io.Reader) error {
	return nil
}

// ForwardMsg is a Tell message that forwards another message to a target actor.
// This exercises the outbox pattern for inter-actor communication.
type ForwardMsg struct {
	actor.BaseMessage

	Target  string
	MsgType tlv.Type
	Payload []byte
}

// MessageType returns a human-readable type name for logging.
func (m *ForwardMsg) MessageType() string {
	return "counter.Forward"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *ForwardMsg) TLVType() tlv.Type {
	return ForwardMsgType
}

// Encode serializes the message to the provided writer.
func (m *ForwardMsg) Encode(w io.Writer) error {
	target := []byte(m.Target)
	msgType := uint64(m.MsgType)
	payload := m.Payload

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(targetRecordType, &target),
		tlv.MakePrimitiveRecord(msgTypeRecordType, &msgType),
		tlv.MakePrimitiveRecord(payloadRecordType, &payload),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *ForwardMsg) Decode(r io.Reader) error {
	var (
		target  []byte
		msgType uint64
		payload []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(targetRecordType, &target),
		tlv.MakePrimitiveRecord(msgTypeRecordType, &msgType),
		tlv.MakePrimitiveRecord(payloadRecordType, &payload),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Bound the framing before decoding so a crafted durable payload cannot
	// drive an unbounded make() inside the tlv decoder (target and payload
	// are []byte records sized from their declared lengths).
	safe, err := safeCounterTLVReader(r)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(safe); err != nil {
		return err
	}

	m.Target = string(target)
	m.MsgType = tlv.Type(msgType)
	m.Payload = payload

	return nil
}

// NewCounterCodec creates a MessageCodec with all counter message types
// registered.
func NewCounterCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(IncrementMsgType, func() actor.TLVMessage {
		return &IncrementMsg{}
	})

	codec.MustRegister(DecrementMsgType, func() actor.TLVMessage {
		return &DecrementMsg{}
	})

	codec.MustRegister(GetCountMsgType, func() actor.TLVMessage {
		return &GetCountMsg{}
	})

	codec.MustRegister(ForwardMsgType, func() actor.TLVMessage {
		return &ForwardMsg{}
	})

	// Register AskResponse for DurableAsk support.
	codec.MustRegister(actor.AskResponseMsgType, func() actor.TLVMessage {
		return &actor.AskResponse{}
	})

	return codec
}

// Compile-time interface checks.
var (
	_ CounterMessage = (*IncrementMsg)(nil)
	_ CounterMessage = (*DecrementMsg)(nil)
	_ CounterMessage = (*GetCountMsg)(nil)
	_ CounterMessage = (*ForwardMsg)(nil)
)
