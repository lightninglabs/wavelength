package actor

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/lightningnetwork/lnd/tlv"
)

// TLVMessage extends Message with TLV serialization capability. Messages that
// need to be persisted to a durable mailbox must implement this interface.
// The TLV format provides efficient, backward-compatible binary encoding.
//
// Each message type handles its own encoding/decoding logic, giving full
// control over optional fields and TLV record management.
type TLVMessage interface {
	Message

	// TLVType returns a unique type identifier for this message. This ID is
	// used by the MessageCodec registry to dispatch deserialization to the
	// correct message constructor. The type should be stable across
	// versions for backward compatibility.
	TLVType() tlv.Type

	// Encode serializes the message to the provided writer as a TLV stream.
	// Implementations should only encode records that have meaningful
	// values, omitting optional fields when not set.
	Encode(w io.Writer) error

	// Decode deserializes a TLV stream from the reader into the message.
	// Implementations should create local RecordT variables for optional
	// fields, pass pointers to the stream, then check the typeMap to
	// determine which fields were actually present.
	Decode(r io.Reader) error
}

// MessageConstructor is a function that creates a new empty instance of a
// TLVMessage type. Used by MessageCodec for deserialization dispatch.
type MessageConstructor func() TLVMessage

// MessageCodec handles serialization and deserialization of TLVMessage types.
// Each actor can have its own codec with only the message types it handles,
// providing type isolation and preventing global state.
type MessageCodec struct {
	mu       sync.RWMutex
	registry map[tlv.Type]MessageConstructor
}

// NewMessageCodec creates a new empty message codec.
func NewMessageCodec() *MessageCodec {
	return &MessageCodec{
		registry: make(map[tlv.Type]MessageConstructor),
	}
}

// Register adds a message type to the codec registry. The constructor should
// return a new empty instance of the message type. Returns an error if the
// type ID is already registered.
func (c *MessageCodec) Register(typeID tlv.Type,
	constructor MessageConstructor) error {

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.registry[typeID]; exists {
		return fmt.Errorf("tlv type %d already registered", typeID)
	}

	c.registry[typeID] = constructor

	return nil
}

// MustRegister is like Register but panics on error. Useful for init-time
// registration where errors should be caught early.
func (c *MessageCodec) MustRegister(typeID tlv.Type,
	constructor MessageConstructor) {

	if err := c.Register(typeID, constructor); err != nil {
		panic(err)
	}
}

// Encode serializes a TLVMessage to bytes. The format is:
// [type_id (BigSize)][payload_length (BigSize)][tlv_stream...]
func (c *MessageCodec) Encode(msg TLVMessage) ([]byte, error) {
	var buf bytes.Buffer

	// Write the message type ID.
	typeID := msg.TLVType()
	if err := tlv.WriteVarInt(
		&buf, uint64(typeID), &[8]byte{},
	); err != nil {
		return nil, fmt.Errorf("write type id: %w", err)
	}

	// Encode the message to a temporary buffer.
	var payloadBuf bytes.Buffer
	if err := msg.Encode(&payloadBuf); err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}

	// Write the payload length.
	if err := tlv.WriteVarInt(
		&buf,
		uint64(
			payloadBuf.Len(),
		),
		&[8]byte{},
	); err != nil {
		return nil, fmt.Errorf("write payload length: %w", err)
	}

	// Write the payload.
	if _, err := buf.Write(payloadBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("write payload: %w", err)
	}

	return buf.Bytes(), nil
}

// Decode deserializes bytes to a TLVMessage. Returns an error if the type ID
// is not registered or if decoding fails.
func (c *MessageCodec) Decode(data []byte) (TLVMessage, error) {
	r := bytes.NewReader(data)

	// Read the message type ID.
	typeID, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return nil, fmt.Errorf("read type id: %w", err)
	}

	// Look up the constructor.
	c.mu.RLock()
	constructor, exists := c.registry[tlv.Type(typeID)]
	c.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("unknown message type: %d", typeID)
	}

	// Read the payload length.
	payloadLen, err := tlv.ReadVarInt(r, &[8]byte{})
	if err != nil {
		return nil, fmt.Errorf("read payload length: %w", err)
	}

	// Reject a declared payload length that cannot be backed by the bytes
	// physically present before allocating make([]byte, payloadLen). The
	// envelope is sourced from the durable mailbox, so a corrupt or
	// malicious length near 2^64 would otherwise panic with "makeslice:
	// cap out of range" or drive an OOM here, before io.ReadFull ever runs.
	if payloadLen > uint64(r.Len()) {
		return nil, fmt.Errorf("payload length %d exceeds %d remaining "+
			"bytes", payloadLen, r.Len())
	}

	// Read the payload.
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}

	// Create a new message instance and decode the payload into it.
	msg := constructor()
	if err := msg.Decode(bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}

	return msg, nil
}

// IsRegistered returns true if the given type ID is registered.
func (c *MessageCodec) IsRegistered(typeID tlv.Type) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, exists := c.registry[typeID]

	return exists
}

// RegisteredTypes returns a slice of all registered type IDs.
func (c *MessageCodec) RegisteredTypes() []tlv.Type {
	c.mu.RLock()
	defer c.mu.RUnlock()

	types := make([]tlv.Type, 0, len(c.registry))
	for typeID := range c.registry {
		types = append(types, typeID)
	}

	return types
}
