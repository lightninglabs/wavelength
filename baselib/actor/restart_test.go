package actor

import (
	"context"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestRestartMessageType verifies the RestartMessage message type string.
func TestRestartMessageType(t *testing.T) {
	t.Parallel()

	msg := &RestartMessage{}
	require.Equal(t, "actor.Restart", msg.MessageType())
}

// TestRestartMessageTLVType verifies the TLV type identifier.
func TestRestartMessageTLVType(t *testing.T) {
	t.Parallel()

	msg := &RestartMessage{}
	require.Equal(t, RestartTLVType, msg.TLVType())
	require.Equal(t, tlv.Type(0xFFFE), msg.TLVType())
}

// TestRestartMessagePriority verifies restart messages have highest priority.
func TestRestartMessagePriority(t *testing.T) {
	t.Parallel()

	msg := &RestartMessage{}
	require.Equal(t, RestartPriority, msg.Priority())
	require.Equal(t, math.MaxInt32, msg.Priority())
}

// TestRestartMessageNilCheckpoint tests encoding/decoding with no checkpoint.
func TestRestartMessageNilCheckpoint(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	original := &RestartMessage{Checkpoint: fn.None[Checkpoint]()}

	// Encode.
	data, err := codec.Encode(original)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Decode.
	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	msg, ok := decoded.(*RestartMessage)
	require.True(t, ok)
	require.True(t, msg.Checkpoint.IsNone())
	require.False(t, msg.HasCheckpoint())
}

// TestRestartMessageWithCheckpoint tests encoding/decoding with a checkpoint.
func TestRestartMessageWithCheckpoint(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	now := time.Now().Truncate(time.Second) // Truncate for comparison.
	originalCheckpoint := Checkpoint{
		ActorID:   "test-actor-123",
		StateType: "round.WaitingForNonces",
		StateData: []byte{
			0x01,
			0x02,
			0x03,
			0x04,
			0x05,
		},
		Version:   42,
		UpdatedAt: now,
	}
	original := &RestartMessage{
		Checkpoint: fn.Some(originalCheckpoint),
	}

	// Encode.
	data, err := codec.Encode(original)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Decode.
	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	msg, ok := decoded.(*RestartMessage)
	require.True(t, ok)
	require.True(t, msg.Checkpoint.IsSome())
	require.True(t, msg.HasCheckpoint())

	// Verify checkpoint fields.
	decodedCheckpoint := msg.Checkpoint.UnwrapOrFail(t)
	require.Equal(t, originalCheckpoint.ActorID, decodedCheckpoint.ActorID)
	require.Equal(
		t, originalCheckpoint.StateType, decodedCheckpoint.StateType,
	)
	require.Equal(
		t, originalCheckpoint.StateData, decodedCheckpoint.StateData,
	)
	require.Equal(t, originalCheckpoint.Version, decodedCheckpoint.Version)
	require.Equal(
		t, originalCheckpoint.UpdatedAt, decodedCheckpoint.UpdatedAt,
	)
}

// TestRestartMessageRapidRoundTrip is a property-based test for RestartMessage.
func TestRestartMessageRapidRoundTrip(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	rapid.Check(t, func(rt *rapid.T) {
		hasCheckpoint := rapid.Bool().Draw(rt, "hasCheckpoint")

		var original *RestartMessage
		if hasCheckpoint {
			actorID := rapid.String().Draw(rt, "actorID")
			stateType := rapid.String().Draw(rt, "stateType")
			stateData := rapid.SliceOf(rapid.Byte()).Draw(
				rt, "stateData",
			)
			version := rapid.Int64Min(0).Draw(rt, "version")
			updatedAt := rapid.Int64Range(0, 1<<40).Draw(
				rt, "updatedAt",
			)

			original = &RestartMessage{
				Checkpoint: fn.Some(Checkpoint{
					ActorID:   actorID,
					StateType: stateType,
					StateData: stateData,
					Version:   version,
					UpdatedAt: time.Unix(updatedAt, 0),
				}),
			}
		} else {
			original = &RestartMessage{
				Checkpoint: fn.None[Checkpoint](),
			}
		}

		// Encode.
		data, err := codec.Encode(original)
		require.NoError(t, err)

		// Decode.
		decoded, err := codec.Decode(data)
		require.NoError(t, err)

		msg := decoded.(*RestartMessage)

		// Verify.
		if hasCheckpoint {
			require.True(t, msg.Checkpoint.IsSome())

			origCP := original.Checkpoint.UnsafeFromSome()
			decodedCP := msg.Checkpoint.UnsafeFromSome()
			require.Equal(t, origCP.ActorID, decodedCP.ActorID)
			require.Equal(t, origCP.StateType, decodedCP.StateType)
			require.Equal(t, origCP.StateData, decodedCP.StateData)
			require.Equal(t, origCP.Version, decodedCP.Version)
			require.Equal(t, origCP.UpdatedAt, decodedCP.UpdatedAt)
		} else {
			require.True(t, msg.Checkpoint.IsNone())
		}
	})
}

// TestIsRestartMessage tests the IsRestartMessage helper function.
func TestIsRestartMessage(t *testing.T) {
	t.Parallel()

	// RestartMessage should return true.
	restartMsg := &RestartMessage{}
	require.True(t, IsRestartMessage(restartMsg))

	// Other message types should return false.
	otherMsg := &testTLVMsg{}
	require.False(t, IsRestartMessage(otherMsg))
}

// TestPrependRestartMessage tests enqueueing a restart message.
func TestPrependRestartMessage(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	checkpoint := &Checkpoint{
		ActorID:   "test-actor",
		StateType: "InitialState",
		StateData: []byte{
			0xAB,
			0xCD,
		},
		Version:   1,
		UpdatedAt: time.Now().Truncate(time.Second),
	}

	ctx := context.Background()
	err := PrependRestartMessage(
		ctx, store, codec, "test-actor", checkpoint,
	)
	require.NoError(t, err)

	// Verify message was enqueued.
	require.Len(t, store.messages, 1)

	msg := slices.Collect(maps.Values(store.messages))[0]
	require.Equal(t, "test-actor", msg.MailboxID)
	require.Equal(t, "actor.Restart", msg.MessageType)
	require.Equal(t, RestartPriority, msg.Priority)
	require.Equal(t, 1, msg.MaxAttempts)
	require.NotEmpty(t, msg.Payload)

	// Decode and verify checkpoint.
	decoded, err := codec.Decode(msg.Payload)
	require.NoError(t, err)

	restartMsg := decoded.(*RestartMessage)
	decodedCP := restartMsg.Checkpoint.UnwrapOrFail(t)
	require.Equal(t, checkpoint.ActorID, decodedCP.ActorID)
	require.Equal(t, checkpoint.StateType, decodedCP.StateType)
	require.Equal(t, checkpoint.StateData, decodedCP.StateData)
	require.Equal(t, checkpoint.Version, decodedCP.Version)
}

// TestPrependRestartMessageNilCheckpoint tests enqueueing without a checkpoint.
func TestPrependRestartMessageNilCheckpoint(t *testing.T) {
	t.Parallel()

	store := newMockDeliveryStore()
	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	ctx := context.Background()
	err := PrependRestartMessage(ctx, store, codec, "new-actor", nil)
	require.NoError(t, err)

	// Verify message was enqueued.
	require.Len(t, store.messages, 1)

	msg := slices.Collect(maps.Values(store.messages))[0]
	require.Equal(t, "new-actor", msg.MailboxID)
	require.Equal(t, RestartPriority, msg.Priority)

	// Decode and verify no checkpoint.
	decoded, err := codec.Decode(msg.Payload)
	require.NoError(t, err)

	restartMsg := decoded.(*RestartMessage)
	require.True(t, restartMsg.Checkpoint.IsNone())
}

// TestRestartMessageEmptyStateData tests checkpoint with empty state data.
func TestRestartMessageEmptyStateData(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	original := &RestartMessage{
		Checkpoint: fn.Some(Checkpoint{
			ActorID:   "actor-with-empty-state",
			StateType: "EmptyState",
			StateData: []byte{}, // Empty but not nil.
			Version:   0,
			UpdatedAt: time.Unix(0, 0),
		}),
	}

	data, err := codec.Encode(original)
	require.NoError(t, err)

	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	msg := decoded.(*RestartMessage)
	decodedCP := msg.Checkpoint.UnwrapOrFail(t)
	require.Equal(t, "actor-with-empty-state", decodedCP.ActorID)
	require.Equal(t, "EmptyState", decodedCP.StateType)
	require.Empty(t, decodedCP.StateData)
}

// TestRestartMessageLargeStateData tests checkpoint with large state data.
func TestRestartMessageLargeStateData(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(RestartTLVType, func() TLVMessage {
		return &RestartMessage{}
	})

	// Create 1MB of state data.
	largeData := make([]byte, 1024*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	original := &RestartMessage{
		Checkpoint: fn.Some(Checkpoint{
			ActorID:   "large-state-actor",
			StateType: "LargeState",
			StateData: largeData,
			Version:   999,
			UpdatedAt: time.Now().Truncate(time.Second),
		}),
	}

	data, err := codec.Encode(original)
	require.NoError(t, err)

	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	msg := decoded.(*RestartMessage)
	decodedCP := msg.Checkpoint.UnwrapOrFail(t)
	require.Equal(t, largeData, decodedCP.StateData)
}

// TestRestartMessageInterfaceCompliance verifies interface implementations.
func TestRestartMessageInterfaceCompliance(t *testing.T) {
	t.Parallel()

	msg := &RestartMessage{}

	// Should implement TLVMessage.
	var _ TLVMessage = msg

	// Should implement PriorityMessage.
	var _ PriorityMessage = msg

	// Should implement Message.
	var _ Message = msg
}
