//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"time"
)

// reconcileInterval is how often the reconciler re-derives the activity feed
// and re-projects it into the canonical store. It lands pending -> terminal
// transitions — a confirmed boarding deposit, a forfeited cooperative leave, a
// completed unilateral exit — live while the daemon runs, rather than only at
// the next startup backfill. The cadence is deliberately coarse: one pass
// re-queries every history source (plus one GetUnrollStatus per pending
// unilateral exit), and on-chain settlement is not latency-sensitive to the
// second. It is much slower than the credit poll, whose per-op registry read
// is far cheaper.
const reconcileInterval = 60 * time.Second

// startReconcilerLoop starts the activity reconciler goroutine. It is a no-op
// without a canonical store: the derive-on-read fallback already reflects live
// state on every List, so there is nothing to reconcile into.
func (r *Runtime) startReconcilerLoop() {
	if r.deps == nil || r.deps.ActivityStore == nil {
		return
	}

	r.wg.Add(1)
	go r.reconcilerLoop()
}

// reconcilerLoop periodically re-projects the derived feed so terminal
// transitions land in the store live. backfillActivity already ran during
// resumeAll, so the loop starts straight into the ticker rather than
// projecting once immediately. It is anchored to the daemon root context and
// drained by the runtime's wait group on stop.
func (r *Runtime) reconcilerLoop() {
	defer r.wg.Done()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.rootCtx.Done():
			return

		case <-ticker.C:
			r.reconcileActivity(r.rootCtx)
		}
	}
}

// reconcileActivity runs one re-derive-and-project pass, factored out so it can
// be unit-tested directly. A pass failure is a transient history-source RPC
// error, logged at debug: the next tick retries, and ProjectEntry
// change-suppression means a partial pass never corrupts the store.
func (r *Runtime) reconcileActivity(ctx context.Context) {
	if _, err := r.reprojectActivity(ctx); err != nil {
		r.deps.resolveLog().DebugS(
			ctx,
			"Activity reconcile pass failed",
			err,
		)
	}
}
