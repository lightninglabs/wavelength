//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
)

// deadlineTickInterval is how often the deadline watcher scans pending
// entries. The watcher tolerates loose timing — a stuck entry transitioning
// a few seconds after its deadline is acceptable — so a coarse tick keeps
// background CPU and lock contention low.
const deadlineTickInterval = 30 * time.Second

// pendingEntry is the minimal record the runtime keeps for each in-flight
// WalletEntry so the deadline watcher can decide whether to overlay a FAILED
// timeout. The underlying swap (or leave) row is the source of truth for
// progress and amounts; the runtime only tracks the wallet-layer deadline
// and the original PENDING start time.
type pendingEntry struct {
	id        string
	kind      walletrpc.EntryKind
	createdAt time.Time
	deadline  time.Time
}

// Runtime owns the swapwallet package's background lifecycle: the unified
// resume-on-startup sweep, the wallet-level deadline watcher, and the
// monitor loop that fans normalized WalletEntry updates to SubscribeWallet
// subscribers. All goroutines anchor to a single rootCtx so daemon shutdown
// cancels them in one step, and so an RPC client disconnect can never
// cancel in-flight work.
type Runtime struct {
	deps *Deps

	// rootCtx is the daemon-lifetime context that every background
	// goroutine inherits.
	rootCtx context.Context //nolint:containedctx

	// cancel stops rootCtx during shutdown.
	cancel context.CancelFunc

	// wg tracks background goroutines so cleanup blocks until they exit.
	wg sync.WaitGroup

	// startOnce makes runtime startup idempotent. The daemon should run
	// the wallet-ready hook once, but this prevents duplicate monitor or
	// deadline goroutines if future wiring accidentally invokes it twice.
	startOnce sync.Once

	// subsMu guards subscribers.
	subsMu sync.Mutex

	// subscribers receive normalized WalletEntry updates from the
	// monitor loop. Channels are best-effort, buffered, and
	// non-blocking: a slow consumer drops updates rather than stalling
	// the runtime. SubscribeWallet handlers reconcile with List on
	// reconnect.
	subscribers map[chan *walletrpc.WalletEntry]struct{}

	// pendingMu guards pending and overlay.
	pendingMu sync.Mutex

	// pending tracks every PENDING WalletEntry by id. The deadline
	// watcher consults it on each tick.
	pending map[string]pendingEntry

	// overlay records wallet-level status elevations for entries the
	// deadline watcher has timed out. The history merger consults this
	// map when computing WalletEntry.status so a stuck row appears
	// FAILED at the wallet layer without mutating the underlying swap
	// or leave state. Cleared when an entry transitions to a terminal
	// status through the monitor loop.
	overlay map[string]overlayStatus
}

// overlayStatus is the runtime's wallet-level overlay applied on top of an
// underlying swap or leave status. It only ever elevates a status to FAILED
// with a synthetic reason; happy-path transitions come from the source row.
type overlayStatus struct {
	status        walletrpc.EntryStatus
	failureReason string
}

// newRuntime builds the runtime owner. It does NOT start background
// goroutines; the caller invokes start so the caller controls ordering
// relative to the swap-backend handshake.
func newRuntime(parent context.Context, deps *Deps) *Runtime {
	rootCtx, cancel := context.WithCancel(parent)

	return &Runtime{
		deps:    deps,
		rootCtx: rootCtx,
		cancel:  cancel,
		subscribers: make(
			map[chan *walletrpc.WalletEntry]struct{},
		),
		pending: make(map[string]pendingEntry),
		overlay: make(map[string]overlayStatus),
	}
}

// start spawns the runtime's background goroutines. The wallet-ready hook
// calls this after the resume sweep so the deadline watcher and monitor loop
// only run once the daemon wallet can service resumed workers. Runtime keeps
// its own guard as a local backstop in case future wiring starts it outside
// the wallet-ready hook.
func (r *Runtime) start() {
	r.startOnce.Do(func() {
		r.wg.Add(1)
		go r.deadlineWatcher()

		r.startMonitorLoop()
	})
}

// resumeAll performs the unified resume sweep during the daemon wallet-ready
// phase. The wallet RPC surface may already be registered while the daemon is
// locked, but wallet-dependent workers must wait until resumed swaps can use
// the unlocked wallet. The implementation is staged across phases; v1
// delegates to the swap backend for the swap-side resume and leaves room for
// additional wallet-managed pending tables.
func (r *Runtime) resumeAll(ctx context.Context) {
	log := r.deps.resolveLog()

	if r.deps.SwapBackend == nil {
		log.WarnS(ctx, "Skipping unified resume sweep",
			ErrSwapBackendUnavailable,
		)

		return
	}

	log.InfoS(ctx, "Running unified swapwallet resume sweep")

	// The swap backend owns the swap-side resume sweep; swapwallet's
	// wallet-level resume responsibilities (deadline re-arming, future
	// wallet-managed pending tables) layer on top in subsequent phases.
	r.deps.SwapBackend.ResumePending(ctx)
}

// trackPending records a new or refreshed pending entry so the deadline
// watcher can age it. The deadline base is the time the RUNTIME first
// observes the entry, NOT the row's original createdAt: a swap backfill
// at daemon startup carries the original swap submit time (possibly
// hours old), and basing the deadline on that would flip every backfilled
// pending row to FAILED(timed_out) within the first deadline tick. The
// deadline is a wallet-process responsibility — it must not carry stale
// clock state from a previous process.
//
// Subsequent trackPending calls for the same id are idempotent: the
// existing first-observed createdAt and deadline are preserved so
// follow-up monitor pushes do not extend the deadline indefinitely.
//
// trackPending intentionally does NOT touch the overlay map: a synthetic
// FAILED overlay set by the deadline watcher must remain visible to
// subscribers until the underlying swap subsystem actually transitions
// to a terminal state. clearPending is the only writer that retires the
// overlay; this keeps a still-pending source row from oscillating the
// wallet surface between FAILED and PENDING on every monitor push.
func (r *Runtime) trackPending(id string, kind walletrpc.EntryKind,
	createdAt time.Time) {

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	if existing, ok := r.pending[id]; ok {
		// Already tracked: preserve the original first-observed
		// createdAt and deadline. Only refresh the kind in case
		// the source row's direction was lazily populated.
		existing.kind = kind
		r.pending[id] = existing

		return
	}

	// First-observed: base the deadline on now, not on the source row's
	// createdAt. This is the load-bearing fix for the
	// restart-resume-and-immediately-time-out failure mode.
	now := time.Now()
	r.pending[id] = pendingEntry{
		id:        id,
		kind:      kind,
		createdAt: createdAt,
		deadline:  now.Add(r.deps.resolveDeadline()),
	}
}

// clearPending removes the runtime record for an entry that has reached a
// terminal status, so the deadline watcher stops ageing it.
func (r *Runtime) clearPending(id string) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	delete(r.pending, id)
	delete(r.overlay, id)
}

// overlayFor returns the wallet-layer overlay for an entry id, if any. The
// history merger calls it when computing WalletEntry.status so a stuck row
// surfaces as FAILED at the wallet layer even when the swap row is still
// progressing.
func (r *Runtime) overlayFor(id string) (overlayStatus, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	ov, ok := r.overlay[id]

	return ov, ok
}

// deadlineWatcher scans pending entries on a coarse tick and elevates any
// entry past its wallet-level deadline to FAILED in the overlay map. The
// watcher does not mutate the underlying swap or leave state — that remains
// the source of truth for protocol-level progress. The wallet user simply
// sees the entry as timed out.
func (r *Runtime) deadlineWatcher() {
	defer r.wg.Done()

	ticker := time.NewTicker(deadlineTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.rootCtx.Done():
			return

		case now := <-ticker.C:
			r.applyDeadlines(now)
		}
	}
}

// applyDeadlines is the body of deadlineWatcher, factored out so unit tests
// can drive it with a fixed clock.
//
// Newly elevated entries are also emitted to SubscribeWallet subscribers so
// long-lived UI consumers see the FAILED transition in real time. The
// hung swap is — by hypothesis — never going to drive a fresh monitor
// push, so without this fan-out the wallet surface would stay PENDING in
// the subscription stream until next List materializes the overlay.
func (r *Runtime) applyDeadlines(now time.Time) {
	timedOut := r.markTimedOut(now)
	for _, entry := range timedOut {
		r.emit(entry)
	}
}

// markTimedOut writes the FAILED overlay for every pending row whose
// deadline has passed and returns synthesized WalletEntry rows for each
// newly elevated entry. The slice is built under pendingMu so the
// caller can fan it out to subscribers without holding the lock (emit
// takes subsMu, and the runtime maintains the invariant that no two
// runtime mutexes are held simultaneously).
func (r *Runtime) markTimedOut(now time.Time) []*walletrpc.WalletEntry {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	var notify []*walletrpc.WalletEntry
	for id, entry := range r.pending {
		if _, alreadyTimedOut := r.overlay[id]; alreadyTimedOut {
			continue
		}
		if now.Before(entry.deadline) {
			continue
		}

		ov := overlayStatus{
			status:        walletrpc.EntryStatus_ENTRY_STATUS_FAILED,
			failureReason: "timed_out",
		}
		r.overlay[id] = ov
		notify = append(notify, &walletrpc.WalletEntry{
			Id:            id,
			Kind:          entry.kind,
			Status:        ov.status,
			FailureReason: ov.failureReason,
			CreatedAtUnix: entry.createdAt.Unix(),
			UpdatedAtUnix: now.Unix(),
		})
	}

	return notify
}

// subscribe registers a channel to receive normalized WalletEntry updates
// from the monitor loop. The caller must drain the channel; the runtime
// drops updates rather than blocking on a slow consumer.
func (r *Runtime) subscribe() chan *walletrpc.WalletEntry {
	ch := make(
		chan *walletrpc.WalletEntry,
		int(
			r.deps.resolveSubscribeBuffer(),
		),
	)

	r.subsMu.Lock()
	r.subscribers[ch] = struct{}{}
	r.subsMu.Unlock()

	return ch
}

// unsubscribe removes a previously-registered subscriber. Safe to call
// multiple times; the channel is closed exactly once.
func (r *Runtime) unsubscribe(ch chan *walletrpc.WalletEntry) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()

	if _, ok := r.subscribers[ch]; !ok {
		return
	}
	delete(r.subscribers, ch)
	close(ch)
}

// emit fans a WalletEntry update out to every subscribed channel using a
// non-blocking send. Slow consumers drop updates; they can reconcile via
// List on reconnect.
func (r *Runtime) emit(entry *walletrpc.WalletEntry) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()

	for ch := range r.subscribers {
		select {
		case ch <- entry:
		default:
		}
	}
}

// stop cancels rootCtx and waits for every background goroutine to exit.
// Safe to call multiple times. After stop, all subscriber channels are
// closed so blocked consumers wake.
func (r *Runtime) stop() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()

	r.subsMu.Lock()
	for ch := range r.subscribers {
		delete(r.subscribers, ch)
		close(ch)
	}
	r.subsMu.Unlock()
}
