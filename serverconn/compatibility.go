package serverconn

import (
	"context"
	"errors"

	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
)

// compatibilityError returns the cached permanent version error if the
// connector has transitioned to the terminal INCOMPATIBLE state, or nil while
// it is still COMPATIBLE. Send paths consult this before contacting the edge.
func (a *ServerConnectionActor) compatibilityError() *mailboxconn.StatusError {
	return a.compatErr.Load()
}

// checkPermanentStatus inspects an error returned by an edge operation. If it
// is a permanent version StatusError, it drives the terminal incompatibility
// transition and returns true. Transient and non-status errors return false so
// the existing retry policy still applies.
func (a *ServerConnectionActor) checkPermanentStatus(ctx context.Context,
	err error) bool {

	if err == nil {
		return false
	}

	var statusErr *mailboxconn.StatusError
	if !errors.As(err, &statusErr) || !statusErr.IsPermanentVersion() {
		return false
	}

	a.markIncompatible(ctx, statusErr)

	return true
}

// markIncompatible performs the one-shot transition to the terminal
// INCOMPATIBLE state. It caches the typed error, cancels ingress and heartbeat
// processing asynchronously (so a calling ingress or heartbeat goroutine never
// waits for itself), fails all pending unary waiters with the same error, and
// invokes the optional compatibility callback exactly once. Subsequent calls
// are no-ops.
func (a *ServerConnectionActor) markIncompatible(ctx context.Context,
	statusErr *mailboxconn.StatusError) {

	if statusErr == nil {
		return
	}

	a.compatOnce.Do(func() {
		// Cache the error first so any concurrent send observes the
		// terminal state immediately.
		a.compatErr.Store(statusErr)

		a.log.WarnS(ctx, "Connector became incompatible", statusErr)

		// Cancel ingress and heartbeat without joining: this may run on
		// the ingress goroutine itself. CancelFunc is idempotent.
		if cancel := a.ingressCancel.Load(); cancel != nil {
			(*cancel)()
		}

		// Fail every in-flight unary waiter so no caller blocks on a
		// response that will never arrive.
		a.responseRegistry.FailAll(statusErr)

		// Notify the owner exactly once.
		if a.cfg.OnIncompatible != nil {
			a.cfg.OnIncompatible(statusErr)
		}
	})
}
