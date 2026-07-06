package chainsource

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/stretchr/testify/require"
)

// mockTargetMessage is a test message type that callers might want to receive
// instead of the chainsource-specific events.
type mockTargetMessage struct {
	actor.BaseMessage
	Source string
	Data   string
}

// MessageType returns the message type identifier.
func (m mockTargetMessage) MessageType() string {
	return "mockTargetMessage"
}

// TestMapConfirmationEvent tests that MapConfirmationEvent correctly
// transforms ConfirmationEvent to a caller-specific message type.
func TestMapConfirmationEvent(t *testing.T) {
	t.Parallel()

	// Create a channel-based target ref that expects mockTargetMessage.
	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"test-target", 10,
	)

	// Create transformation function from ConfirmationEvent to
	// mockTargetMessage.
	transformFn := func(ce ConfirmationEvent) mockTargetMessage {
		return mockTargetMessage{
			Source: "confirmation",
			Data:   ce.Txid.String(),
		}
	}

	// Use the convenience helper to create the adapted ref.
	adaptedRef := MapConfirmationEvent(targetRef, transformFn)

	// Create a test ConfirmationEvent and send it.
	ctx := t.Context()
	testTxid, _ := chainhash.NewHashFromStr(
		"00000000000000000000000000000000000000000000000000000000000" +
			"00001",
	)
	confEvent := ConfirmationEvent{
		Txid:        *testTxid,
		BlockHeight: 100,
		BlockHash:   chainhash.Hash{},
		NumConfs:    6,
	}

	require.NoError(t, adaptedRef.Tell(ctx, confEvent))

	// Verify the target received the transformed message.
	received, ok := targetRef.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for message")
	require.Equal(t, "confirmation", received.Source)
	require.Equal(t, testTxid.String(), received.Data)
}

// TestMapSpendEvent tests that MapSpendEvent correctly transforms SpendEvent
// to a caller-specific message type.
func TestMapSpendEvent(t *testing.T) {
	t.Parallel()

	// Create a channel-based target ref that expects mockTargetMessage.
	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"test-target", 10,
	)

	// Create transformation function from SpendEvent to mockTargetMessage.
	transformFn := func(se SpendEvent) mockTargetMessage {
		return mockTargetMessage{
			Source: "spend",
			Data:   se.SpendingTxid.String(),
		}
	}

	// Use the convenience helper to create the adapted ref.
	adaptedRef := MapSpendEvent(targetRef, transformFn)

	// Create a test SpendEvent and send it.
	ctx := t.Context()
	testOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 0,
	}
	testSpendingTxid, _ := chainhash.NewHashFromStr(
		"00000000000000000000000000000000000000000000000000000000000" +
			"00002",
	)
	spendEvent := SpendEvent{
		Outpoint:          testOutpoint,
		SpendingTxid:      *testSpendingTxid,
		SpendingTx:        &wire.MsgTx{},
		SpenderInputIndex: 0,
		SpendingHeight:    200,
	}

	require.NoError(t, adaptedRef.Tell(ctx, spendEvent))

	// Verify the target received the transformed message.
	received, ok := targetRef.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for message")
	require.Equal(t, "spend", received.Source)
	require.Equal(t, testSpendingTxid.String(), received.Data)
}

// TestMapBlockEpoch tests that MapBlockEpoch correctly transforms BlockEpoch
// to a caller-specific message type.
func TestMapBlockEpoch(t *testing.T) {
	t.Parallel()

	// Create a channel-based target ref that expects mockTargetMessage.
	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"test-target", 10,
	)

	// Create transformation function from BlockEpoch to mockTargetMessage.
	transformFn := func(be BlockEpoch) mockTargetMessage {
		return mockTargetMessage{
			Source: "block",
			Data:   be.Hash.String(),
		}
	}

	// Use the convenience helper to create the adapted ref.
	adaptedRef := MapBlockEpoch(targetRef, transformFn)

	// Create a test BlockEpoch and send it.
	ctx := t.Context()
	testBlockHash, _ := chainhash.NewHashFromStr(
		"00000000000000000000000000000000000000000000000000000000000" +
			"00003",
	)
	blockEpoch := BlockEpoch{
		Height:    300,
		Hash:      *testBlockHash,
		Timestamp: time.Now().Unix(),
	}

	require.NoError(t, adaptedRef.Tell(ctx, blockEpoch))

	// Verify the target received the transformed message.
	received, ok := targetRef.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for message")
	require.Equal(t, "block", received.Source)
	require.Equal(t, testBlockHash.String(), received.Data)
}

// TestMapConfirmationEventTypeSafety tests that the type system ensures
// compile-time safety when using MapConfirmationEvent.
func TestMapConfirmationEventTypeSafety(t *testing.T) {
	t.Parallel()

	// This test verifies that the type system works correctly. If this
	// compiles, it proves type safety is maintained.
	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"test-target", 10,
	)

	// Create the adapted ref using the helper.
	mapFn := func(ce ConfirmationEvent) mockTargetMessage {
		return mockTargetMessage{
			Source: "confirmation",
			Data:   "test",
		}
	}
	adaptedRef := MapConfirmationEvent(targetRef, mapFn)

	// The fact that we can assign to TellOnlyRef[ConfirmationEvent] proves
	// the types are correct.
	ctx := t.Context()
	testTxid := chainhash.Hash{}
	require.NoError(
		t,
		adaptedRef.Tell(
			ctx, ConfirmationEvent{
				Txid:        testTxid,
				BlockHeight: 1,
			},
		),
	)

	// Verify the message was transformed and delivered.
	_, ok := targetRef.AwaitMessage(time.Second)
	require.True(t, ok, "timeout waiting for message")
}

// TestMapSpendEventMultipleMessages tests that MapSpendEvent correctly handles
// multiple sequential messages.
func TestMapSpendEventMultipleMessages(t *testing.T) {
	t.Parallel()

	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"test-target", 10,
	)

	counter := 0
	transformFn := func(se SpendEvent) mockTargetMessage {
		counter++

		return mockTargetMessage{
			Source: "spend",
			Data:   se.SpendingTxid.String(),
		}
	}

	adaptedRef := MapSpendEvent(targetRef, transformFn)

	ctx := t.Context()

	// Send multiple spend events.
	for i := 0; i < 3; i++ {
		spendEvent := SpendEvent{
			Outpoint: wire.OutPoint{
				Index: uint32(i),
			},
			SpendingTxid:      chainhash.Hash{},
			SpendingTx:        &wire.MsgTx{},
			SpenderInputIndex: uint32(i),
			SpendingHeight:    int32(100 + i),
		}
		require.NoError(t, adaptedRef.Tell(ctx, spendEvent))
	}

	// Verify all messages were transformed and delivered.
	for i := 0; i < 3; i++ {
		_, ok := targetRef.AwaitMessage(time.Second)
		require.True(t, ok, "timeout waiting for message %d", i)
	}
	require.Equal(t, 3, counter)
}

// TestMapBlockEpochID tests that the adapted ref has a proper ID.
func TestMapBlockEpochID(t *testing.T) {
	t.Parallel()

	targetRef := actor.NewChannelTellOnlyRef[mockTargetMessage](
		"my-block-consumer", 10,
	)

	transformFn := func(be BlockEpoch) mockTargetMessage {
		return mockTargetMessage{}
	}

	adaptedRef := MapBlockEpoch(targetRef, transformFn)

	// The ID should include the map-input prefix.
	require.Contains(t, adaptedRef.ID(), "map-input")
	require.Contains(t, adaptedRef.ID(), "my-block-consumer")
}
