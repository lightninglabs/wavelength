package vtxo

import (
	"context"
	"sync"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// LazyChainResolver is a forwarding TellOnlyRef for ExpiringNotification
// that allows the real target to be set after the VTXO manager is already
// running. This breaks the init-order dependency between the VTXO manager
// and the unroll subsystem.
//
// Notifications received before the target is wired are buffered and
// replayed when Set() is called. This prevents critical-expiry
// notifications from being dropped during the brief init window.
type LazyChainResolver struct {
	mu       sync.Mutex
	target   actor.TellOnlyRef[ExpiringNotification]
	buffered []bufferedNotification
}

type bufferedNotification struct {
	msg ExpiringNotification
}

// NewLazyChainResolver creates a new lazy chain resolver with no target.
// Call Set() to wire the real destination once the unroll subsystem is
// initialized.
func NewLazyChainResolver() *LazyChainResolver {
	return &LazyChainResolver{}
}

// Set stores the real chain resolver target and replays any buffered
// notifications. Safe to call once from the daemon init path.
func (l *LazyChainResolver) Set(ref actor.TellOnlyRef[ExpiringNotification]) {
	l.mu.Lock()
	l.target = ref
	pending := l.buffered
	l.buffered = nil
	l.mu.Unlock()

	for _, p := range pending {
		// Best-effort replay; errors are non-fatal since the
		// job will be picked up on restart if delivery fails.
		_ = ref.Tell(context.Background(), p.msg)
	}
}

// ID implements actor.BaseActorRef.
func (l *LazyChainResolver) ID() string {
	return "lazy-chain-resolver"
}

// Tell forwards the message to the real target. If the target has not
// been set yet, the notification is buffered for replay when Set() is
// called.
func (l *LazyChainResolver) Tell(ctx context.Context,
	msg ExpiringNotification) error {

	l.mu.Lock()
	t := l.target
	if t == nil {
		l.buffered = append(l.buffered, bufferedNotification{
			msg: msg,
		})
		l.mu.Unlock()

		return nil
	}
	l.mu.Unlock()

	return t.Tell(ctx, msg)
}
