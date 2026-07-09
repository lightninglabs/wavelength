//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
	kind      walletdkrpc.EntryKind
	createdAt time.Time
	deadline  time.Time
	noTimeout bool
	entry     *walletdkrpc.WalletEntry
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

	// projectMu serializes project-then-emit so the event_seq a
	// transition is assigned and the emit that carries it stay in the same
	// order across the concurrent producers (monitor, reconciler, credit
	// poll, deadline watcher, RPC handlers). Without it a later-committed
	// but lower-seq event could emit after a higher one, and the live
	// cursor would advance past it and drop it silently.
	projectMu sync.Mutex

	// subsMu guards subscribers.
	subsMu sync.Mutex

	// subscribers receive activity updates from the projection paths.
	// Each update carries the event_seq its transition was assigned so a
	// SubscribeWallet stream can hand the consumer a resumable cursor.
	// Sends are non-blocking: a slow consumer's buffer fills and its
	// overflowed flag is set rather than dropping silently, so the handler
	// signals a gap and the consumer reconciles via List.
	subscribers map[*subscriber]struct{}

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

	// rehydratePageSize is the keyset page size for the startup
	// pending-EXIT rehydration scan. A field (not a const) so tests can
	// drive multi-page paths without seeding hundreds of rows; newRuntime
	// defaults it to defaultRehydratePageSize.
	rehydratePageSize int32
}

// overlayStatus is the runtime's wallet-level overlay applied on top of an
// underlying swap or leave status. It only ever elevates a status to FAILED
// with a synthetic reason; happy-path transitions come from the source row.
type overlayStatus struct {
	status        walletdkrpc.EntryStatus
	failureReason string
	failureCode   walletdkrpc.EntryFailureCode
}

// timedOutCode is the failure code the deadline overlay stamps on entries it
// elevates to FAILED.
const timedOutCode = walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_TIMED_OUT

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
			map[*subscriber]struct{},
		),
		pending:           make(map[string]pendingEntry),
		overlay:           make(map[string]overlayStatus),
		rehydratePageSize: defaultRehydratePageSize,
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
		r.startCreditProjectorLoop()
		r.startReconcilerLoop()
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

	// Restore wallet-local PENDING EXIT rows from the store into the
	// in-memory pending map before anything else. This keeps the
	// cooperative-leave forfeit->COMPLETE correlation restart-survivable
	// (see rehydrateWalletLocalPending), and runs before backfillActivity
	// so a leave that sealed while the daemon was down completes in the
	// single startup pass. It depends only on the store, so it must run
	// even when no swap backend is configured (below).
	r.rehydrateWalletLocalPending(ctx)

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

	// Seed the canonical activity log (issue #774) from the current derived
	// state so the store reflects every existing operation before the first
	// live transition is projected. Idempotent on canonical_id; a no-op
	// when no activity store is wired.
	r.backfillActivity(ctx)
}

// defaultRehydratePageSize is the keyset page size for the pending-EXIT
// rehydration scan. Pending leaves are few, so a modest page keeps the
// one-shot startup scan cheap.
const defaultRehydratePageSize = 256

// rehydrateWalletLocalPending re-tracks wallet-local PENDING EXIT rows from the
// canonical store into the in-memory pending map at startup.
//
// A cooperative-leave EXIT is projected durably at submit, keyed by its stable
// send_job_id and carrying the retained consumed outpoint. But its
// pending->COMPLETE flip is driven by matching that outpoint against the
// forfeited-VTXO set, and the only source the completion pass reads is the
// in-memory pending map — which a restart empties, since the map is populated
// only by the (RPC-driven) submit path. Without re-tracking, a leave that
// sealed while the daemon was down, or was interrupted by any mid-flight
// restart, is stranded PENDING in the store forever.
//
// Re-tracking restores the correlation from the durable store row so the
// existing derive/reconcile pass flips it COMPLETE under its stable id. Only
// EXIT rows are scanned: SEND/RECV are owned by the live monitor and DEPOSIT
// re-derives from the ledger. A unilateral EXIT (outpoint-keyed) is re-tracked
// too — redundant but harmless (it re-derives to the same id and dedupes).
//
// The scan is filtered to PENDING EXIT in SQL and paged by the unique
// canonical_id, so it reads only the matching rows (not the whole feed) and its
// cursor is strictly monotonic — a full page always advances it.
func (r *Runtime) rehydrateWalletLocalPending(ctx context.Context) {
	if r.deps == nil || r.deps.ActivityStore == nil {
		return
	}

	log := r.deps.resolveLog()

	pageSize := r.rehydratePageSize
	if pageSize <= 0 {
		pageSize = defaultRehydratePageSize
	}

	var (
		cursorID string
		restored int
	)
	for {
		batch, err := r.deps.ActivityStore.ListEntriesByKindStatus(
			ctx, int64(walletdkrpc.EntryKind_ENTRY_KIND_EXIT),
			int64(walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING),
			cursorID, pageSize,
		)
		if err != nil {
			log.WarnS(ctx, "Pending-EXIT rehydration scan failed",
				err,
			)

			return
		}
		if len(batch) == 0 {
			break
		}

		for _, row := range batch {
			cursorID = row.CanonicalID

			entry, err := rowToWalletEntry(row)
			if err != nil {
				log.WarnS(ctx, "Skipping undecodable activity "+
					"row during rehydration", err,
					slog.String("id", row.CanonicalID),
				)

				continue
			}

			r.trackPendingEntryWithoutTimeout(entry)
			restored++
		}

		if len(batch) < int(pageSize) {
			break
		}
	}

	if restored > 0 {
		log.InfoS(ctx, "Rehydrated wallet-local pending EXIT rows",
			slog.Int("count", restored),
		)
	}
}

// trackPending records a new or refreshed wallet-local pending entry so the
// deadline watcher can age it. The deadline base is the time the runtime first
// observes the entry, NOT the row's original createdAt; otherwise restored
// entries could be projected to FAILED(timed_out) within the first deadline
// tick after restart.
//
// Subsequent trackPending calls for the same id are idempotent: the
// existing first-observed createdAt and deadline are preserved so
// follow-up monitor pushes do not extend the deadline indefinitely.
//
// trackPending intentionally does NOT touch the overlay map: a synthetic
// FAILED overlay set by the deadline watcher must remain visible to
// subscribers until the underlying wallet-local operation is cleared.
func (r *Runtime) trackPending(id string, kind walletdkrpc.EntryKind,
	createdAt time.Time) {

	r.trackPendingEntryAt(&walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		CreatedAtUnix: createdAt.Unix(),
		UpdatedAtUnix: createdAt.Unix(),
	}, createdAt, false)
}

// trackPendingEntry records a wallet-local pending activity row. Unlike swap
// rows, these entries do not have a durable swap FSM feeding List/Subscribe
// updates, so the runtime keeps the friendly WalletEntry snapshot that should
// appear in activity until a backing ledger or status source supersedes it.
func (r *Runtime) trackPendingEntry(entry *walletdkrpc.WalletEntry) {
	if entry == nil || entry.GetId() == "" {
		return
	}

	r.trackPendingEntryAt(
		entry,
		unixToTime(
			entry.GetCreatedAtUnix(),
		),
		false,
	)
}

// trackPendingEntryWithoutTimeout records a wallet-local pending activity row
// that should remain pending until a source of truth supersedes it. Cooperative
// leave uses this because a successful on-chain round can take longer than the
// generic wallet deadline, and the v1 activity layer cannot yet correlate the
// pending outpoint id to the later confirmed sweep txid.
func (r *Runtime) trackPendingEntryWithoutTimeout(
	entry *walletdkrpc.WalletEntry) {

	if entry == nil || entry.GetId() == "" {
		return
	}

	r.trackPendingEntryAt(
		entry,
		unixToTime(
			entry.GetCreatedAtUnix(),
		),
		true,
	)
}

// trackPendingEntryAt is the shared implementation for callers that already
// have a precise submit time and callers that only have a WalletEntry unix
// timestamp.
func (r *Runtime) trackPendingEntryAt(entry *walletdkrpc.WalletEntry,
	createdAt time.Time, noTimeout bool) {

	if entry == nil || entry.GetId() == "" {
		return
	}

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	entryCopy := cloneWalletEntry(entry)
	if entryCopy.Status == walletdkrpc.EntryStatus_ENTRY_STATUS_UNSPECIFIED {
		entryCopy.Status = walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING
	}

	if entryCopy.GetCreatedAtUnix() == 0 {
		entryCopy.CreatedAtUnix = createdAt.Unix()
	}
	if entryCopy.GetUpdatedAtUnix() == 0 {
		entryCopy.UpdatedAtUnix = entryCopy.GetCreatedAtUnix()
	}

	if existing, ok := r.pending[entryCopy.GetId()]; ok {
		// Already tracked: preserve the original first-observed
		// timestamps and deadline. Refresh the rest of the snapshot
		// in case the source row's direction or metadata was lazily
		// populated.
		existing.kind = entryCopy.GetKind()
		if existing.entry != nil {
			if existing.entry.GetCreatedAtUnix() != 0 {
				entryCopy.CreatedAtUnix = existing.entry.
					GetCreatedAtUnix()
			}
			if existing.entry.GetUpdatedAtUnix() != 0 {
				entryCopy.UpdatedAtUnix = existing.entry.
					GetUpdatedAtUnix()
			}
		}
		existing.entry = entryCopy
		existing.noTimeout = noTimeout
		r.pending[entryCopy.GetId()] = existing

		return
	}

	// First-observed: base the deadline on now, not on the source row's
	// createdAt. This is the load-bearing fix for the
	// restart-resume-and-immediately-time-out failure mode.
	now := time.Now()
	r.pending[entryCopy.GetId()] = pendingEntry{
		id:        entryCopy.GetId(),
		kind:      entryCopy.GetKind(),
		createdAt: createdAt,
		deadline:  now.Add(r.deps.resolveDeadline()),
		noTimeout: noTimeout,
		entry:     entryCopy,
	}
}

// pendingSnapshot returns copies of the wallet-local pending entries that
// should be included in List/Subscribe snapshots.
func (r *Runtime) pendingSnapshot() []*walletdkrpc.WalletEntry {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	out := make([]*walletdkrpc.WalletEntry, 0, len(r.pending))
	for _, pending := range r.pending {
		entry := cloneWalletEntry(pending.entry)
		if entry == nil {
			entry = &walletdkrpc.WalletEntry{
				Id:            pending.id,
				Kind:          pending.kind,
				Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
				CreatedAtUnix: pending.createdAt.Unix(),
				UpdatedAtUnix: pending.createdAt.Unix(),
			}
		}
		out = append(out, entry)
	}

	return out
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

// deadlineWatcher scans pending wallet-local entries on a coarse tick and
// elevates any entry past its wallet-level deadline to FAILED in the overlay
// map. Swap rows use the swap FSM's own terminal state instead.
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
// long-lived UI consumers see the FAILED transition in real time.
func (r *Runtime) applyDeadlines(now time.Time) {
	timedOut := r.markTimedOut(now)
	for _, entry := range timedOut {
		r.projectAndEmit(r.rootCtx, entry)
	}
}

// markTimedOut writes the FAILED overlay for every wallet-local pending row
// whose deadline has passed and returns synthesized WalletEntry rows for each
// newly elevated entry. Swap rows are dropped from tracking so they cannot
// diverge from the swap FSM.
func (r *Runtime) markTimedOut(now time.Time) []*walletdkrpc.WalletEntry {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	var notify []*walletdkrpc.WalletEntry
	for id, entry := range r.pending {
		if isSwapKind(entry.kind) {
			delete(r.pending, id)
			delete(r.overlay, id)

			continue
		}
		if _, alreadyTimedOut := r.overlay[id]; alreadyTimedOut {
			continue
		}
		if entry.noTimeout {
			continue
		}
		if now.Before(entry.deadline) {
			continue
		}

		ov := overlayStatus{
			status:        walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
			failureReason: "timed_out",
			failureCode:   timedOutCode,
		}
		r.overlay[id] = ov
		notifyEntry := cloneWalletEntry(entry.entry)
		if notifyEntry == nil {
			notifyEntry = &walletdkrpc.WalletEntry{
				Id:            id,
				Kind:          entry.kind,
				CreatedAtUnix: entry.createdAt.Unix(),
			}
		}
		notifyEntry.Status = ov.status
		notifyEntry.FailureReason = ov.failureReason
		notifyEntry.FailureCode = failureCodePtr(ov.failureCode)
		notifyEntry.UpdatedAtUnix = now.Unix()
		notify = append(notify, notifyEntry)
	}

	return notify
}

// cloneWalletEntry returns a shallow copy with mutable nested fields copied
// where the runtime updates them. Request oneofs are immutable after entry
// creation in this package, so a shallow copy is sufficient for those.
func cloneWalletEntry(entry *walletdkrpc.WalletEntry) *walletdkrpc.WalletEntry {
	if entry == nil {
		return nil
	}

	entryCopy := *entry
	if entry.GetProgress() != nil {
		progress := *entry.GetProgress()
		entryCopy.Progress = &progress
	}

	return &entryCopy
}

// isSwapKind reports pinned SEND/RECV rows whose lifecycle is backed by the
// swap FSM. SubscribeSwaps monitor updates are even stricter: every row from
// that stream is treated as swap-backed even when its direction is still
// UNSPECIFIED.
func isSwapKind(kind walletdkrpc.EntryKind) bool {
	return kind == walletdkrpc.EntryKind_ENTRY_KIND_SEND ||
		kind == walletdkrpc.EntryKind_ENTRY_KIND_RECV
}

// subscribeUpdate is one activity update fanned to a subscriber: the projected
// WalletEntry plus the event_seq its transition was assigned, which a
// SubscribeWallet stream surfaces as the resumable cursor.
type subscribeUpdate struct {
	seq   int64
	entry *walletdkrpc.WalletEntry
}

// subscriber is a registered SubscribeWallet consumer. Updates arrive on ch;
// when a send would block (the consumer is slower than the producer),
// overflowed is set instead of dropping silently, so the handler can signal a
// gap and let the consumer reconcile via List.
type subscriber struct {
	ch         chan subscribeUpdate
	overflowed atomic.Bool
}

// subscribe registers a consumer to receive activity updates. The caller must
// drain the channel and unsubscribe when done.
func (r *Runtime) subscribe() *subscriber {
	sub := &subscriber{
		ch: make(
			chan subscribeUpdate,
			int(
				r.deps.resolveSubscribeBuffer(),
			),
		),
	}

	r.subsMu.Lock()
	r.subscribers[sub] = struct{}{}
	r.subsMu.Unlock()

	return sub
}

// unsubscribe removes a previously-registered subscriber. Safe to call
// multiple times; the channel is closed exactly once.
func (r *Runtime) unsubscribe(sub *subscriber) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()

	if _, ok := r.subscribers[sub]; !ok {
		return
	}
	delete(r.subscribers, sub)
	close(sub.ch)
}

// emit fans one activity update out to every subscriber with a non-blocking
// send. A consumer whose buffer is full has its overflowed flag set rather
// than losing the update silently: the handler turns that into a gap signal so
// the consumer reconciles via List and resumes from its cursor.
func (r *Runtime) emit(seq int64, entry *walletdkrpc.WalletEntry) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()

	for sub := range r.subscribers {
		// Each subscriber gets its own copy: the handler goroutines
		// marshal their responses concurrently, and Marshal mutates a
		// *WalletEntry's internal size cache, so a shared pointer would
		// race across two subscribers (e.g. CLI + app).
		update := subscribeUpdate{
			seq:   seq,
			entry: cloneWalletEntry(entry),
		}
		select {
		case sub.ch <- update:
		default:
			sub.overflowed.Store(true)
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
	for sub := range r.subscribers {
		delete(r.subscribers, sub)
		close(sub.ch)
	}
	r.subsMu.Unlock()
}
