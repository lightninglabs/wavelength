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

// walletIntent records the canonical wallet-layer id for one user-submitted
// operation along with the secondary keys the history merger uses to project
// future ledger rows onto the same id. Without this map, a Send returning
// id=X would surface as a different id when the same operation later
// produces a ledger row, breaking caller-side correlation.
type walletIntent struct {
	// canonicalID is the stable id the caller saw at submit time and
	// will continue to see in every subsequent List/Subscribe row that
	// originates from the same operation.
	canonicalID string

	// kind is the user-visible category. Used to assert that a ledger
	// projection matches the original intent.
	kind walletrpc.EntryKind

	// outpoints captures the queued VTXO outpoints for an EXIT intent
	// so a later sweep ledger row whose source rows are these outpoints
	// can be projected onto canonicalID.
	outpoints map[string]struct{}

	// address is the boarding address tied to a DEPOSIT intent. A
	// later boarding-sweep or boarding-confirmation ledger row that
	// references this address projects onto canonicalID.
	address string

	// paymentHash is the swap-side correlator for SEND and RECV
	// intents. The swap subsystem's own rows already use the payment
	// hash as id, so this also acts as the projection key.
	paymentHash string

	// txid is filled in once the underlying operation observes an
	// onchain or OOR txid; subsequent ledger rows keyed by the same
	// txid project onto canonicalID.
	txid string
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

	// intentsMu guards the intent indices.
	intentsMu sync.RWMutex

	// intentsByCanonical is the canonical-id -> intent index. Send,
	// Recv, and Deposit handlers register entries here; the history
	// merger consults the secondary indices below to resolve a ledger
	// row to its canonical id.
	intentsByCanonical map[string]*walletIntent

	// intentsByOutpoint maps a VTXO outpoint -> canonical EXIT intent
	// id so a future sweep ledger row whose source rows are these
	// outpoints projects onto the original intent.
	intentsByOutpoint map[string]string

	// intentsByAddress maps a boarding address -> canonical DEPOSIT
	// intent id so a later boarding-confirmation ledger row references
	// the original deposit intent rather than a synthetic id.
	intentsByAddress map[string]string

	// intentsByPaymentHash maps a payment_hash -> canonical SEND/RECV
	// intent id. Swap-side rows already key by payment hash, so this
	// index also serves as the swap projection key.
	intentsByPaymentHash map[string]string

	// intentsByTxid maps a txid -> canonical intent id. Populated as
	// the runtime observes broadcast transactions for an intent so
	// later ledger rows keyed by txid project onto the original.
	intentsByTxid map[string]string
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
		pending:              make(map[string]pendingEntry),
		overlay:              make(map[string]overlayStatus),
		intentsByCanonical:   make(map[string]*walletIntent),
		intentsByOutpoint:    make(map[string]string),
		intentsByAddress:     make(map[string]string),
		intentsByPaymentHash: make(map[string]string),
		intentsByTxid:        make(map[string]string),
	}
}

// start spawns the runtime's background goroutines. Called by Register
// after the synchronous resume sweep so the daemon never accepts a wallet
// RPC before the deadline watcher is running and the monitor loop is
// already consuming swap updates from the in-process swap subserver.
func (r *Runtime) start() {
	r.wg.Add(1)
	go r.deadlineWatcher()

	r.startMonitorLoop()
}

// resumeAll performs the unified resume sweep. It MUST run synchronously
// before the gRPC server begins accepting wallet RPCs so the daemon never
// surfaces a PENDING entry it is not actively driving. The implementation is
// staged across phases; v1 delegates to the swap backend for the swap-side
// resume and leaves room for additional wallet-managed pending tables.
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

// registerSendInvoiceIntent records a SEND intent submitted via Send
// with an invoice destination. The canonical id is the swap payment hash,
// which is also what the swap subsystem persists, so the index is mostly
// a no-op projection — but it lets the history merger treat swap rows
// and runtime-tracked rows uniformly.
func (r *Runtime) registerSendInvoiceIntent(canonicalID string) {
	r.intentsMu.Lock()
	defer r.intentsMu.Unlock()

	in := &walletIntent{
		canonicalID: canonicalID,
		kind:        walletrpc.EntryKind_ENTRY_KIND_SEND,
		paymentHash: canonicalID,
	}
	r.intentsByCanonical[canonicalID] = in
	r.intentsByPaymentHash[canonicalID] = canonicalID
}

// registerRecvIntent records a RECV intent submitted via Recv. Same
// canonical-id semantics as registerSendInvoiceIntent.
func (r *Runtime) registerRecvIntent(canonicalID string) {
	r.intentsMu.Lock()
	defer r.intentsMu.Unlock()

	in := &walletIntent{
		canonicalID: canonicalID,
		kind:        walletrpc.EntryKind_ENTRY_KIND_RECV,
		paymentHash: canonicalID,
	}
	r.intentsByCanonical[canonicalID] = in
	r.intentsByPaymentHash[canonicalID] = canonicalID
}

// registerExitIntent records an EXIT intent submitted via Send with an
// onchain destination. The canonical id is the leave-job id returned by
// LeaveVTXOs (or the first queued outpoint as a fallback). The runtime
// indexes the queued outpoints so a later sweep ledger row whose source
// rows are these outpoints projects back onto canonicalID.
func (r *Runtime) registerExitIntent(canonicalID string,
	queuedOutpoints []string) {

	r.intentsMu.Lock()
	defer r.intentsMu.Unlock()

	out := make(map[string]struct{}, len(queuedOutpoints))
	for _, op := range queuedOutpoints {
		out[op] = struct{}{}
		r.intentsByOutpoint[op] = canonicalID
	}

	r.intentsByCanonical[canonicalID] = &walletIntent{
		canonicalID: canonicalID,
		kind:        walletrpc.EntryKind_ENTRY_KIND_EXIT,
		outpoints:   out,
	}
}

// registerDepositIntent records a DEPOSIT intent submitted via Deposit
// with the freshly minted boarding address as the secondary key.
func (r *Runtime) registerDepositIntent(canonicalID, address string) {
	r.intentsMu.Lock()
	defer r.intentsMu.Unlock()

	r.intentsByCanonical[canonicalID] = &walletIntent{
		canonicalID: canonicalID,
		kind:        walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		address:     address,
	}
	r.intentsByAddress[address] = canonicalID
}

// observeIntentTxid associates a broadcast txid with a previously
// registered intent. Subsequent ledger rows keyed by that txid project
// onto the original canonical id.
func (r *Runtime) observeIntentTxid(canonicalID, txid string) {
	if txid == "" {
		return
	}
	r.intentsMu.Lock()
	defer r.intentsMu.Unlock()

	in, ok := r.intentsByCanonical[canonicalID]
	if !ok {
		return
	}
	in.txid = txid
	r.intentsByTxid[txid] = canonicalID
}

// resolveCanonicalID returns the canonical wallet-layer id for a row the
// history merger has just produced, projected onto the registered intent
// when one of the secondary keys (payment hash, txid, outpoint, address)
// matches. Returns the row's synthetic id unchanged when no intent claims
// the projection.
func (r *Runtime) resolveCanonicalID(syntheticID, paymentHash, txid string,
	outpoints []string, address string) string {

	r.intentsMu.RLock()
	defer r.intentsMu.RUnlock()

	if paymentHash != "" {
		if id, ok := r.intentsByPaymentHash[paymentHash]; ok {
			return id
		}
	}
	if txid != "" {
		if id, ok := r.intentsByTxid[txid]; ok {
			return id
		}
	}
	for _, op := range outpoints {
		if id, ok := r.intentsByOutpoint[op]; ok {
			return id
		}
	}
	if address != "" {
		if id, ok := r.intentsByAddress[address]; ok {
			return id
		}
	}

	return syntheticID
}

// trackPending records a new or refreshed pending entry so the deadline
// watcher can age it. Calling trackPending for an id already present
// updates the createdAt only when the existing entry is missing one,
// preserving the original submit time across monitor updates.
func (r *Runtime) trackPending(id string, kind walletrpc.EntryKind,
	createdAt time.Time) {

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	existing, ok := r.pending[id]
	if ok && !existing.createdAt.IsZero() {
		createdAt = existing.createdAt
	}

	r.pending[id] = pendingEntry{
		id:        id,
		kind:      kind,
		createdAt: createdAt,
		deadline:  createdAt.Add(r.deps.resolveDeadline()),
	}
	delete(r.overlay, id)
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
func (r *Runtime) applyDeadlines(now time.Time) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()

	for id, entry := range r.pending {
		if _, alreadyTimedOut := r.overlay[id]; alreadyTimedOut {
			continue
		}
		if now.Before(entry.deadline) {
			continue
		}

		r.overlay[id] = overlayStatus{
			status:        walletrpc.EntryStatus_ENTRY_STATUS_FAILED,
			failureReason: "timed_out",
		}
	}
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
