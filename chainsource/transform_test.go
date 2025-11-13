package chainsource

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/actor"
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

// mockTellOnlyRef is a mock implementation of TellOnlyRef for testing.
type mockTellOnlyRef[M actor.Message] struct {
	id       string
	received []M
}

func (m *mockTellOnlyRef[M]) Tell(ctx context.Context, msg M) {
	m.received = append(m.received, msg)
}

func (m *mockTellOnlyRef[M]) ID() string {
	return m.id
}

// TestMapConfirmationEvent tests that MapConfirmationEvent correctly
// transforms ConfirmationEvent to a caller-specific message type.
func TestMapConfirmationEvent(t *testing.T) {
	t.Parallel()

	// Create a mock target ref that expects mockTargetMessage.
	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "test-target",
	}

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
	ctx := context.Background()
	testTxid, _ := chainhash.NewHashFromStr(
		"0000000000000000000000000000000000000000000000000000000000000001",
	)
	confEvent := ConfirmationEvent{
		Txid:        *testTxid,
		BlockHeight: 100,
		BlockHash:   chainhash.Hash{},
		NumConfs:    6,
	}

	adaptedRef.Tell(ctx, confEvent)

	// Verify the target received the transformed message.
	require.Len(t, targetRef.received, 1)
	received := targetRef.received[0]
	require.Equal(t, "confirmation", received.Source)
	require.Equal(t, testTxid.String(), received.Data)
}

// TestMapSpendEvent tests that MapSpendEvent correctly transforms SpendEvent
// to a caller-specific message type.
func TestMapSpendEvent(t *testing.T) {
	t.Parallel()

	// Create a mock target ref that expects mockTargetMessage.
	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "test-target",
	}

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
	ctx := context.Background()
	testOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 0,
	}
	testSpendingTxid, _ := chainhash.NewHashFromStr(
		"0000000000000000000000000000000000000000000000000000000000000002",
	)
	spendEvent := SpendEvent{
		Outpoint:          testOutpoint,
		SpendingTxid:      *testSpendingTxid,
		SpendingTx:        &wire.MsgTx{},
		SpenderInputIndex: 0,
		SpendingHeight:    200,
	}

	adaptedRef.Tell(ctx, spendEvent)

	// Verify the target received the transformed message.
	require.Len(t, targetRef.received, 1)
	received := targetRef.received[0]
	require.Equal(t, "spend", received.Source)
	require.Equal(t, testSpendingTxid.String(), received.Data)
}

// TestMapBlockEpoch tests that MapBlockEpoch correctly transforms BlockEpoch
// to a caller-specific message type.
func TestMapBlockEpoch(t *testing.T) {
	t.Parallel()

	// Create a mock target ref that expects mockTargetMessage.
	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "test-target",
	}

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
	ctx := context.Background()
	testBlockHash, _ := chainhash.NewHashFromStr(
		"0000000000000000000000000000000000000000000000000000000000000003",
	)
	blockEpoch := BlockEpoch{
		Height:    300,
		Hash:      *testBlockHash,
		Timestamp: time.Now().Unix(),
	}

	adaptedRef.Tell(ctx, blockEpoch)

	// Verify the target received the transformed message.
	require.Len(t, targetRef.received, 1)
	received := targetRef.received[0]
	require.Equal(t, "block", received.Source)
	require.Equal(t, testBlockHash.String(), received.Data)
}

// TestMapConfirmationEventTypeSafety tests that the type system ensures
// compile-time safety when using MapConfirmationEvent.
func TestMapConfirmationEventTypeSafety(t *testing.T) {
	t.Parallel()

	// This test verifies that the type system works correctly. If this
	// compiles, it proves type safety is maintained.
	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "test-target",
	}

	// Create the adapted ref using the helper.
	var adaptedRef actor.TellOnlyRef[ConfirmationEvent] = MapConfirmationEvent(
		targetRef,
		func(ce ConfirmationEvent) mockTargetMessage {
			return mockTargetMessage{
				Source: "confirmation",
				Data:   "test",
			}
		},
	)

	// The fact that we can assign to TellOnlyRef[ConfirmationEvent] proves
	// the types are correct.
	ctx := context.Background()
	testTxid := chainhash.Hash{}
	adaptedRef.Tell(ctx, ConfirmationEvent{
		Txid:        testTxid,
		BlockHeight: 1,
	})

	// Verify the message was transformed and delivered.
	require.Len(t, targetRef.received, 1)
}

// TestMapSpendEventMultipleMessages tests that MapSpendEvent correctly handles
// multiple sequential messages.
func TestMapSpendEventMultipleMessages(t *testing.T) {
	t.Parallel()

	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "test-target",
	}

	counter := 0
	transformFn := func(se SpendEvent) mockTargetMessage {
		counter++
		return mockTargetMessage{
			Source: "spend",
			Data:   se.SpendingTxid.String(),
		}
	}

	adaptedRef := MapSpendEvent(targetRef, transformFn)

	ctx := context.Background()

	// Send multiple spend events.
	for i := 0; i < 3; i++ {
		spendEvent := SpendEvent{
			Outpoint:          wire.OutPoint{Index: uint32(i)},
			SpendingTxid:      chainhash.Hash{},
			SpendingTx:        &wire.MsgTx{},
			SpenderInputIndex: uint32(i),
			SpendingHeight:    int32(100 + i),
		}
		adaptedRef.Tell(ctx, spendEvent)
	}

	// Verify all messages were transformed and delivered.
	require.Len(t, targetRef.received, 3)
	require.Equal(t, 3, counter)
}

// TestMapBlockEpochID tests that the adapted ref has a proper ID.
func TestMapBlockEpochID(t *testing.T) {
	t.Parallel()

	targetRef := &mockTellOnlyRef[mockTargetMessage]{
		id: "my-block-consumer",
	}

	transformFn := func(be BlockEpoch) mockTargetMessage {
		return mockTargetMessage{}
	}

	adaptedRef := MapBlockEpoch(targetRef, transformFn)

	// The ID should include the map-input prefix.
	require.Contains(t, adaptedRef.ID(), "map-input")
	require.Contains(t, adaptedRef.ID(), "my-block-consumer")
}
