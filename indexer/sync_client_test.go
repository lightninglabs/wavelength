package indexer

import (
	"context"
	"testing"

	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// syncBackendCall records a backend call argument used in tests.
type syncBackendCall struct {
	after uint64
}

// testSyncBackend is a deterministic in-memory SyncBackend for
// tests.
type testSyncBackend struct {
	vtxoCalls []syncBackendCall
	oorCalls  []syncBackendCall

	vtxoResponses []*arkrpc.ListVTXOEventsByScriptsResponse
	oorResponses  []*arkrpc.ListOORRecipientEventsByScriptResponse
}

// ListVTXOEventsByScriptsTaproot records afterEventID and returns
// queued responses.
func (b *testSyncBackend) ListVTXOEventsByScriptsTaproot(_ context.Context,
	_ []TaprootScriptScope, afterEventID uint64, _ uint32,
	_ ...mailboxrpc.RPCOptions) (*arkrpc.ListVTXOEventsByScriptsResponse,
	error) {

	b.vtxoCalls = append(b.vtxoCalls, syncBackendCall{
		after: afterEventID,
	})

	if len(b.vtxoResponses) == 0 {
		return &arkrpc.ListVTXOEventsByScriptsResponse{}, nil
	}

	resp := b.vtxoResponses[0]
	b.vtxoResponses = b.vtxoResponses[1:]

	return resp, nil
}

// ListOORRecipientEventsByScriptTaproot records afterEventID and
// returns queued responses.
func (b *testSyncBackend) ListOORRecipientEventsByScriptTaproot(
	_ context.Context, _ []byte, afterEventID uint64, _ uint32,
	_ ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	b.oorCalls = append(b.oorCalls, syncBackendCall{
		after: afterEventID,
	})

	if len(b.oorResponses) == 0 {
		return &arkrpc.ListOORRecipientEventsByScriptResponse{},
			nil
	}

	resp := b.oorResponses[0]
	b.oorResponses = b.oorResponses[1:]

	return resp, nil
}

// TestMemorySyncCursorStoreMonotonic verifies monotonic cursor
// storage.
func TestMemorySyncCursorStoreMonotonic(t *testing.T) {
	t.Parallel()

	store := NewMemorySyncCursorStore()
	ctx := t.Context()

	err := store.SaveCursor(ctx, "n", "k", 11)
	require.NoError(t, err)

	err = store.SaveCursor(ctx, "n", "k", 9)
	require.NoError(t, err)

	cursor, err := store.LoadCursor(ctx, "n", "k")
	require.NoError(t, err)
	require.Equal(t, uint64(11), cursor)
}

// TestSyncClientVTXOEventCursorPersistence verifies the event cursor
// is loaded and advanced across successive polling calls when the
// caller invokes Ack.
func TestSyncClientVTXOEventCursorPersistence(t *testing.T) {
	t.Parallel()

	backend := &testSyncBackend{
		vtxoResponses: []*arkrpc.ListVTXOEventsByScriptsResponse{
			{
				Events: []*arkrpc.VTXOEvent{{
					EventId: 1,
				}},
				NextCursor: 1,
			},
			{
				Events:     nil,
				NextCursor: 1,
			},
		},
	}
	syncClient, err := NewSyncClient(
		backend, NewMemorySyncCursorStore(), fn.None[btclog.Logger](),
	)
	require.NoError(t, err)

	// First poll: receives one event at cursor 0.
	result1, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(),
		"sub-1", nil, 100,
	)
	require.NoError(t, err)
	require.Len(t, result1.Response.Events, 1)
	require.Equal(t, uint64(1), result1.Response.NextCursor)

	// Ack the batch so the cursor advances.
	require.NoError(t, result1.Ack())

	// Second poll: starts from cursor 1 (advanced by ack).
	result2, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(),
		"sub-1", nil, 100,
	)
	require.NoError(t, err)
	require.Empty(t, result2.Response.Events)

	require.Len(t, backend.vtxoCalls, 2)
	require.Equal(t, uint64(0), backend.vtxoCalls[0].after)
	require.Equal(t, uint64(1), backend.vtxoCalls[1].after)
}

// TestSyncClientOORCursorPersistence verifies the OOR cursor is
// loaded and advanced across successive polling calls when the
// caller invokes Ack.
func TestSyncClientOORCursorPersistence(t *testing.T) {
	t.Parallel()

	backend := &testSyncBackend{
		oorResponses: []*arkrpc.ListOORRecipientEventsByScriptResponse{
			{
				Events: []*arkrpc.OORRecipientEvent{{
					EventId: 2,
				}},
				NextCursor: 2,
			},
			{
				Events:     nil,
				NextCursor: 2,
			},
		},
	}
	syncClient, err := NewSyncClient(
		backend, NewMemorySyncCursorStore(), fn.None[btclog.Logger](),
	)
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20, 0x01}

	// First poll: receives one event at cursor 0.
	result1, err := syncClient.SyncOORRecipientEventsTaproot(
		t.Context(), pkScript, 100,
	)
	require.NoError(t, err)
	require.Len(t, result1.Response.Events, 1)
	require.Equal(t, uint64(2), result1.Response.NextCursor)

	// Ack the batch so the cursor advances.
	require.NoError(t, result1.Ack())

	// Second poll: starts from cursor 2 (advanced by ack).
	result2, err := syncClient.SyncOORRecipientEventsTaproot(
		t.Context(), pkScript, 100,
	)
	require.NoError(t, err)
	require.Empty(t, result2.Response.Events)

	require.Len(t, backend.oorCalls, 2)
	require.Equal(t, uint64(0), backend.oorCalls[0].after)
	require.Equal(t, uint64(2), backend.oorCalls[1].after)
}

// TestSyncClientVTXONoAckDoesNotAdvanceCursor verifies that omitting
// Ack leaves the cursor at its original position, causing the next
// poll to re-fetch from the same offset.
func TestSyncClientVTXONoAckDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()

	backend := &testSyncBackend{
		vtxoResponses: []*arkrpc.ListVTXOEventsByScriptsResponse{
			{
				Events: []*arkrpc.VTXOEvent{{
					EventId: 5,
				}},
				NextCursor: 5,
			},
			{
				Events: []*arkrpc.VTXOEvent{{
					EventId: 5,
				}},
				NextCursor: 5,
			},
		},
	}
	syncClient, err := NewSyncClient(
		backend, NewMemorySyncCursorStore(), fn.None[btclog.Logger](),
	)
	require.NoError(t, err)

	// First poll: receive events but do NOT ack.
	result1, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(),
		"no-ack-key", nil, 100,
	)
	require.NoError(t, err)
	require.Len(t, result1.Response.Events, 1)

	// Deliberately skip result1.Ack().

	// Second poll: should still start from cursor 0 because
	// the first batch was never acknowledged.
	result2, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(),
		"no-ack-key", nil, 100,
	)
	require.NoError(t, err)
	require.Len(t, result2.Response.Events, 1)

	// Both calls should have used cursor 0.
	require.Len(t, backend.vtxoCalls, 2)
	require.Equal(t, uint64(0), backend.vtxoCalls[0].after)
	require.Equal(t, uint64(0), backend.vtxoCalls[1].after)
}

// TestNewSyncClientRejectsNilStore verifies that NewSyncClient
// returns an error when store is nil.
func TestNewSyncClientRejectsNilStore(t *testing.T) {
	t.Parallel()

	_, err := NewSyncClient(
		&testSyncBackend{}, nil, fn.None[btclog.Logger](),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cursor store")
}

// TestNewSyncClientRejectsNilBackend verifies that NewSyncClient
// returns an error when backend is nil.
func TestNewSyncClientRejectsNilBackend(t *testing.T) {
	t.Parallel()

	_, err := NewSyncClient(
		nil, NewMemorySyncCursorStore(), fn.None[btclog.Logger](),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend")
}
