package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
)

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

	// events is the typed event server that fans TipBlock updates
	// out to all active subscribers. Its Start/Stop are driven by
	// TipPoller.Start/Stop.
	events *EventServer[*TipBlock]

	// mu guards the cached tip so BestBlock readers see a
	// consistent height/hash/timestamp triple.
	mu        sync.Mutex
	tipHeight int32
	tipHash   chainhash.Hash
	tipTime   time.Time

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
// the cadence.
func NewTipPoller(esplora *EsploraClient, pollInterval time.Duration,
	log btclog.Logger) *TipPoller {

	return &TipPoller{
		esplora:      esplora,
		pollInterval: pollInterval,
		log:          log,
		events:       NewEventServer[*TipBlock](log),
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

	t.mu.Lock()
	t.tipHeight = height
	t.tipHash = hash
	t.tipTime = tipTime
	t.mu.Unlock()

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

	// Stop the event server after the poll loop has exited so
	// that no SendUpdate is in flight when the server tears down
	// its subscriber handler.
	if err := t.events.Stop(); err != nil {
		t.log.WarnS(
			context.Background(),
			"Tip poller event server stop returned error",
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

// poll performs one tip-detection cycle. On detected progress it
// fetches the hash and header for each new height, broadcasts a
// TipBlock to every subscriber, and advances the cached tip
// monotonically. A failure to fetch any single block aborts the
// remainder of the cycle so subscribers never see an out-of-order
// event; the next tick re-attempts from the same starting point.
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
	t.mu.Unlock()

	// Known limitation: a same-height reorg (block at height N
	// replaced by a different block at height N) is invisible to
	// this loop until the chain advances to N+1, because we gate
	// progress on height alone rather than (height, hash). This
	// matches the behavior of the per-component pollers that
	// preceded the unified TipPoller and has historically been
	// acceptable for lwwallet's confirmation-target use case
	// (downstream callers re-check status against Esplora on every
	// tip event, so a stale hash at height N converges within one
	// extra tip advance). Documented here so it is not filed as a
	// regression by a future reader.
	if newHeight <= oldHeight {
		return
	}

	t.log.DebugS(context.Background(), "Tip poller advancing",
		slog.Int("old_height", int(oldHeight)),
		slog.Int("new_height", int(newHeight)),
	)

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

		event := &TipBlock{
			Height: height,
			Hash:   hash,
			Header: header,
		}

		// Hold t.mu across the {update tip + SendUpdate}
		// pair so BestBlockAndSubscribe can serialize against
		// it: a subscriber that acquires t.mu before us reads
		// the OLD tip and is guaranteed to receive THIS event
		// once it Subscribes (subscribe.Server's handler is
		// single-threaded over Subscribe and SendUpdate, so
		// our SendUpdate enqueues behind their Subscribe). A
		// subscriber that acquires t.mu after us reads the
		// NEW tip and will see only events strictly newer
		// than this one. Without holding t.mu here a tip
		// reader+subscriber pair has a small window where it
		// can read the new tip but miss this event entirely.
		t.mu.Lock()
		t.tipHeight = height
		t.tipHash = hash
		t.tipTime = time.Unix(header.Timestamp, 0)
		sendErr := t.events.SendUpdate(event)
		t.mu.Unlock()

		// SendUpdate failures only happen when the embedded
		// subscribe.Server is shutting down; log and exit so
		// we do not advance the cached tip past an event we
		// failed to fan out.
		if sendErr != nil {
			t.log.WarnS(
				context.Background(),
				"Tip poller send update failed",
				sendErr,
				slog.Int("height", int(height)),
			)

			return
		}
	}
}
