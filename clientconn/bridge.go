package clientconn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"golang.org/x/sync/singleflight"
)

// ClientsConnBridge is the top-level router that implements
// actor.TellOnlyRef[ClientConnMsg]. It manages a collection of
// per-client ClientRuntime instances and routes outbound server events
// to the correct client's DurableActor based on the ClientID in the
// message.
//
// When Tell() is called with a SendServerEventRequest, the bridge:
//  1. Extracts the ClientID from the message
//  2. Looks up the per-client ClientRuntime
//  3. Forwards to the per-client DurableActor via TellRef().Tell()
//
// Because TellRef().Tell() persists the message durably before returning,
// a nil return from Tell() means the message is crash-safe in the
// per-client durable mailbox. There is no crash gap.
type ClientsConnBridge struct {
	mu      sync.RWMutex
	clients map[ClientID]*ClientRuntime

	statusTracker StatusTracker

	// maxClients bounds the number of concurrently registered
	// clients. Zero means unlimited.
	maxClients int

	// onUnknownClient is called when HandleInbound receives an
	// envelope from an unregistered sender. If nil, unknown
	// clients are silently ignored.
	onUnknownClient UnknownClientHandler

	// registerGroup deduplicates concurrent HandleInbound calls
	// for the same clientID so only one registration attempt
	// runs per client.
	registerGroup singleflight.Group
}

// NewClientsConnBridge creates a new bridge with the given options.
// If no StatusTracker is provided, a noopStatusTracker is used.
func NewClientsConnBridge(
	opts ...BridgeOption,
) *ClientsConnBridge {

	o := &bridgeOptions{
		statusTracker: &noopStatusTracker{},
	}
	for _, opt := range opts {
		opt(o)
	}

	return &ClientsConnBridge{
		clients:         make(map[ClientID]*ClientRuntime),
		statusTracker:   o.statusTracker,
		maxClients:      o.maxClients,
		onUnknownClient: o.onUnknownClient,
	}
}

// Tell implements actor.TellOnlyRef[ClientConnMsg]. It routes the
// message to the correct per-client DurableActor based on the ClientID.
// Returns an error if the client is not registered.
func (b *ClientsConnBridge) Tell(ctx context.Context,
	msg ClientConnMsg) error {

	switch m := msg.(type) {
	case *SendServerEventRequest:
		if m == nil {
			return fmt.Errorf(
				"typed-nil SendServerEventRequest",
			)
		}

		if m.Message == nil {
			return fmt.Errorf(
				"nil Message in SendServerEventRequest",
			)
		}

		clientID := m.Message.ClientID()

		// Hold the read lock across the lookup and the Tell so
		// a concurrent DeregisterClient cannot Stop the runtime
		// between the map lookup and the durable Tell call.
		b.mu.RLock()
		runtime, ok := b.clients[clientID]
		if !ok {
			b.mu.RUnlock()

			return fmt.Errorf(
				"client %q not registered", clientID,
			)
		}

		// Forward to the per-client DurableActor. TellRef()
		// returns a TellOnlyRef that persists the message to the
		// durable mailbox before returning nil. This guarantees
		// crash safety.
		err := runtime.TellRef().Tell(ctx, &sendEventMsg{
			Message:  m.Message,
			clientID: clientID,
		})
		b.mu.RUnlock()

		return err

	default:
		return fmt.Errorf(
			"unknown message type: %T", msg,
		)
	}
}

// RegisterClient creates a new per-client runtime with the given config,
// starts it, and adds it to the bridge's client map. Returns an error if
// the client is already registered or if the runtime fails to start.
func (b *ClientsConnBridge) RegisterClient(ctx context.Context,
	clientID ClientID, cfg PerClientConfig,
) (*ClientRuntime, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.clients[clientID]; exists {
		return nil, fmt.Errorf(
			"client %q already registered", clientID,
		)
	}

	// Enforce the maximum concurrent client limit when configured.
	if b.maxClients > 0 && len(b.clients) >= b.maxClients {
		return nil, fmt.Errorf(
			"max clients reached (%d)", b.maxClients,
		)
	}

	// Enforce uniqueness of mailbox IDs across all registered
	// clients. Two clients sharing the same LocalMailboxID would
	// alias their checkpoint and durable actor identity, corrupting
	// each other's delivery progress.
	for id, existing := range b.clients {
		existingCfg := existing.connector.cfg

		if existingCfg.LocalMailboxID == cfg.LocalMailboxID {
			return nil, fmt.Errorf(
				"client %q: LocalMailboxID %q "+
					"already in use by client %q",
				clientID, cfg.LocalMailboxID, id,
			)
		}

		if existingCfg.RemoteMailboxID == cfg.RemoteMailboxID {
			return nil, fmt.Errorf(
				"client %q: RemoteMailboxID %q "+
					"already in use by client %q",
				clientID, cfg.RemoteMailboxID, id,
			)
		}
	}

	// Stamp the client ID so the ingress loop can reference it
	// for activity tracking without knowing the bridge's key map.
	cfg.ClientID = clientID
	cfg.ActivityMarker = b.statusTracker

	// Notify the tracker of the new client so it can initialise
	// per-client state.
	b.statusTracker.RegisterClient(clientID)

	runtime, err := NewClientRuntime(cfg)
	if err != nil {
		// Roll back the tracker registration on failure.
		b.statusTracker.DeregisterClient(clientID)

		return nil, fmt.Errorf(
			"create runtime for client %q: %w",
			clientID, err,
		)
	}

	if err := runtime.Start(ctx); err != nil {
		// Clean up the partially initialized runtime so its
		// DurableActor and ingress goroutine don't leak.
		runtime.Stop()

		// Roll back the tracker registration on failure.
		b.statusTracker.DeregisterClient(clientID)

		return nil, fmt.Errorf(
			"start runtime for client %q: %w",
			clientID, err,
		)
	}

	b.clients[clientID] = runtime

	return runtime, nil
}

// DeregisterClient stops the per-client runtime and removes it from the
// bridge's client map. Returns an error if the client is not registered.
func (b *ClientsConnBridge) DeregisterClient(
	clientID ClientID,
) error {

	// Remove from client map under lock, but defer the tracker
	// notification to after the lock is released. The tracker's
	// DeregisterClient fires status-change callbacks, and those
	// callbacks may query bridge state — holding b.mu here would
	// deadlock.
	var runtime *ClientRuntime

	b.mu.Lock()
	rt, ok := b.clients[clientID]
	if !ok {
		b.mu.Unlock()

		return fmt.Errorf(
			"client %q not registered", clientID,
		)
	}

	runtime = rt
	delete(b.clients, clientID)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stopErr error
	if err := runtime.StopAndWait(ctx); err != nil {
		stopErr = fmt.Errorf(
			"stop client %q runtime: %w", clientID, err,
		)
	}

	// Notify the tracker so it can clean up per-client state.
	// This is deliberately outside b.mu to avoid the deadlock
	// described above.
	b.statusTracker.DeregisterClient(clientID)

	return stopErr
}

// GetClient returns the per-client runtime for the given client. The
// boolean indicates whether the client is registered.
func (b *ClientsConnBridge) GetClient(
	clientID ClientID,
) (*ClientRuntime, bool) {

	b.mu.RLock()
	defer b.mu.RUnlock()

	rt, ok := b.clients[clientID]

	return rt, ok
}

// GetUnary returns the per-client UnaryFacade for sending unary RPCs to
// the given client. The boolean indicates whether the client is
// registered.
func (b *ClientsConnBridge) GetUnary(
	clientID ClientID,
) (*UnaryFacade, bool) {

	runtime, ok := b.GetClient(clientID)
	if !ok {
		return nil, false
	}

	return runtime.Unary(), true
}

// HandleInbound checks whether an inbound envelope's sender is a
// registered client. If not, and an UnknownClientHandler is configured,
// the handler is invoked to register the client on the bridge.
//
// Concurrent calls for the same clientID are deduplicated via
// singleflight so only one registration attempt runs per client.
// Different clients can register concurrently without blocking
// each other.
//
// This method is intended to be called at the transport boundary
// (e.g., a MailboxServiceServer decorator) before the envelope is
// persisted. The handler typically calls
// RegisterClientWithAllDispatchers, which starts the per-client
// ingress loop that will then pull and dispatch the envelope.
func (b *ClientsConnBridge) HandleInbound(ctx context.Context,
	env *mailboxpb.Envelope) error {

	if env == nil {
		return nil
	}

	senderID := env.Sender
	if senderID == "" {
		return nil
	}

	clientID := ClientID(senderID)

	// Fast path: already registered.
	if _, ok := b.GetClient(clientID); ok {
		return nil
	}

	// No handler configured — silently ignore unknown clients.
	if b.onUnknownClient == nil {
		return nil
	}

	// Deduplicate concurrent registrations for the same client.
	// singleflight ensures only one goroutine runs the handler
	// while others block and receive the same result.
	_, err, _ := b.registerGroup.Do(string(clientID),
		func() (any, error) {
			// Double-check after winning the singleflight
			// race in case a prior call already completed.
			if _, ok := b.GetClient(clientID); ok {
				return nil, nil
			}

			return nil, b.onUnknownClient.HandleUnknownClient(
				ctx, clientID, env,
			)
		},
	)

	return err
}

// ClientStatus returns the current liveness status of the given client
// as reported by the configured StatusTracker. This is informational
// only — messages are always delivered to the mailbox regardless of
// status.
func (b *ClientsConnBridge) ClientStatus(
	clientID ClientID,
) ClientStatus {

	return b.statusTracker.Status(clientID)
}

// ID implements actor.BaseActorRef. The bridge uses a fixed identifier
// since it is a singleton router, not a per-client actor.
func (b *ClientsConnBridge) ID() string {
	return "clientconn-bridge"
}

// ClientSnapshot is a point-in-time view of a registered client's
// state, suitable for admin RPC responses.
type ClientSnapshot struct {
	// ID is the unique client identifier.
	ID ClientID

	// Status is the current liveness status.
	Status ClientStatus
}

// ListClients returns a snapshot of all currently registered clients
// and their statuses. The returned slice is safe to use after the lock
// is released.
func (b *ClientsConnBridge) ListClients() []ClientSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ClientSnapshot, 0, len(b.clients))
	for id := range b.clients {
		result = append(result, ClientSnapshot{
			ID:     id,
			Status: b.statusTracker.Status(id),
		})
	}

	return result
}

// Stop shuts down all registered client runtimes and clears the client
// map.
func (b *ClientsConnBridge) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = b.StopAndWait(ctx)
}

// StopAndWait shuts down all registered client runtimes, waits for their
// durable actors to exit, and clears the client map.
func (b *ClientsConnBridge) StopAndWait(ctx context.Context) error {
	// Snapshot the current runtimes under the lock, then stop and
	// deregister them outside the lock. Deregistration can fire
	// status-change callbacks that query bridge state.
	type clientEntry struct {
		id      ClientID
		runtime *ClientRuntime
	}

	b.mu.Lock()
	clients := make([]clientEntry, 0, len(b.clients))
	for id, runtime := range b.clients {
		clients = append(clients, clientEntry{
			id:      id,
			runtime: runtime,
		})
	}
	b.clients = make(map[ClientID]*ClientRuntime)
	b.mu.Unlock()

	var stopErr error
	for _, client := range clients {
		if err := client.runtime.StopAndWait(ctx); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf(
				"stop client %q runtime: %w", client.id, err,
			))
		}

		b.statusTracker.DeregisterClient(client.id)
	}

	return stopErr
}

// Compile-time interface check.
var _ actor.TellOnlyRef[ClientConnMsg] = (*ClientsConnBridge)(nil)
