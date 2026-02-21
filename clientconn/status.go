package clientconn

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

// StatusTracker provides client liveness information to the server.
// Implementation is deferred to a follow-up — the real tracker will
// derive status from gRPC connection state or a heartbeat mechanism.
type StatusTracker interface {
	// Status returns the current liveness status for a client.
	Status(clientID ClientID) ClientStatus

	// OnStatusChange registers a callback for status transitions.
	// The callback is invoked synchronously from the goroutine that
	// detects the transition.
	OnStatusChange(fn func(clientID ClientID, status ClientStatus))
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

// BridgeOption is a functional option for configuring a ClientsConnBridge.
type BridgeOption func(*bridgeOptions)

// bridgeOptions holds optional configuration for the bridge.
type bridgeOptions struct {
	statusTracker StatusTracker

	// maxClients bounds the number of concurrently registered
	// clients. Zero means unlimited.
	maxClients int
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
