package actor

import (
	"context"
	"io"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// RestartTLVType is the TLV type identifier for RestartMessage.
// Uses a high value to avoid conflicts with application message types.
//
// Note that AskResponseMsgType uses 0xFFFF, so RestartTLVType must be
// different.
const RestartTLVType tlv.Type = 0xFFFE

// RestartPriority is the priority level for restart messages.
// Uses math.MaxInt32 to ensure restart messages are processed first.
const RestartPriority = math.MaxInt32

// RestartMessage is a special message sent to an actor when it starts up and
// has a persisted checkpoint. This allows the actor to restore its FSM state
// and continue processing from where it left off after a crash.
//
// The RestartMessage is always placed at the front of the mailbox (highest
// priority) to ensure it is processed before any other pending messages.
//
// If Checkpoint is None, this indicates a fresh start with no prior state.
// Actors can use this to perform any initialization logic.
type RestartMessage struct {
	BaseMessage

	// Checkpoint contains the persisted FSM state to restore from.
	// None indicates a fresh start with no prior state.
	Checkpoint fn.Option[Checkpoint]
}

// MessageType returns the type name for this message.
func (RestartMessage) MessageType() string {
	return "actor.Restart"
}

// TLVType returns the TLV type identifier for RestartMessage.
func (RestartMessage) TLVType() tlv.Type {
	return RestartTLVType
}

// Encode serializes the RestartMessage as a TLV stream. If no checkpoint is
// present, an empty stream is written. Otherwise, all checkpoint fields are
// encoded using odd TLV types (1, 3, 5, 7, 9).
func (m *RestartMessage) Encode(w io.Writer) error {
	// No checkpoint = empty payload (no TLV records to encode).
	if m.Checkpoint.IsNone() {
		return nil
	}

	// Extract checkpoint fields for encoding.
	cp := m.Checkpoint.UnsafeFromSome()

	actorIDBytes := []byte(cp.ActorID)
	stateTypeBytes := []byte(cp.StateType)
	version := uint64(cp.Version)
	updatedAt := uint64(cp.UpdatedAt.Unix())

	// Build TLV records. All fields use odd types to signal optional.
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(1, &actorIDBytes),
		tlv.MakePrimitiveRecord(3, &stateTypeBytes),
		tlv.MakePrimitiveRecord(5, &cp.StateData),
		tlv.MakePrimitiveRecord(7, &version),
		tlv.MakePrimitiveRecord(9, &updatedAt),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the RestartMessage. Creates local
// RecordT variables for decoding, then checks the typeMap to determine if
// checkpoint data was present in the stream.
func (m *RestartMessage) Decode(r io.Reader) error {
	// Create local ZeroRecordT variables for the decoder to write into.
	var (
		actorID   = tlv.ZeroRecordT[tlv.TlvType1, []byte]()
		stateType = tlv.ZeroRecordT[tlv.TlvType3, []byte]()
		stateData = tlv.ZeroRecordT[tlv.TlvType5, []byte]()
		version   = tlv.ZeroRecordT[tlv.TlvType7, uint64]()
		updatedAt = tlv.ZeroRecordT[tlv.TlvType9, uint64]()
	)

	// Build stream with pointers to local variables.
	stream, err := tlv.NewStream(
		actorID.Record(), stateType.Record(), stateData.Record(),
		version.Record(), updatedAt.Record(),
	)
	if err != nil {
		return err
	}

	// Bound the framing before decoding so a crafted durable checkpoint
	// payload cannot drive an unbounded make() inside the tlv decoder
	// (actorID/stateType/stateData are []byte records sized from their
	// declared lengths).
	safe, err := safeActorTLVReader(r)
	if err != nil {
		return err
	}

	// Decode and get typeMap showing which fields were present.
	typeMap, err := stream.DecodeWithParsedTypes(safe)
	if err != nil {
		return err
	}

	// Check if checkpoint data was present by looking for actorID field.
	if _, ok := typeMap[actorID.TlvType()]; !ok {
		// No checkpoint data present - this is a fresh start.
		m.Checkpoint = fn.None[Checkpoint]()

		return nil
	}

	// Checkpoint data is present. Reconstruct from decoded values.
	m.Checkpoint = fn.Some(Checkpoint{
		ActorID:   string(actorID.Val),
		StateType: string(stateType.Val),
		StateData: stateData.Val,
		Version:   int64(version.Val),
		UpdatedAt: time.Unix(int64(updatedAt.Val), 0),
	})

	return nil
}

// Priority returns the processing priority for restart messages.
// This ensures restart messages are always processed first.
func (RestartMessage) Priority() int {
	return RestartPriority
}

// HasCheckpoint returns true if this restart message contains a checkpoint.
func (m *RestartMessage) HasCheckpoint() bool {
	return m.Checkpoint.IsSome()
}

// PrependRestartMessage enqueues a restart message at the front of an actor's
// mailbox. The message has the highest possible priority to ensure it is
// processed before any other pending messages.
//
// This function should be called during actor startup after loading the
// checkpoint from the database. Even if no checkpoint exists, a RestartMessage
// with None Checkpoint can be sent to signal actor initialization.
func PrependRestartMessage(
	ctx context.Context,
	store DeliveryStore,
	codec *MessageCodec,
	mailboxID string,
	checkpoint *Checkpoint,
) error {

	msg := &RestartMessage{
		Checkpoint: fn.OptionFromPtr(checkpoint),
	}

	// Encode the message.
	payload, err := codec.Encode(msg)
	if err != nil {
		return err
	}

	// Generate a UUID v7 for the message (time-ordered, RFC 9562).
	id := uuid.Must(uuid.NewV7()).String()

	// Enqueue with highest priority to ensure front-of-queue processing.
	return store.EnqueueMessage(ctx, EnqueueParams{
		ID:          id,
		MailboxID:   mailboxID,
		MessageType: msg.MessageType(),
		Payload:     payload,
		Priority:    RestartPriority,
		// Use epoch so restart delivery never depends on wall-clock
		// skew versus a test/fake delivery-store clock.
		AvailableAt: time.Unix(0, 0),
		// Restart message should only be delivered once.
		MaxAttempts: 1,
	})
}

// IsRestartMessage returns true if the message is a RestartMessage.
func IsRestartMessage(msg Message) bool {
	_, ok := msg.(*RestartMessage)

	return ok
}

// Compile-time interface checks.
var (
	_ TLVMessage      = (*RestartMessage)(nil)
	_ PriorityMessage = (*RestartMessage)(nil)
)
