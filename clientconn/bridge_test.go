package clientconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/stretchr/testify/require"
)

// mockUnknownClientHandler is a test implementation of
// UnknownClientHandler that records calls and optionally returns
// an error.
type mockUnknownClientHandler struct {
	mu    sync.Mutex
	calls []ClientID
	err   error
}

// HandleUnknownClient records the call and returns the configured
// error.
func (m *mockUnknownClientHandler) HandleUnknownClient(
	_ context.Context, clientID ClientID,
	_ *mailboxpb.Envelope) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, clientID)

	return m.err
}

// callCount returns the number of times HandleUnknownClient was
// called.
func (m *mockUnknownClientHandler) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.calls)
}

// TestHandleInboundNilEnvelope verifies that HandleInbound is a
// no-op when the envelope is nil.
func TestHandleInboundNilEnvelope(t *testing.T) {
	t.Parallel()

	handler := &mockUnknownClientHandler{}
	bridge := NewClientsConnBridge(
		WithOnUnknownClient(handler),
	)

	err := bridge.HandleInbound(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, handler.callCount())
}

// TestHandleInboundEmptySender verifies that HandleInbound is a
// no-op when the envelope has an empty sender.
func TestHandleInboundEmptySender(t *testing.T) {
	t.Parallel()

	handler := &mockUnknownClientHandler{}
	bridge := NewClientsConnBridge(
		WithOnUnknownClient(handler),
	)

	env := &mailboxpb.Envelope{Sender: ""}
	err := bridge.HandleInbound(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, 0, handler.callCount())
}

// TestHandleInboundNoHandler verifies that HandleInbound silently
// ignores unknown clients when no handler is configured.
func TestHandleInboundNoHandler(t *testing.T) {
	t.Parallel()

	bridge := NewClientsConnBridge()

	env := &mailboxpb.Envelope{Sender: "client-1"}
	err := bridge.HandleInbound(context.Background(), env)
	require.NoError(t, err)
}

// TestHandleInboundKnownClient verifies that HandleInbound skips
// the handler when the client is already registered.
func TestHandleInboundKnownClient(t *testing.T) {
	t.Parallel()

	handler := &mockUnknownClientHandler{}
	bridge := NewClientsConnBridge(
		WithOnUnknownClient(handler),
	)

	// Pre-register client with a minimal config.
	ctx := context.Background()
	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)
	cfg.LocalMailboxID = "svc:client-1"
	cfg.RemoteMailboxID = "client-1"

	_, err := bridge.RegisterClient(ctx, "client-1", cfg)
	require.NoError(t, err)

	env := &mailboxpb.Envelope{Sender: "client-1"}
	err = bridge.HandleInbound(ctx, env)
	require.NoError(t, err)
	require.Equal(t, 0, handler.callCount())

	bridge.Stop()
}

// TestHandleInboundTriggersHandler verifies that HandleInbound calls
// the handler for an unknown client.
func TestHandleInboundTriggersHandler(t *testing.T) {
	t.Parallel()

	handler := &mockUnknownClientHandler{}
	bridge := NewClientsConnBridge(
		WithOnUnknownClient(handler),
	)

	env := &mailboxpb.Envelope{
		Sender:    "new-client",
		Recipient: "svc:new-client",
	}
	err := bridge.HandleInbound(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, 1, handler.callCount())
}

// TestHandleInboundHandlerError verifies that HandleInbound
// propagates errors from the handler.
func TestHandleInboundHandlerError(t *testing.T) {
	t.Parallel()

	handler := &mockUnknownClientHandler{
		err: fmt.Errorf("registration failed"),
	}
	bridge := NewClientsConnBridge(
		WithOnUnknownClient(handler),
	)

	env := &mailboxpb.Envelope{Sender: "fail-client"}
	err := bridge.HandleInbound(context.Background(), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "registration failed")
}

// TestHandleInboundConcurrentSameClient verifies that concurrent
// HandleInbound calls for the same client only trigger one
// handler invocation via singleflight dedup.
//
// The handler mimics production behavior by registering the
// client on the bridge, so late-arriving goroutines see the
// client via GetClient and skip the handler entirely.
func TestHandleInboundConcurrentSameClient(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32

	const goroutines = 10

	// gate holds all goroutines until they're all ready, so
	// they race into singleflight.Do simultaneously.
	gate := make(chan struct{})

	// entered signals the handler has been invoked, so the
	// test knows the singleflight key is in-flight. release
	// unblocks the handler so it can return.
	entered := make(chan struct{})
	release := make(chan struct{})

	bridge := NewClientsConnBridge()

	handler := &registeringHandler{
		callCount: &callCount,
		entered:   entered,
		release:   release,
		bridge:    bridge,
	}
	bridge.onUnknownClient = handler

	// Start all goroutines. They block on the gate until
	// all are ready, then race into HandleInbound.
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()

			// Wait for the gate to open so all
			// goroutines enter simultaneously.
			<-gate

			env := &mailboxpb.Envelope{
				Sender:    "same-client",
				Recipient: "svc:same-client",
				MsgId:     fmt.Sprintf("msg-%d", i),
			}

			_ = bridge.HandleInbound(
				context.Background(), env,
			)
		}(i)
	}

	// Open the gate so all goroutines race into
	// HandleInbound → singleflight.Do together.
	close(gate)

	// Wait for the handler to be entered (one goroutine won
	// the singleflight race). While it blocks, the remaining
	// goroutines queue up on the same singleflight key.
	<-entered

	// Release the handler. All queued goroutines receive the
	// same result without invoking the handler again.
	close(release)
	wg.Wait()

	// Exactly one handler invocation: concurrent calls are
	// coalesced by singleflight, and late arrivals see the
	// registered client via GetClient.
	require.Equal(t, int32(1), callCount.Load())

	bridge.Stop()
}

// registeringHandler is a test handler that registers the client
// on the bridge (mimicking production behavior), signals entry,
// and blocks until released.
type registeringHandler struct {
	callCount *atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	bridge    *ClientsConnBridge
}

// HandleUnknownClient registers the client on the bridge,
// increments the call counter, signals entry, and blocks until
// released.
func (h *registeringHandler) HandleUnknownClient(ctx context.Context,
	clientID ClientID, env *mailboxpb.Envelope) error {

	h.callCount.Add(1)

	// Register the client on the bridge, just like production
	// code does. This ensures late-arriving goroutines see the
	// client in GetClient and skip the handler.
	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)
	cfg.LocalMailboxID = env.Recipient
	cfg.RemoteMailboxID = env.Sender

	_, err := h.bridge.RegisterClient(ctx, clientID, cfg)
	if err != nil {
		return err
	}

	// Signal that we're inside the handler.
	close(h.entered)

	// Block until the test releases us. While we block, other
	// goroutines queue on the same singleflight key.
	<-h.release

	return nil
}
