package clientconn

import (
	"context"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
)

// ClientStatus represents the liveness state of a connected client as
// observed by the server. This is informational only — messages are
// always delivered to the mailbox regardless of status (async-first
// model). Status is useful for logging, metrics, and UI display.
type ClientStatus int

const (
	// StatusUnknown indicates the client's liveness is not known.
	// This is the default when no StatusTracker is configured.
	StatusUnknown ClientStatus = iota

	// StatusOnline indicates the client is actively connected and
	// pulling from its mailbox.
	StatusOnline

	// StatusOffline indicates the client is not currently connected.
	// Messages sent while offline accumulate in the mailbox and are
	// delivered when the client next connects.
	StatusOffline
)

// String returns a human-readable representation of the client status.
func (s ClientStatus) String() string {
	switch s {
	case StatusOnline:
		return "online"

	case StatusOffline:
		return "offline"

	default:
		return "unknown"
	}
}

// StatusTracker provides client liveness information to the server. A
// tracker also receives inbound activity and client lifecycle signals
// from the bridge so it can derive status transitions. Trackers that do
// not use those signals can implement the methods as no-ops.
type StatusTracker interface {
	ActivityMarker
	ClientRegistrar

	// Status returns the current liveness status for a client.
	Status(clientID ClientID) ClientStatus

	// OnStatusChange registers a callback for status transitions.
	// The callback is invoked synchronously from the goroutine that
	// detects the transition.
	OnStatusChange(fn func(clientID ClientID, status ClientStatus))
}

// ActivityMarker records inbound client activity. The ingress loop calls
// MarkActive after successfully dispatching an inbound envelope.
type ActivityMarker interface {
	// MarkActive records that the given client has been observed
	// sending traffic. Implementations must be safe for concurrent
	// use from multiple ingress goroutines.
	MarkActive(clientID ClientID)
}

// ClientRegistrar receives client lifecycle notifications from the
// bridge.
type ClientRegistrar interface {
	// RegisterClient initialises tracking state for a new client.
	RegisterClient(clientID ClientID)

	// DeregisterClient removes tracking state for a departing client.
	DeregisterClient(clientID ClientID)
}

// noopStatusTracker is the default StatusTracker that always returns
// StatusUnknown. It is used when no real tracker is wired.
type noopStatusTracker struct{}

// Status always returns StatusUnknown.
func (n *noopStatusTracker) Status(_ ClientID) ClientStatus {
	return StatusUnknown
}

// OnStatusChange is a no-op — the noop tracker never fires transitions.
func (n *noopStatusTracker) OnStatusChange(
	_ func(ClientID, ClientStatus),
) {
}

// MarkActive is a no-op for the default tracker.
func (n *noopStatusTracker) MarkActive(_ ClientID) {
}

// RegisterClient is a no-op for the default tracker.
func (n *noopStatusTracker) RegisterClient(_ ClientID) {
}

// DeregisterClient is a no-op for the default tracker.
func (n *noopStatusTracker) DeregisterClient(_ ClientID) {
}

// BridgeOption is a functional option for configuring a ClientsConnBridge.
type BridgeOption func(*bridgeOptions)

// UnknownClientHandler is called by HandleInbound when an envelope
// arrives from a sender that is not registered on the bridge. The
// handler is responsible for building a PerClientConfig and calling
// RegisterClient (typically via RegisterClientWithAllDispatchers).
//
// Implementations must be safe for concurrent use. The bridge
// deduplicates concurrent calls for the same clientID via
// singleflight, so the handler itself does not need its own locking.
type UnknownClientHandler interface {
	// HandleUnknownClient registers a previously unseen client on
	// the bridge. The envelope that triggered detection is passed
	// so the handler can extract mailbox IDs and protocol version.
	HandleUnknownClient(ctx context.Context,
		clientID ClientID, env *mailboxpb.Envelope) error
}

// bridgeOptions holds optional configuration for the bridge.
type bridgeOptions struct {
	statusTracker StatusTracker

	// maxClients bounds the number of concurrently registered
	// clients. Zero means unlimited.
	maxClients int

	// onUnknownClient is called when HandleInbound receives an
	// envelope from an unregistered sender. If nil, unknown
	// clients are silently ignored.
	onUnknownClient UnknownClientHandler
}

// WithStatusTracker configures the bridge to use the given StatusTracker
// for client liveness queries. If not set, a noopStatusTracker is used.
// A nil tracker is silently ignored, preserving the default noop.
func WithStatusTracker(tracker StatusTracker) BridgeOption {
	return func(o *bridgeOptions) {
		if tracker != nil {
			o.statusTracker = tracker
		}
	}
}

// WithMaxClients bounds the number of concurrently registered clients.
// RegisterClient returns an error when the limit is reached. Zero or
// negative values mean unlimited (the default).
func WithMaxClients(n int) BridgeOption {
	return func(o *bridgeOptions) {
		o.maxClients = n
	}
}

// WithOnUnknownClient configures the bridge to call the given handler
// when HandleInbound receives an envelope from an unregistered sender.
// A nil handler is silently ignored, preserving the default no-op.
func WithOnUnknownClient(h UnknownClientHandler) BridgeOption {
	return func(o *bridgeOptions) {
		if h != nil {
			o.onUnknownClient = h
		}
	}
}
