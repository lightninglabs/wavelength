package indexer

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/stretchr/testify/require"
)

// syncBackendCall records a backend call argument used in tests.
type syncBackendCall struct {
	after uint64
}

// testSyncBackend is a deterministic in-memory SyncBackend for tests.
type testSyncBackend struct {
	vtxoCalls []syncBackendCall
	oorCalls  []syncBackendCall

	vtxoResponses []*arkrpc.ListVTXOEventsByScriptsResponse
	oorResponses  []*arkrpc.ListOORRecipientEventsByScriptResponse
}

// ListVTXOEventsByScriptsTaproot records afterEventID and returns queued
// responses.
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

// ListOORRecipientEventsByScriptTaproot records afterEventID and returns queued
// responses.
func (b *testSyncBackend) ListOORRecipientEventsByScriptTaproot(
	_ context.Context, _ []byte, _ *btcec.PrivateKey, afterEventID uint64,
	_ uint32, _ ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	b.oorCalls = append(b.oorCalls, syncBackendCall{
		after: afterEventID,
	})

	if len(b.oorResponses) == 0 {
		return &arkrpc.ListOORRecipientEventsByScriptResponse{}, nil
	}

	resp := b.oorResponses[0]
	b.oorResponses = b.oorResponses[1:]

	return resp, nil
}

// TestMemorySyncCursorStoreMonotonic verifies monotonic cursor storage.
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

// TestSyncClientVTXOEventCursorPersistence verifies the event cursor is loaded
// and advanced across successive polling calls.
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
	syncClient := NewSyncClient(backend, NewMemorySyncCursorStore())

	resp1, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(), "sub-1", nil, 100,
	)
	require.NoError(t, err)
	require.Len(t, resp1.Events, 1)
	require.Equal(t, uint64(1), resp1.NextCursor)

	resp2, err := syncClient.SyncVTXOEventsTaproot(
		t.Context(), "sub-1", nil, 100,
	)
	require.NoError(t, err)
	require.Empty(t, resp2.Events)

	require.Len(t, backend.vtxoCalls, 2)
	require.Equal(t, uint64(0), backend.vtxoCalls[0].after)
	require.Equal(t, uint64(1), backend.vtxoCalls[1].after)
}

// TestSyncClientOORCursorPersistence verifies the OOR cursor is loaded and
// advanced across successive polling calls.
func TestSyncClientOORCursorPersistence(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

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
	syncClient := NewSyncClient(backend, NewMemorySyncCursorStore())

	pkScript := []byte{0x51, 0x20, 0x01}

	resp1, err := syncClient.SyncOORRecipientEventsTaproot(
		t.Context(), pkScript, priv, 100,
	)
	require.NoError(t, err)
	require.Len(t, resp1.Events, 1)
	require.Equal(t, uint64(2), resp1.NextCursor)

	resp2, err := syncClient.SyncOORRecipientEventsTaproot(
		t.Context(), pkScript, priv, 100,
	)
	require.NoError(t, err)
	require.Empty(t, resp2.Events)

	require.Len(t, backend.oorCalls, 2)
	require.Equal(t, uint64(0), backend.oorCalls[0].after)
	require.Equal(t, uint64(2), backend.oorCalls[1].after)
}
