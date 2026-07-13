package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
)

// DefaultHashHistorySize is the default upper bound on entries retained
// in the TipPoller's bounded height -> hash history map. It is sized to
// at least twice the conventional Bitcoin reorg-safety depth (6) so a
// reorg at finality depth still has its disconnected hashes available
// for walk-back, with headroom. Configurable via NewTipPollerWithConfig.
const DefaultHashHistorySize = 100

// TipBlock describes a newly detected block emitted by TipPoller.
// Each subscriber receives one TipBlock per advance: when the tip
// moves from oldHeight to newHeight the poller fans out (newHeight -
// oldHeight) events in monotonically increasing height order so
// downstream consumers can apply per-block work without skipping or
// re-ordering.
type TipBlock struct {
	// Height is the block's height in the canonical chain.
	Height int32

	// Hash is the block's identifying hash. Together with Height
	// this uniquely identifies a block for downstream chain-watch
	// handlers.
	Hash chainhash.Hash

	// Header carries the JSON header response (height + timestamp);
	// it is included so subscribers do not need to re-fetch it from
	// Esplora. Will be non-nil for events emitted from the poll
	// loop.
	Header *esploraBlock
}

// TipSubscription is the typed handle returned by TipPoller.Subscribe.
// It is a thin alias over Subscription[*TipBlock] that exists purely
// for ergonomic call-site naming.
type TipSubscription = Subscription[*TipBlock]

// ReorgEvent describes a chain reorganization observed by the
// TipPoller. The poller emits one ReorgEvent every time it detects
// that one or more previously broadcast blocks are no longer in the
// canonical chain. Disconnected hashes are listed in canonical
// (low-height first) order; Connected blocks are the new tip's path
// from ForkHeight+1 onward, also in canonical order. Either slice may
// be empty: a same-height hash-replacement reorg has Disconnected of
// length 1 and Connected of length 1.
type ReorgEvent struct {
	// ForkHeight is the highest height at which the old chain and the
	// new chain agree on the block hash. The first disconnected /
	// connected block is at ForkHeight+1.
	ForkHeight int32

	// Disconnected lists block hashes that were previously broadcast
	// as part of the canonical chain and are no longer on it, in
	// ascending height order (ForkHeight+1 first).
	Disconnected []chainhash.Hash

	// Connected lists blocks now on the canonical chain starting at
	// ForkHeight+1, in ascending height order. The poller will also
	// fan these out individually as TipBlock events on the standard
	// Subscribe channel after the ReorgEvent is delivered.
	Connected []*TipBlock
}

// ReorgSubscription is the typed handle returned by
// TipPoller.SubscribeReorgs.
type ReorgSubscription = Subscription[*ReorgEvent]

// ChainEvent is the unified update delivered on the ordered chain
// stream returned by SubscribeChain. Exactly one of Reorg and Tip is
// non-nil per event:
//
//   - Reorg-only events announce a reorg's disconnected range and
//     precede the replacement tip events on the same stream.
//   - Tip-only events announce a new block (either a forward advance
//     or a post-reorg connected block).
//
// Cross-event ordering on this stream is producer-ordered: the
// embedded EventServer delivers updates in SendUpdate order to a
// single subscriber goroutine, so a downstream consumer that
// dispatches on event type from one channel observes the same order
// the TipPoller emitted. That is the load-bearing property
// downstream needs to emit BlockDisconnected before BlockConnected
// for the replacement chain (btcwallet's disconnectBlock requires
// it; chainsource finality synthesis requires it too).
type ChainEvent struct {
	Reorg *ReorgEvent
	Tip   *TipBlock
}

// ChainSubscription is the typed handle returned by
// TipPoller.SubscribeChain. It carries the unified ChainEvent stream.
type ChainSubscription = Subscription[*ChainEvent]

// TipPoller is the single source of truth for the lwwallet chain
// tip. Exactly one polling goroutine periodically asks the Esplora
// backend for the current best height. When new blocks are detected
// the poller walks oldHeight+1 → newHeight, resolves each block's
// hash + header (which the EsploraClient's caches will absorb on
// repeat callers) and broadcasts a TipBlock event to every active
// subscriber via the embedded EventServer.
//
// Centralizing the tip stream lets multiple downstream chain
// watchers (the btcwallet chain.Interface adapter and the
// chainsource.ChainBackend) share a single Esplora call cadence
// instead of polling independently. Subscriber lifecycle is
// delegated to lnd's subscribe.Server (wrapped by EventServer) so
// the poller itself never has to reason about close-channel races
// or slow-subscriber back-pressure.
type TipPoller struct {
	esplora      *EsploraClient
	pollInterval time.Duration
	log          btclog.Logger

	// historySize caps the bounded height -> hash history map that
	// lets the poller resolve old-chain hashes during reorg walk-back.
	historySize int

	// events is the typed event server that fans TipBlock updates
	// out to all active subscribers. Its Start/Stop are driven by
	// TipPoller.Start/Stop.
	events *EventServer[*TipBlock]

	// reorgs is the typed event server that fans ReorgEvent updates
	// out to subscribers that opt in via SubscribeReorgs. Reorgs are
	// rare enough that a separate server is cheaper than re-fitting
	// every TipBlock consumer with reorg-aware logic.
	reorgs *EventServer[*ReorgEvent]

	// chain is the unified event server that fans both tip and reorg
	// updates out on a single ordered stream. Consumers that need
	// strict reorg-before-replacement-tip ordering (chain.Interface
	// adapter, chainsource backend) subscribe here rather than to
	// the separate events / reorgs servers, which would race across
	// two independent translator goroutines.
	chain *EventServer[*ChainEvent]

	// mu guards the cached tip so BestBlock readers see a
	// consistent height/hash/timestamp triple. It also guards
	// recentHashes, which is sized small enough that linear scans
	// under the lock are cheap.
	mu        sync.Mutex
	tipHeight int32
	tipHash   chainhash.Hash
	tipTime   time.Time

	// recentHashes maps a recent canonical-chain height to the hash
	// the poller broadcast for it. It is bounded to historySize
	// entries: on every insert we prune any entry whose height is
	// more than historySize below the current tip. The buffer lets
	// reorg detection walk back to the fork point using the cached
	// hashes rather than re-fetching the entire pre-fork chain.
	recentHashes map[int32]chainhash.Hash

	// started gates re-entrant Start calls; the underlying
	// EventServer is also idempotent on Start.
	started bool

	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewTipPoller constructs a TipPoller bound to the given Esplora
// client. The poll interval controls how often the goroutine asks
// Esplora for the latest tip height; subscribers do not influence
// the cadence. The bounded hash-history map is sized to
// DefaultHashHistorySize.
func NewTipPoller(esplora *EsploraClient, pollInterval time.Duration,
	log btclog.Logger) *TipPoller {

	return NewTipPollerWithConfig(
		esplora, pollInterval, DefaultHashHistorySize, log,
	)
}

// NewTipPollerWithConfig is the explicit-history-size constructor.
// historySize <= 0 falls back to DefaultHashHistorySize so callers
// cannot accidentally disable reorg walk-back by passing a zero value.
func NewTipPollerWithConfig(esplora *EsploraClient, pollInterval time.Duration,
	historySize int, log btclog.Logger) *TipPoller {

	if historySize <= 0 {
		historySize = DefaultHashHistorySize
	}

	return &TipPoller{
		esplora:      esplora,
		pollInterval: pollInterval,
		historySize:  historySize,
		log:          log,
		events:       NewEventServer[*TipBlock](log),
		reorgs:       NewEventServer[*ReorgEvent](log),
		chain:        NewEventServer[*ChainEvent](log),
		recentHashes: make(map[int32]chainhash.Hash),
		quit:         make(chan struct{}),
	}
}

// Start fetches the initial tip synchronously, starts the embedded
// event server, and then spawns the polling goroutine. Returning
// early without starting the goroutine when the initial fetch fails
// preserves the existing chain-backend contract: a misconfigured
// Esplora endpoint must surface at startup, not silently translate
// into a "stuck at height 0" runtime symptom.
func (t *TipPoller) Start() error {
	// Claim the started slot atomically before doing any network
	// I/O. Setting the flag in the same critical section as the
	// check prevents two concurrent Start() calls from both passing
	// the guard, both fetching the initial tip, and both spawning
	// poll goroutines that would race on tipHeight/tipHash and
	// double the SendUpdate cadence to subscribers.
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()

		return fmt.Errorf("tip poller already started")
	}
	t.started = true
	t.mu.Unlock()

	// resetStarted releases the started slot on any failure path
	// below so a caller that retries Start on the same instance
	// after a transient Esplora failure is not permanently locked
	// out by the atomic claim above.
	resetStarted := func() {
		t.mu.Lock()
		t.started = false
		t.mu.Unlock()
	}

	height, err := t.esplora.GetTipHeight(context.Background())
	if err != nil {
		resetStarted()

		return fmt.Errorf("get initial tip height: %w", err)
	}

	hash, err := t.esplora.GetBlockHashByHeight(
		context.Background(), height,
	)
	if err != nil {
		resetStarted()

		return fmt.Errorf("get initial tip hash: %w", err)
	}

	// Fetching the initial header is best-effort: a missing
	// timestamp at start-up is not load-bearing — the next
	// confirmed block updates it from the live header anyway —
	// and treating it as fatal would force every test mock to
	// serve `/block/<hash>` for the initial seed even when the
	// tested path never reads `tipTime`.
	var tipTime time.Time
	header, hdrErr := t.esplora.GetBlockHeader(
		context.Background(), hash,
	)
	if hdrErr == nil {
		tipTime = time.Unix(header.Timestamp, 0)
	} else {
		t.log.WarnS(
			context.Background(),
			"Tip poller initial header fetch failed",
			hdrErr,
			slog.String("hash", hash.String()),
		)
	}

	if err := t.events.Start(); err != nil {
		resetStarted()

		return fmt.Errorf("start event server: %w", err)
	}

	if err := t.reorgs.Start(); err != nil {
		// Roll back the tip-event server so a partial Start does
		// not leave a half-running poller behind.
		_ = t.events.Stop()
		resetStarted()

		return fmt.Errorf("start reorg event server: %w", err)
	}

	if err := t.chain.Start(); err != nil {
		_ = t.events.Stop()
		_ = t.reorgs.Stop()
		resetStarted()

		return fmt.Errorf("start chain event server: %w", err)
	}

	// Seed recentHashes by walking back historySize-1 heights so a
	// reorg whose disconnected range extends below the seeded tip
	// but within the configured history can still resolve every
	// disconnected hash from the cache. Without this, a fresh
	// poller would only ever cache the initial tip, leaving a
	// downstream chain.Interface consumer unable to enumerate every
	// hash btcwallet must roll back on a multi-block reorg below
	// the seeded tip. The walk-back is best-effort: a single
	// per-height fetch failure ends the seed loop early; the next
	// poll tick still drives the cache forward as the chain grows.
	t.mu.Lock()
	t.tipHeight = height
	t.tipHash = hash
	t.tipTime = tipTime
	t.recordHashLocked(height, hash)
	t.mu.Unlock()

	for h := height - 1; h > height-int32(t.historySize) && h >= 0; h-- {
		liveHash, err := t.esplora.GetBlockHashByHeight(
			context.Background(), h,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller history seed fetch failed",
				err,
				slog.Int("height", int(h)),
			)

			break
		}

		t.mu.Lock()
		t.recordHashLocked(h, liveHash)
		t.mu.Unlock()
	}

	t.wg.Add(1)
	go t.pollLoop()

	t.log.InfoS(context.Background(), "Tip poller started",
		slog.Int("tip_height", int(height)),
		slog.String("tip_hash", hash.String()),
	)

	return nil
}

// Stop signals the polling goroutine to exit, waits for it to
// drain, and tears down the event server. Stop is idempotent; the
// second call returns immediately after the first has finished.
func (t *TipPoller) Stop() {
	t.stopOnce.Do(func() {
		close(t.quit)
	})

	t.wg.Wait()

	// Stop the event servers after the poll loop has exited so
	// that no SendUpdate is in flight when the server tears down
	// its subscriber handler.
	if err := t.events.Stop(); err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller event server stop returned error",
			err,
		)
	}

	if err := t.reorgs.Stop(); err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller reorg event server stop returned error",
			err,
		)
	}

	if err := t.chain.Stop(); err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller chain event server stop returned error",
			err,
		)
	}
}

// BestBlock returns a snapshot of the currently cached tip. Callers
// that just need to read the chain tip without subscribing to new
// block events should use this instead of issuing a fresh Esplora
// request.
func (t *TipPoller) BestBlock() (int32, chainhash.Hash, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.tipHeight, t.tipHash, t.tipTime
}

// Subscribe returns a typed subscription that receives a TipBlock
// for every new block the poller observes. The caller must invoke
// Cancel on the returned subscription when finished. The first
// event a subscriber receives corresponds to the first block
// detected after Subscribe returns; callers that need the current
// tip atomically with the subscription should use
// BestBlockAndSubscribe instead — Subscribe alone leaves a small
// window where a tip event could land between a separate
// BestBlock() read and the subscription registering.
func (t *TipPoller) Subscribe() (*TipSubscription, error) {
	return t.events.Subscribe()
}

// SubscribeReorgs returns a typed subscription that receives a
// ReorgEvent every time the poller detects that one or more
// previously broadcast blocks have been replaced on the canonical
// chain. Callers that need both tip events and reorg events should
// subscribe to both streams; reorg events are delivered BEFORE the
// connected blocks are fanned out on the TipBlock stream so that
// consumers can mark their registrations dirty before the
// re-confirmation arrives.
func (t *TipPoller) SubscribeReorgs() (*ReorgSubscription, error) {
	return t.reorgs.Subscribe()
}

// BestBlockAndSubscribe atomically reads the current cached tip and
// registers a new subscription. The poll goroutine holds t.mu
// during the {update tip + SendUpdate} pair, and this function
// holds t.mu around {Subscribe + read tip}, so the two operations
// serialize: callers either see the old tip and receive the next
// tip event, or see the new tip and skip ahead to events strictly
// after it. The non-atomic Subscribe + BestBlock pair leaves a
// race where a tip event could land between the read and the
// register, causing a missed event or a duplicated one.
func (t *TipPoller) BestBlockAndSubscribe() (int32, chainhash.Hash, time.Time,
	*TipSubscription, error) {

	t.mu.Lock()
	defer t.mu.Unlock()

	sub, err := t.events.Subscribe()
	if err != nil {
		return 0, chainhash.Hash{}, time.Time{}, nil,
			fmt.Errorf("subscribe to tip poller: %w", err)
	}

	return t.tipHeight, t.tipHash, t.tipTime, sub, nil
}

// BestBlockAndSubscribeAll atomically reads the cached tip and
// registers both a TipBlock subscription and a ReorgEvent
// subscription. Reorg-aware consumers (e.g. ChainBackend) use this
// to avoid the small window where a reorg could land between
// independently registering on the two streams.
func (t *TipPoller) BestBlockAndSubscribeAll() (int32, chainhash.Hash,
	time.Time, *TipSubscription, *ReorgSubscription, error) {

	t.mu.Lock()
	defer t.mu.Unlock()

	sub, err := t.events.Subscribe()
	if err != nil {
		return 0, chainhash.Hash{}, time.Time{}, nil, nil,
			fmt.Errorf("subscribe to tip events: %w", err)
	}

	reorgSub, err := t.reorgs.Subscribe()
	if err != nil {
		sub.Cancel()

		return 0, chainhash.Hash{}, time.Time{}, nil, nil,
			fmt.Errorf("subscribe to reorg events: %w", err)
	}

	return t.tipHeight, t.tipHash, t.tipTime, sub, reorgSub, nil
}

// SubscribeChain returns a typed subscription on the unified chain
// event stream. Updates arrive in producer order through a single
// channel; consumers that need strict reorg-before-replacement-tip
// ordering must subscribe here rather than to the separate tip /
// reorg streams (which race across two independent translator
// goroutines).
func (t *TipPoller) SubscribeChain() (*ChainSubscription, error) {
	return t.chain.Subscribe()
}

// BestBlockAndSubscribeChain atomically reads the cached tip and
// registers a ChainSubscription. Same atomicity guarantee as
// BestBlockAndSubscribe, applied to the unified stream.
func (t *TipPoller) BestBlockAndSubscribeChain() (int32, chainhash.Hash,
	time.Time, *ChainSubscription, error) {

	t.mu.Lock()
	defer t.mu.Unlock()

	sub, err := t.chain.Subscribe()
	if err != nil {
		return 0, chainhash.Hash{}, time.Time{}, nil,
			fmt.Errorf("subscribe to chain events: %w", err)
	}

	return t.tipHeight, t.tipHash, t.tipTime, sub, nil
}

// recordHashLocked inserts a (height, hash) pair into the recent-
// hash history map and prunes any entry whose height has fallen out
// of the historySize window relative to the new entry. Caller must
// hold t.mu.
func (t *TipPoller) recordHashLocked(height int32, hash chainhash.Hash) {
	t.recentHashes[height] = hash

	cutoff := height - int32(t.historySize)
	for h := range t.recentHashes {
		if h <= cutoff {
			delete(t.recentHashes, h)
		}
	}
}

// hashAtHeightLocked returns the cached hash for a height plus
// whether it was present. Caller must hold t.mu.
func (t *TipPoller) hashAtHeightLocked(height int32) (chainhash.Hash, bool) {
	h, ok := t.recentHashes[height]

	return h, ok
}

// pollLoop is the single tip-polling goroutine. It ticks at
// pollInterval, asks Esplora for the latest tip, and walks the
// gap from the cached tip to the new tip emitting one TipBlock per
// step.
func (t *TipPoller) pollLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.poll()

		case <-t.quit:
			return
		}
	}
}

// poll performs one tip-detection cycle. The cycle is reorg-aware:
//
//  1. Query (tip height, tip hash).
//  2. If the tip is at the same height as the cached tip but the hash
//     differs, walk back through cached hashes to find the fork point
//     and emit a ReorgEvent + a re-broadcast TipBlock at the same
//     height. This is the "same-height reorg" case that earlier
//     versions of the poller could not detect.
//  3. If the new height is strictly greater than the cached height,
//     fetch each intervening block and verify that its PrevBlock
//     points at the last hash broadcast. A mismatch at any step walks
//     back to the fork point, emits a ReorgEvent for the disconnected
//     range, and then fans TipBlock events for the new chain in
//     ascending order. If the new chain extends cleanly (the common
//     case), no ReorgEvent fires and TipBlock events are dispatched as
//     before.
//
// A failure to fetch any single block aborts the remainder of the
// cycle so subscribers never see an out-of-order event; the next
// tick re-attempts from the same starting point.
func (t *TipPoller) poll() {
	newHeight, err := t.esplora.GetTipHeight(context.Background())
	if err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller GetTipHeight failed",
			err,
		)

		return
	}

	t.mu.Lock()
	oldHeight := t.tipHeight
	oldHash := t.tipHash
	t.mu.Unlock()

	switch {
	case newHeight < oldHeight:
		// The remote reports fewer blocks than we have cached.
		// Two distinct cases hide behind a lower height:
		//
		//   1. A transient indexer hiccup where the remote
		//      momentarily lags our cached tip but is still on
		//      the same chain (our tip remains canonical).
		//   2. A genuine reorg onto a SHORTER but higher-work
		//      chain: blocks above newHeight were orphaned and
		//      the surviving chain has fewer total blocks. Bitcoin
		//      follows most-work, not most-blocks, so a shorter
		//      chain can legitimately win.
		//
		// Disambiguate by comparing the live hash at newHeight to
		// our cached hash for that height. Agreement means the
		// remote is merely behind (case 1, no-op); divergence
		// means the chain reorged out from under us (case 2) and
		// we must walk back to the fork point. Without this an
		// orphaning reorg to a shorter chain would be silently
		// ignored until the new chain grew past our stale tip.
		newTipHash, err := t.esplora.GetBlockHashByHeight(
			context.Background(), newHeight,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller shorter-chain hash check failed",
				err,
				slog.Int("height", int(newHeight)),
			)

			return
		}

		t.mu.Lock()
		cachedHash, haveCached := t.hashAtHeightLocked(newHeight)
		t.mu.Unlock()

		switch {
		// newHeight predates our retained history floor, so we
		// cannot prove a reorg by hash comparison. A reorg this
		// deep exceeds historySize and is an operator-level
		// problem rather than a productionally recoverable one
		// (mirrors handleReorg's own deep-reorg guard); log and
		// re-check next tick rather than walk a pruned history.
		case !haveCached:
			t.log.WarnS(
				context.Background(),
				"Tip poller remote tip below retained "+
					"history floor; ignoring",
				fmt.Errorf("history floor exceeded"),
				slog.Int("new_height", int(newHeight)),
				slog.Int("old_height", int(oldHeight)),
			)

			return

		// Same hash at newHeight: the remote is merely lagging
		// our cached tip on the same chain. No-op.
		case newTipHash == cachedHash:
			return
		}

		// Divergent hash at newHeight: a reorg onto a shorter,
		// higher-work chain. handleReorg builds Disconnected up
		// to our old cached tip (covering the orphaned blocks
		// above newHeight) and Connected up to newHeight.
		t.handleReorg(newHeight, newTipHash)

		return

	case newHeight == oldHeight:
		// Same-height: query the hash at this height (which
		// the BlockHashByHeight endpoint resolves directly).
		// If it differs from the cached hash at the same
		// height we have a same-height reorg; if it matches
		// the chain has not moved.
		newTipHash, err := t.esplora.GetBlockHashByHeight(
			context.Background(), newHeight,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller same-height hash check failed",
				err,
				slog.Int("height", int(newHeight)),
			)

			return
		}

		if newTipHash == oldHash {
			return
		}

		t.handleReorg(newHeight, newTipHash)

		return

	default:
		// newHeight > oldHeight: forward advance, possibly with
		// a deeper reorg if the first new height's predecessor
		// is not the cached tip hash.
		t.advance(oldHeight, newHeight)
	}
}

// advance walks the chain forward from oldHeight+1 to newHeight,
// detecting reorgs along the way. Every new height's raw header is
// checked against the previously broadcast hash. A mismatch triggers
// a walk-back to the fork point before the replacement block is
// broadcast, including when a reorg lands midway through a multi-block
// catch-up walk.
func (t *TipPoller) advance(oldHeight, newHeight int32) {
	t.log.DebugS(context.Background(), "Tip poller advancing",
		slog.Int("old_height", int(oldHeight)),
		slog.Int("new_height", int(newHeight)),
	)

	t.mu.Lock()
	previousHash, ok := t.hashAtHeightLocked(oldHeight)
	t.mu.Unlock()
	if !ok {
		t.log.WarnS(
			context.Background(),
			"Tip poller advance starts below retained history",
			fmt.Errorf("history floor exceeded"),
			slog.Int("old_height", int(oldHeight)),
		)

		return
	}

	for height := oldHeight + 1; height <= newHeight; height++ {
		hash, err := t.esplora.GetBlockHashByHeight(
			context.Background(), height,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller GetBlockHashByHeight failed",
				err,
				slog.Int("height", int(height)),
			)

			return
		}

		header, err := t.esplora.GetBlockHeader(
			context.Background(), hash,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller GetBlockHeader failed",
				err,
				slog.String("hash", hash.String()),
			)

			return
		}

		// Verify continuity at every step. Checking only the first
		// height would miss a reorg that lands after an earlier block
		// in this catch-up walk has already been broadcast.
		rawHdr, err := t.esplora.GetRawBlockHeader(
			context.Background(), hash,
		)
		if err != nil {
			// Without the raw header we cannot prove that this
			// block connects to the last one subscribers saw.
			// Abort without advancing; the next tick retries.
			t.log.WarnS(
				context.Background(),
				"Tip poller raw header fetch failed; aborting "+
					"cycle",
				err,
				slog.String("hash", hash.String()),
			)

			return
		}

		if rawHdr.PrevBlock != previousHash {
			// The live chain changed during this walk. Resolve the
			// actual current tip and let handleReorg disconnect any
			// earlier blocks this iteration already broadcast.
			tipHash, tipErr := t.esplora.GetBlockHashByHeight(
				context.Background(), newHeight,
			)
			if tipErr != nil {
				t.log.WarnS(
					context.Background(),
					"Tip poller reorg tip fetch failed",
					tipErr,
					slog.Int("height", int(newHeight)),
				)

				return
			}

			t.handleReorg(newHeight, tipHash)

			return
		}

		event := &TipBlock{
			Height: height,
			Hash:   hash,
			Header: header,
		}

		if !t.broadcastTipBlock(event) {
			return
		}

		previousHash = hash
	}
}

// broadcastTipBlock updates the cached tip, records the hash in the
// history map, and fans the TipBlock out to subscribers. Returns
// false if the send failed (server shutting down), telling the
// caller to abort the cycle so the cached tip is not advanced past
// an event subscribers did not receive.
func (t *TipPoller) broadcastTipBlock(event *TipBlock) bool {
	// Hold t.mu across the {update tip + SendUpdate} pair so
	// BestBlockAndSubscribe can serialize against it: a subscriber
	// that acquires t.mu before us reads the OLD tip and is
	// guaranteed to receive THIS event once it Subscribes
	// (subscribe.Server's handler is single-threaded over
	// Subscribe and SendUpdate, so our SendUpdate enqueues behind
	// their Subscribe). A subscriber that acquires t.mu after us
	// reads the NEW tip and will see only events strictly newer
	// than this one.
	t.mu.Lock()
	t.tipHeight = event.Height
	t.tipHash = event.Hash
	if event.Header != nil {
		t.tipTime = time.Unix(event.Header.Timestamp, 0)
	}
	t.recordHashLocked(event.Height, event.Hash)
	tipErr := t.events.SendUpdate(event)
	chainErr := t.chain.SendUpdate(&ChainEvent{Tip: event})
	t.mu.Unlock()

	if tipErr != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller send update failed",
			tipErr,
			slog.Int("height", int(event.Height)),
		)

		return false
	}

	if chainErr != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller chain stream send failed",
			chainErr,
			slog.Int("height", int(event.Height)),
		)

		return false
	}

	return true
}

// handleReorg walks back through the recent-hash history map until it
// finds the fork point between the cached chain and the new chain
// whose tip is (newTipHeight, newTipHash). It then constructs a
// ReorgEvent listing the disconnected hashes and connected blocks,
// broadcasts the reorg, and finally fans out the connected blocks
// as TipBlock events in ascending height order so consumers can
// re-check registrations against each new block.
func (t *TipPoller) handleReorg(newTipHeight int32, newTipHash chainhash.Hash) {
	t.log.InfoS(context.Background(), "Tip poller detected reorg",
		slog.Int("new_tip_height", int(newTipHeight)),
		slog.String("new_tip_hash", newTipHash.String()),
	)

	// Walk back to find the fork point. We probe heights from
	// newTipHeight downwards, comparing the live Esplora hash at
	// each height to the cached hash. The first height at which
	// they agree is the fork point. We bound the search by the
	// retained history depth so a misbehaving Esplora cannot drag
	// us into an unbounded loop.
	t.mu.Lock()
	cachedTipHeight := t.tipHeight
	cutoff := cachedTipHeight - int32(t.historySize)
	t.mu.Unlock()

	if cutoff < 0 {
		cutoff = 0
	}

	// connectedByHeight collects new-chain blocks discovered
	// during walk-back so we can re-broadcast them in ascending
	// order after the fork point is found.
	connectedByHeight := make(map[int32]chainhash.Hash)
	connectedByHeight[newTipHeight] = newTipHash

	forkHeight := int32(-1)
	probeHeight := newTipHeight
	for probeHeight > cutoff {
		probeHeight--

		// Heights strictly above our cached tip cannot be a fork
		// point: we never broadcast a block there, so the live
		// block is a NEW connected block on a longer chain (a
		// forward reorg that both reorganized old blocks AND
		// extended past our tip). Record it and keep walking
		// down toward the real fork. Without this, the
		// !haveCached branch below would mistake the first such
		// height for the fork point and compute a forkHeight
		// above the cached tip, underflowing the Disconnected
		// slice capacity.
		if probeHeight > cachedTipHeight {
			liveHash, err := t.esplora.GetBlockHashByHeight(
				context.Background(), probeHeight,
			)
			if err != nil {
				t.log.WarnS(
					context.Background(),
					"Tip poller reorg walk-back failed",
					err,
					slog.Int("height", int(probeHeight)),
				)

				return
			}

			connectedByHeight[probeHeight] = liveHash

			continue
		}

		t.mu.Lock()
		cachedHash, haveCached := t.hashAtHeightLocked(probeHeight)
		t.mu.Unlock()

		if !haveCached {
			// We never broadcast a block at this height
			// (typically because it's older than our
			// retained history's start, e.g. on a fresh
			// poller that only ever cached the initial
			// tip). Treat probeHeight as the fork point:
			// anything at or below it is not part of our
			// old broadcast set, so it cannot be
			// "disconnected" from a downstream consumer's
			// point of view. The live hash at this height
			// is not a new connected block we owe
			// subscribers either; only blocks strictly
			// above probeHeight that we previously
			// broadcast (and that have now changed) form
			// the reorg boundary.
			forkHeight = probeHeight

			break
		}

		liveHash, err := t.esplora.GetBlockHashByHeight(
			context.Background(), probeHeight,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller reorg walk-back failed",
				err,
				slog.Int("height", int(probeHeight)),
			)

			return
		}

		if liveHash == cachedHash {
			// Genuine fork point: the new chain agrees with
			// our cached canonical chain at this height.
			forkHeight = probeHeight

			break
		}

		// haveCached && liveHash != cachedHash: this height is
		// part of the reorg. Record the live hash so we can
		// rebroadcast it after the fork point is found.
		connectedByHeight[probeHeight] = liveHash
	}

	if forkHeight < 0 {
		// We exhausted the retained history without finding a
		// fork point. Be conservative: log and bail. A future
		// tick will re-attempt with the now-pruned history; in
		// practice a reorg deeper than DefaultHashHistorySize
		// is a developer / operator problem, not a
		// productionally recoverable condition.
		t.log.WarnS(
			context.Background(),
			"Tip poller reorg deeper than retained history; "+
				"giving up walk-back",
			fmt.Errorf("history exhausted"),
			slog.Int("new_tip_height", int(newTipHeight)),
			slog.Int("history_size", t.historySize),
		)

		return
	}

	// Build the Disconnected slice from cached hashes between
	// forkHeight+1 and the cached tipHeight in one critical
	// section. The walk is already in ascending order so no sort
	// is needed; we cap at the history size to bound the worst
	// case under t.mu.
	t.mu.Lock()
	cachedTipHeight = t.tipHeight
	disconnected := make(
		[]chainhash.Hash, 0, int(cachedTipHeight-forkHeight),
	)
	for h := forkHeight + 1; h <= cachedTipHeight; h++ {
		if hash, ok := t.hashAtHeightLocked(h); ok {
			disconnected = append(disconnected, hash)
		}
	}
	t.mu.Unlock()

	// Build the Connected slice. Heights range from forkHeight+1
	// to newTipHeight; for each, fetch the header so the
	// TipBlock carries a populated Header field for downstream
	// consumers.
	connectedHeights := make([]int32, 0, len(connectedByHeight))
	for h := range connectedByHeight {
		if h <= forkHeight {
			continue
		}
		connectedHeights = append(connectedHeights, h)
	}
	sort.Slice(connectedHeights, func(i, j int) bool {
		return connectedHeights[i] < connectedHeights[j]
	})

	connected := make([]*TipBlock, 0, len(connectedHeights))
	for _, h := range connectedHeights {
		hash := connectedByHeight[h]
		header, err := t.esplora.GetBlockHeader(
			context.Background(), hash,
		)
		if err != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller reorg header fetch failed",
				err,
				slog.String("hash", hash.String()),
			)

			return
		}

		connected = append(connected, &TipBlock{
			Height: h,
			Hash:   hash,
			Header: header,
		})
	}

	// Prune the now-stale hashes from the history before we
	// broadcast the reorg so a subscriber that immediately calls
	// back into BestBlock sees the new tip.
	t.mu.Lock()
	for h := forkHeight + 1; h <= cachedTipHeight; h++ {
		delete(t.recentHashes, h)
	}
	t.mu.Unlock()

	reorgEvent := &ReorgEvent{
		ForkHeight:   forkHeight,
		Disconnected: disconnected,
		Connected:    connected,
	}

	t.log.InfoS(context.Background(), "Tip poller emitting reorg",
		slog.Int("fork_height", int(forkHeight)),
		slog.Int("disconnected", len(disconnected)),
		slog.Int("connected", len(connected)),
	)

	if err := t.reorgs.SendUpdate(reorgEvent); err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller reorg send failed",
			err,
		)

		return
	}

	// Also emit the reorg on the unified chain stream BEFORE any
	// connected-block tip events land on it, so a single consumer
	// reading the chain subscription sees Reorged before any of
	// the replacement Connected blocks. This is the load-bearing
	// ordering property the chain.Interface adapter needs to emit
	// BlockDisconnected before BlockConnected, and the chainsource
	// backend needs to reset registrations before block-epoch
	// driven re-checks run on the replacement chain.
	if err := t.chain.SendUpdate(
		&ChainEvent{Reorg: reorgEvent},
	); err != nil {

		t.log.WarnS(
			context.Background(),
			"Tip poller chain reorg send failed",
			err,
		)

		return
	}

	// Finally, fan the connected blocks out on the standard
	// TipBlock stream so existing consumers re-check on each
	// new block exactly as they would on a non-reorg advance.
	// broadcastTipBlock also pushes each block onto the unified
	// chain stream so the single-channel consumer observes the
	// connected blocks immediately after the reorg event in the
	// same producer-ordered sequence.
	for _, block := range connected {
		if !t.broadcastTipBlock(block) {
			return
		}
	}
}
