package indexer

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"

	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/build"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
		scopes []TaprootScriptScope, afterEventID uint64, limit uint32,
		opts ...mailboxrpc.RPCOptions) (
		*arkrpc.ListVTXOEventsByScriptsResponse, error)

	// ListOORRecipientEventsByScriptTaproot returns
	// script-scoped OOR recipient events.
	ListOORRecipientEventsByScriptTaproot(ctx context.Context,
		pkScript []byte, afterEventID uint64, limit uint32,
		opts ...mailboxrpc.RPCOptions) (
		*arkrpc.ListOORRecipientEventsByScriptResponse, error)
}

// SyncCursorStore persists monotonic cursors used by
// SyncClient.
type SyncCursorStore interface {
	// LoadCursor returns the cursor for (namespace, key).
	// Unknown keys should return 0, nil.
	LoadCursor(ctx context.Context, namespace string,
		key string) (uint64, error)

	// SaveCursor persists cursor for (namespace, key).
	// Implementations should treat cursors as monotonic and
	// avoid backwards movement.
	SaveCursor(ctx context.Context, namespace string, key string,
		cursor uint64) error
}

// SyncClient provides cursor-aware, restart-friendly polling helpers
// over the indexer RPC surface.
//
// Callers must ensure that at most one goroutine calls a given
// sync method with the same cursor key at a time. Concurrent calls
// with different keys are safe. Violating this constraint causes a
// time-of-check/time-of-use race on the stored cursor that may skip
// or re-deliver events.
type SyncClient struct {
	backend SyncBackend
	cursors SyncCursorStore

	// Log is an optional logger for this sync client. If None, the client
	// falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]
}

// NewSyncClient creates a SyncClient. Both backend and store are
// required; passing nil for either is a programming error. The optional
// log is used for constructor and runtime logging; if unset, the client
// falls back to context-based logging.
func NewSyncClient(backend SyncBackend, store SyncCursorStore,
	log fn.Option[btclog.Logger]) (*SyncClient, error) {

	if backend == nil {
		return nil, fmt.Errorf("sync backend must not be nil")
	}
	if store == nil {
		return nil, fmt.Errorf("sync cursor store must not be nil")
	}

	client := &SyncClient{
		backend: backend,
		cursors: store,
		Log:     log,
	}

	client.logger(context.Background()).InfoS(
		context.Background(), "Initializing sync client",
	)

	return client, nil
}

// logger returns the configured logger, falling back to extracting a logger
// from context. If neither is available, returns btclog.Disabled which safely
// no-ops all log calls.
func (c *SyncClient) logger(ctx context.Context) btclog.Logger {
	return c.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// VTXOSyncResult wraps a VTXO event response and defers cursor
// persistence until the caller explicitly acknowledges the batch via
// Ack. This prevents the cursor from advancing when the caller fails
// to fully process the events.
type VTXOSyncResult struct {
	// Response is the raw RPC response containing the events.
	Response *arkrpc.ListVTXOEventsByScriptsResponse

	ack func() error
}

// Ack persists the response's NextCursor to the store, advancing
// the cursor past the returned events. Callers should only call Ack
// after they have fully processed the batch. Ack is idempotent
// within a single result but must not be called concurrently.
//
// NOTE: The ack closure captures the context from the
// SyncVTXOEventsTaproot call. If that context is cancelled before
// Ack is invoked (e.g., during shutdown), the cursor persist will
// fail even though the batch was processed. Callers that need to
// persist cursors on shutdown should call Ack before cancelling the
// polling context.
func (r *VTXOSyncResult) Ack() error {
	return r.ack()
}

// OORSyncResult wraps an OOR recipient event response and defers
// cursor persistence until the caller explicitly acknowledges the
// batch via Ack.
type OORSyncResult struct {
	// Response is the raw RPC response containing the events.
	Response *arkrpc.ListOORRecipientEventsByScriptResponse

	ack func() error
}

// Ack persists the response's NextCursor to the store, advancing
// the cursor past the returned events. Callers should only call Ack
// after they have fully processed the batch. Ack is idempotent
// within a single result but must not be called concurrently.
//
// NOTE: The ack closure captures the context from the
// SyncOORRecipientEventsTaproot call. If that context is cancelled
// before Ack is invoked, the cursor persist will fail even though
// the batch was processed. Callers that need to persist cursors on
// shutdown should call Ack before cancelling the polling context.
func (r *OORSyncResult) Ack() error {
	return r.ack()
}

// SyncVTXOEventsTaproot polls VTXO events starting from the stored
// cursor for cursorKey. The cursor is NOT advanced until the caller
// invokes Ack on the returned result.
func (c *SyncClient) SyncVTXOEventsTaproot(ctx context.Context,
	cursorKey string, scopes []TaprootScriptScope, limit uint32,
	opts ...mailboxrpc.RPCOptions) (*VTXOSyncResult, error) {

	if cursorKey == "" {
		return nil, fmt.Errorf("missing cursor key")
	}

	cursor, err := c.cursors.LoadCursor(
		ctx, vtxoCursorNamespace, cursorKey,
	)
	if err != nil {
		return nil, fmt.Errorf("load vtxo cursor: %w", err)
	}

	c.logger(ctx).TraceS(ctx, "Polling VTXO events",
		slog.String("cursor_key", cursorKey),
		slog.Uint64("cursor", cursor),
		slog.Int("limit", int(limit)))

	resp, err := c.backend.ListVTXOEventsByScriptsTaproot(
		ctx, scopes, cursor, limit, opts...,
	)
	if err != nil {
		return nil, err
	}

	c.logger(ctx).TraceS(ctx, "VTXO events poll complete",
		slog.String("cursor_key", cursorKey),
		slog.Int("event_count", len(resp.Events)),
		slog.Uint64("next_cursor", resp.NextCursor))

	// Capture the cursor for the ack closure. The cursor is
	// only persisted when the caller calls Ack, preventing
	// advancement on incomplete processing.
	nextCursor := resp.NextCursor
	prevCursor := cursor

	return &VTXOSyncResult{
		Response: resp,
		ack: func() error {
			if nextCursor <= prevCursor {
				return nil
			}

			return c.cursors.SaveCursor(
				ctx, vtxoCursorNamespace, cursorKey, nextCursor,
			)
		},
	}, nil
}

// SyncOORRecipientEventsTaproot polls OOR recipient events for
// pkScript starting from the stored cursor. The cursor is NOT
// advanced until the caller invokes Ack on the returned result.
func (c *SyncClient) SyncOORRecipientEventsTaproot(ctx context.Context,
	pkScript []byte, limit uint32, opts ...mailboxrpc.RPCOptions) (
	*OORSyncResult, error) {

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

	c.logger(ctx).TraceS(ctx, "Polling OOR recipient events",
		slog.String("pk_script", scriptKey),
		slog.Uint64("cursor", cursor),
		slog.Int("limit", int(limit)))

	resp, err := c.backend.ListOORRecipientEventsByScriptTaproot(
		ctx, pkScript, cursor, limit, opts...,
	)
	if err != nil {
		return nil, err
	}

	c.logger(ctx).TraceS(ctx, "OOR recipient events poll complete",
		slog.String("pk_script", scriptKey),
		slog.Int("event_count", len(resp.Events)),
		slog.Uint64("next_cursor", resp.NextCursor))

	// Capture the cursor for the ack closure.
	nextCursor := resp.NextCursor
	prevCursor := cursor

	return &OORSyncResult{
		Response: resp,
		ack: func() error {
			if nextCursor <= prevCursor {
				return nil
			}

			return c.cursors.SaveCursor(
				ctx, oorCursorNamespace, scriptKey, nextCursor,
			)
		},
	}, nil
}

// MemorySyncCursorStore is an in-memory SyncCursorStore
// implementation.
type MemorySyncCursorStore struct {
	mu      sync.RWMutex
	cursors map[string]uint64
}

// NewMemorySyncCursorStore creates a new in-memory sync cursor store.
func NewMemorySyncCursorStore() *MemorySyncCursorStore {
	return &MemorySyncCursorStore{
		cursors: make(map[string]uint64),
	}
}

// LoadCursor returns the stored cursor for (namespace, key).
func (s *MemorySyncCursorStore) LoadCursor(_ context.Context, namespace string,
	key string) (uint64, error) {

	s.mu.RLock()
	defer s.mu.RUnlock()

	namespacedKey := s.namespacedKey(namespace, key)

	return s.cursors[namespacedKey], nil
}

// SaveCursor stores cursor for (namespace, key) monotonically.
func (s *MemorySyncCursorStore) SaveCursor(_ context.Context, namespace string,
	key string, cursor uint64) error {

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

// namespacedKey returns the canonical map key for
// (namespace, key).
func (s *MemorySyncCursorStore) namespacedKey(namespace string,
	key string) string {

	return namespace + "/" + key
}

var _ SyncCursorStore = (*MemorySyncCursorStore)(nil)
var _ SyncBackend = (*Client)(nil)
