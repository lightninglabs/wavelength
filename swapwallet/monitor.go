//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"google.golang.org/grpc/metadata"
)

// monitorBackoffMin is the smallest backoff used after a SubscribeSwaps
// failure. Subsequent failures double the backoff up to monitorBackoffMax.
// monitorEscalateAfter caps consecutive WarnS log lines before the loop
// emits a single Error-level escalation; without this the loop would
// produce unbounded warn-level log spam on a permanently broken swap
// subsystem.
const (
	monitorBackoffMin    = 500 * time.Millisecond
	monitorBackoffMax    = 30 * time.Second
	monitorEscalateAfter = 10
)

// startMonitorLoop spawns the background goroutine that consumes swap
// updates from the in-process swap subserver and fans them out to wallet
// subscribers as normalized WalletEntry rows. Resilient to transient
// upstream errors: failures back off exponentially up to monitorBackoffMax.
func (r *Runtime) startMonitorLoop() {
	r.wg.Add(1)
	go r.monitorLoop()
}

// monitorLoop is the runtime's swap-update consumer. It subscribes to
// SwapService.SubscribeSwaps in-process via the inlineSwapStream shim,
// normalizes each update into a WalletEntry, projects it onto the runtime's
// canonical-id intent map, and fans the result out to every channel
// registered through subscribe(). The loop survives transient upstream
// errors with bounded exponential backoff and only exits on rootCtx
// cancellation (daemon shutdown).
//
// After monitorEscalateAfter consecutive failures the loop emits ONE
// ErrorS escalation so a permanently broken swap subsystem produces a
// loud operator signal exactly once instead of unbounded WarnS spam.
// Subsequent successful attempts reset the counter so transient flapping
// does not re-trigger the alarm.
func (r *Runtime) monitorLoop() {
	defer r.wg.Done()

	log := r.deps.resolveLog()
	if r.deps.SwapService == nil {
		log.WarnS(r.rootCtx, "Monitor loop disabled",
			ErrSwapBackendUnavailable,
		)

		return
	}

	// includeExisting requests a full snapshot of currently-persisted
	// swap rows from the subserver. We need it on the FIRST subscribe so
	// resume-replay reaches subscribers; on every subsequent reconnect
	// (after a transient failure) replaying the whole history would just
	// re-fan-out terminal-state events that subscribers have already
	// seen. Flip to false after the first attempt completes (cleanly or
	// not) so reconnect noise stays quiet.
	includeExisting := true
	backoff := monitorBackoffMin
	consecutiveFailures := 0
	escalated := false
	for {
		if r.rootCtx.Err() != nil {
			return
		}

		err := r.runOneSubscription(includeExisting)
		includeExisting = false
		switch {
		case err == nil:
			// Clean stream end: reset backoff and the failure
			// counter so a flaky upstream that briefly drops the
			// connection does not penalize the next try and a
			// recovered upstream re-arms the escalation alarm.
			backoff = monitorBackoffMin
			consecutiveFailures = 0
			escalated = false

		case errors.Is(err, context.Canceled),
			errors.Is(err, io.EOF):

			// Shutdown path: don't log noise.

		default:
			consecutiveFailures++

			// Always emit per-attempt WarnS so operators tailing
			// logs see each failure. After the escalation
			// threshold emit one additional WarnS with a sticky
			// marker so monitoring filters can pull "persistent
			// failure" out of the noise — without escalating to
			// ErrorS, which the project reserves for internal
			// bugs (a broken swap subsystem is external).
			log.WarnS(
				r.rootCtx,
				"Swap subscribe failed; backing off",
				err,
			)
			if consecutiveFailures >= monitorEscalateAfter &&
				!escalated {

				log.WarnS(
					r.rootCtx,
					"Swap subscribe failing "+
						"persistently; swap "+
						"subsystem may be down",
					err,
				)
				escalated = true
			}

			select {
			case <-r.rootCtx.Done():
				return

			case <-time.After(backoff):
			}

			backoff *= 2
			if backoff > monitorBackoffMax {
				backoff = monitorBackoffMax
			}
		}
	}
}

// runOneSubscription invokes SubscribeSwaps once and forwards every
// pushed update to fanOutSwapUpdate. The method returns when the swap
// service ends the stream (clean nil), when rootCtx is canceled, or
// when the inline stream forwards an error from Send.
func (r *Runtime) runOneSubscription(includeExisting bool) error {
	stream := newInlineSwapStream(r.rootCtx, r.fanOutSwapUpdate)

	// SubscribeSwaps is implemented as a server-streaming gRPC handler;
	// in-process we call it as a plain Go method with our inline stream
	// adapter. The call blocks until the service returns, which happens
	// when our stream's Send returns an error (rootCtx canceled) or the
	// service shuts down.
	err := r.deps.SwapService.SubscribeSwaps(
		&swapclientrpc.SubscribeSwapsRequest{
			IncludeExisting: includeExisting,
		},
		stream,
	)

	// rootCtx cancellation maps to context.Canceled; treat io.EOF as a
	// clean end as well so callers don't trip the backoff on it.
	if errors.Is(err, context.Canceled) || err == io.EOF {
		return nil
	}

	return err
}

// fanOutSwapUpdate translates one SubscribeSwapsResponse into a WalletEntry,
// projects the canonical id, clears any stale wallet timeout overlay for
// swap-backed rows, and emits to subscribers. Returns an error only when
// rootCtx has been canceled; transient errors do not stop the loop.
func (r *Runtime) fanOutSwapUpdate(
	resp *swapclientrpc.SubscribeSwapsResponse) error {

	if r.rootCtx.Err() != nil {
		return r.rootCtx.Err()
	}

	summary := resp.GetSwap()
	if summary == nil {
		return nil
	}

	// The monitor consumes summaries for swaps in both directions, so we
	// let direction-derivation pick the kind here (callers that need a
	// pinned kind — router.sendInvoice and recv.go — pass it explicitly).
	// entry.Id is the swap row's payment_hash, which is the stable
	// canonical id across the swap lifecycle — no further projection.
	entry := swapEntryFromSummary(
		summary, "", "", walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
	)

	// All SubscribeSwaps rows are swap-backed, including the first lazy
	// summaries whose direction still normalizes to UNSPECIFIED. The swap
	// FSM owns their terminal state, so wallet-local overlays are never
	// applied here.
	sourceStatus := entry.GetStatus()

	// Keep the pending tracker in sync. Pending swap rows are explicitly
	// cleared so stale wallet overlays cannot outlive the swap FSM source
	// of truth.
	switch sourceStatus {
	case walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING:
		r.clearPending(entry.GetId())

	case walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED:

		r.clearPending(entry.GetId())
	}

	r.projectAndEmit(r.rootCtx, entry)

	return nil
}

// inlineSwapStream adapts SwapClientService.SubscribeSwaps's
// server-streaming gRPC server interface into a plain Go callback so the
// daemon can consume swap updates in-process without dialing itself. Every
// Send delegates to the registered callback; metadata operations are
// no-ops (the in-process consumer has no transport).
type inlineSwapStream struct {
	ctx context.Context //nolint:containedctx

	// send is invoked once per pushed SubscribeSwapsResponse. Returning
	// an error from send causes the gRPC handler to terminate its
	// streaming loop, which is how we unwind on rootCtx cancellation.
	send func(*swapclientrpc.SubscribeSwapsResponse) error
}

// newInlineSwapStream constructs an in-process server-stream adapter
// bound to the given context and per-message callback.
func newInlineSwapStream(ctx context.Context,
	send func(*swapclientrpc.SubscribeSwapsResponse) error,
) *inlineSwapStream {

	return &inlineSwapStream{ctx: ctx, send: send}
}

// Send forwards the response to the callback. When the callback returns
// an error (typically rootCtx cancellation) the gRPC service-side loop
// observes it and terminates.
func (s *inlineSwapStream) Send(
	resp *swapclientrpc.SubscribeSwapsResponse) error {

	return s.send(resp)
}

// Context returns the stream context. The swap service uses this for
// cancellation signalling.
func (s *inlineSwapStream) Context() context.Context {
	return s.ctx
}

// SetHeader is a no-op for the in-process consumer. The wallet layer
// does not propagate gRPC metadata.
func (s *inlineSwapStream) SetHeader(metadata.MD) error { return nil }

// SendHeader is a no-op for the in-process consumer.
func (s *inlineSwapStream) SendHeader(metadata.MD) error { return nil }

// SetTrailer is a no-op for the in-process consumer.
func (s *inlineSwapStream) SetTrailer(metadata.MD) {}

// SendMsg satisfies the grpc.ServerStream interface. In our wiring all
// pushes go through Send; SendMsg is only invoked by the generic
// gRPC framework codepaths we do not exercise here.
func (s *inlineSwapStream) SendMsg(m any) error {
	resp, ok := m.(*swapclientrpc.SubscribeSwapsResponse)
	if !ok {
		return errors.New("inlineSwapStream: unexpected message type")
	}

	return s.Send(resp)
}

// RecvMsg satisfies the grpc.ServerStream interface. The wallet-layer
// consumer never receives on the stream because SubscribeSwaps is
// server-streaming only.
func (s *inlineSwapStream) RecvMsg(any) error {
	return io.EOF
}
