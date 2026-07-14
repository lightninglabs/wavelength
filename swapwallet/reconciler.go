//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
)

// reconcilerKinds are the low-volume, backfill-only kinds the reconciler
// re-projects over their FULL history each pass: a confirmed boarding deposit
// or a completed exit whose pending -> terminal transition has no live
// projector. Their counts stay small (deposits and exits are rare), so a
// full-history scan is cheap. The high-volume SEND/RECV kinds are reconciled
// separately over a bounded window — see rawOORReconcileKinds.
var reconcilerKinds = []walletdkrpc.EntryKind{
	walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
	walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
}

// rawOORReconcileKinds are the kinds reconciled over a bounded recent window
// rather than the full history. A RAW out-of-round send/receive (`ark send oor`
// / `ark oor receive`) is neither swap-backed nor credit-backed, so it has no
// live projector and its ledger-derived row would otherwise reach the store
// only at the next startup backfill (issue #903). But SEND/RECV are the
// highest-volume wallet operations and grow without bound, so paging their
// ENTIRE history every tick would be O(N) work — a ProjectEntry read per row —
// as flagged in review. We therefore reproject only the most recent
// rawOORReconcileWindow rows (see reprojectRecentActivity).
//
// Re-deriving the swap/credit-backed SEND/RECV rows that fall in that window
// alongside the raw ones is safe, not contended: ProjectEntry change
// suppression makes an unchanged row a no-op (no duplicate event, nothing
// fanned to subscribers), so the reconciler never double-emits a row the
// monitor already advanced. And unlike the credit poll — which sees only the
// credit leg and so must never emit for a mixed pay — the reconciler projects
// the fully-merged derived row (the same one List and the startup backfill
// produce), so any transition it lands is authoritative.
//
// SEND must stay paired with RECV here: deriveActivity gates ledger collection
// on DEPOSIT || EXIT || SEND (not RECV), so reconciling RECV without SEND would
// silently derive no ledger rows and the raw-OOR receives would never land.
var rawOORReconcileKinds = []walletdkrpc.EntryKind{
	walletdkrpc.EntryKind_ENTRY_KIND_SEND,
	walletdkrpc.EntryKind_ENTRY_KIND_RECV,
}

// rawOORReconcileWindow bounds the raw-OOR reconcile to the most recent N
// activity rows. A raw OOR settles almost immediately and sorts to the top by
// updated_at, so a modest window reliably catches it before newer sends/
// receives push it out; older SEND/RECV rows are already terminal and
// immutable, so skipping them loses nothing (a restart's full backfill is the
// backstop). 100 balances catch-reliability against the per-tick ProjectEntry
// cost — a deployment sustaining more than 100 sends+receives within one
// reconcileInterval (60s) should raise it.
const rawOORReconcileWindow = 100

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

// reconcileActivity runs the re-derive-and-project passes, factored out so it
// can be unit-tested directly: a full-history pass over the low-volume
// DEPOSIT/EXIT kinds, then a bounded recent-window pass over the high-volume
// SEND/RECV kinds (raw OOR). A pass failure is a transient history-source RPC
// error: the next tick retries, ProjectEntry change-suppression keeps a partial
// pass from appending spurious events, and a pending record is cleared only
// after its terminal row is durably projected, so a partial pass never strands
// or corrupts a row. It is the only live path landing these flips, so a failure
// is logged at warn (not debug) — matching the sibling credit poll and
// backfill.
func (r *Runtime) reconcileActivity(ctx context.Context) {
	// Full-history pass over the low-volume DEPOSIT/EXIT kinds.
	if _, err := r.reprojectActivity(ctx, reconcilerKinds); err != nil {
		r.deps.resolveLog().WarnS(ctx, "Activity reconcile pass failed",
			err,
		)
	}

	// Bounded recent-window pass over the high-volume SEND/RECV kinds so a
	// raw OOR (which has no live projector) lands live without paging the
	// unbounded send/receive history every tick. See rawOORReconcileWindow.
	if _, err := r.reprojectRecentActivity(
		ctx, rawOORReconcileKinds, rawOORReconcileWindow,
	); err != nil {

		r.deps.resolveLog().WarnS(
			ctx,
			"Raw-OOR activity reconcile pass failed",
			err,
		)
	}
}
