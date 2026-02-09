package indexer

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
)

const (
	// vtxoCursorNamespace is the namespace used for VTXO
	// event feed cursors.
	vtxoCursorNamespace = "vtxo_events"

	// oorCursorNamespace is the namespace used for OOR
	// recipient event cursors.
	oorCursorNamespace = "oor_recipient_events"
)

// SyncBackend captures the high-level indexer client methods
// used by SyncClient.
//
// *Client satisfies this interface.
type SyncBackend interface {
	// ListVTXOEventsByScriptsTaproot returns script-scoped
	// VTXO lifecycle events.
	ListVTXOEventsByScriptsTaproot(ctx context.Context,
		scopes []TaprootScriptScope,
		afterEventID uint64, limit uint32,
		opts ...mailboxrpc.RPCOptions,
	) (*arkrpc.ListVTXOEventsByScriptsResponse, error)

	// ListOORRecipientEventsByScriptTaproot returns
	// script-scoped OOR recipient events.
	ListOORRecipientEventsByScriptTaproot(ctx context.Context,
		pkScript []byte, signingKey *btcec.PrivateKey,
		afterEventID uint64, limit uint32,
		opts ...mailboxrpc.RPCOptions,
	) (*arkrpc.ListOORRecipientEventsByScriptResponse, error)
}

// SyncCursorStore persists monotonic cursors used by SyncClient.
type SyncCursorStore interface {
	// LoadCursor returns the cursor for (namespace, key).
	// Unknown keys should return 0, nil.
	LoadCursor(ctx context.Context, namespace string, key string) (
		uint64, error)

	// SaveCursor persists cursor for (namespace, key).
	// Implementations should treat cursors as monotonic and
	// avoid backwards movement.
	SaveCursor(ctx context.Context, namespace string, key string,
		cursor uint64) error
}

// SyncClient provides cursor-aware, restart-friendly polling helpers over the
// indexer RPC surface.
type SyncClient struct {
	backend SyncBackend
	cursors SyncCursorStore
}

// NewSyncClient creates a SyncClient.
//
// If store is nil, a process-local in-memory store is used.
func NewSyncClient(backend SyncBackend, store SyncCursorStore) *SyncClient {
	if store == nil {
		store = NewMemorySyncCursorStore()
	}

	return &SyncClient{
		backend: backend,
		cursors: store,
	}
}

// SyncVTXOEventsTaproot polls VTXO events starting from the stored cursor for
// cursorKey, then persists NextCursor on success.
func (c *SyncClient) SyncVTXOEventsTaproot(ctx context.Context,
	cursorKey string, scopes []TaprootScriptScope, limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListVTXOEventsByScriptsResponse, error) {

	if c == nil || c.backend == nil {
		return nil, fmt.Errorf("missing sync backend")
	}
	if c.cursors == nil {
		return nil, fmt.Errorf("missing sync cursor store")
	}
	if cursorKey == "" {
		return nil, fmt.Errorf("missing cursor key")
	}

	cursor, err := c.cursors.LoadCursor(
		ctx, vtxoCursorNamespace, cursorKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load vtxo cursor: %w", err)
	}

	resp, err := c.backend.ListVTXOEventsByScriptsTaproot(
		ctx, scopes, cursor, limit, opts...,
	)
	if err != nil {
		return nil, err
	}

	if resp.NextCursor > cursor {
		err := c.cursors.SaveCursor(
			ctx, vtxoCursorNamespace, cursorKey, resp.NextCursor,
		)
		if err != nil {
			return nil, fmt.Errorf("save vtxo cursor: %w", err)
		}
	}

	return resp, nil
}

// SyncOORRecipientEventsTaproot polls OOR recipient events for pkScript
// starting from the stored cursor, then persists NextCursor on success.
func (c *SyncClient) SyncOORRecipientEventsTaproot(ctx context.Context,
	pkScript []byte, signingKey *btcec.PrivateKey, limit uint32,
	opts ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	if c == nil || c.backend == nil {
		return nil, fmt.Errorf("missing sync backend")
	}
	if c.cursors == nil {
		return nil, fmt.Errorf("missing sync cursor store")
	}
	if len(pkScript) == 0 {
		return nil, fmt.Errorf("missing pkScript")
	}

	scriptKey := hex.EncodeToString(pkScript)
	cursor, err := c.cursors.LoadCursor(
		ctx, oorCursorNamespace, scriptKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load oor cursor: %w", err)
	}

	resp, err := c.backend.ListOORRecipientEventsByScriptTaproot(
		ctx, pkScript, signingKey, cursor, limit, opts...,
	)
	if err != nil {
		return nil, err
	}

	if resp.NextCursor > cursor {
		err := c.cursors.SaveCursor(
			ctx, oorCursorNamespace, scriptKey, resp.NextCursor,
		)
		if err != nil {
			return nil, fmt.Errorf("save oor cursor: %w", err)
		}
	}

	return resp, nil
}

// MemorySyncCursorStore is an in-memory SyncCursorStore implementation.
type MemorySyncCursorStore struct {
	mu      sync.Mutex
	cursors map[string]uint64
}

// NewMemorySyncCursorStore creates a new in-memory sync cursor store.
func NewMemorySyncCursorStore() *MemorySyncCursorStore {
	return &MemorySyncCursorStore{
		cursors: make(map[string]uint64),
	}
}

// LoadCursor returns the stored cursor for (namespace, key).
func (s *MemorySyncCursorStore) LoadCursor(_ context.Context,
	namespace string, key string) (uint64, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	namespacedKey := s.namespacedKey(namespace, key)

	return s.cursors[namespacedKey], nil
}

// SaveCursor stores cursor for (namespace, key) monotonically.
func (s *MemorySyncCursorStore) SaveCursor(_ context.Context,
	namespace string, key string, cursor uint64) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	namespacedKey := s.namespacedKey(namespace, key)
	old := s.cursors[namespacedKey]
	if cursor < old {
		return nil
	}

	s.cursors[namespacedKey] = cursor

	return nil
}

// namespacedKey returns the canonical map key for (namespace, key).
func (s *MemorySyncCursorStore) namespacedKey(namespace string,
	key string) string {

	return namespace + ":" + key
}

var _ SyncCursorStore = (*MemorySyncCursorStore)(nil)
var _ SyncBackend = (*Client)(nil)
