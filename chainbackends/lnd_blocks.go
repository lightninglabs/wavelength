package chainbackends

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

const (
	// lndBlockBufferSize bounds each subscriber's pending block history.
	lndBlockBufferSize = 10

	// lndBlockReconnectBackoff avoids spinning on a failed upstream stream.
	lndBlockReconnectBackoff = time.Second

	// lndBlockMaxReconnectBackoff caps recovery delay during an outage.
	lndBlockMaxReconnectBackoff = 30 * time.Second
)

// lndBlockSubscriber owns a bounded queue and a context cancellation hook.
type lndBlockSubscriber struct {
	epochs chan *chainsource.BlockEpoch
	stop   func() bool
}

// lndBlockRun owns one upstream stream across all RegisterBlocks callers.
// Subscriber and tip access is serialized by LNDBackend.blockMu. The run stays
// installed until upstream cancellation completes, preventing overlapping
// streams when the last cancellation races a new registration.
type lndBlockRun struct {
	//nolint:containedctx // the shared registration owns this context
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	subs   map[*lndBlockSubscriber]struct{}
	tip    *chainsource.BlockEpoch
}

// RegisterBlocks shares one upstream stream, replaying its latest observed tip
// to new subscribers. Live epochs retain upstream order, including replacement
// blocks at the same or lower heights. A full queue is closed rather than
// dropping individual events or blocking other subscribers; the existing
// chainsource actor reconnects a closed registration. Such an overflow can
// require the consumer to reconcile chain state, as can a fresh subscription.
func (b *LNDBackend) RegisterBlocks(ctx context.Context) (
	*chainsource.BlockRegistration, error) {

	for {
		b.blockMu.Lock()
		if err := ctx.Err(); err != nil {
			b.blockMu.Unlock()

			return nil, err
		}
		if b.blocksStopped {
			b.blockMu.Unlock()

			return nil, fmt.Errorf("block notifier stopped")
		}

		run := b.blockRun
		if run != nil && run.ctx.Err() != nil {
			b.blockMu.Unlock()
			select {
			case <-run.done:
				continue

			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if run == nil {
			// The shared stream belongs to the backend
			// registrations, never to the first subscriber's
			// request context. The final cancellation or Stop ends
			// this root context.
			runCtx, cancel := context.WithCancel(
				context.Background(),
			)
			event, err := b.notifier.RegisterBlockEpochNtfn(nil)
			if err != nil {
				cancel()
				b.blockMu.Unlock()

				return nil, fmt.Errorf("register blocks: %w",
					err)
			}
			run = &lndBlockRun{
				ctx: runCtx, cancel: cancel,
				done: make(chan struct{}),
				subs: make(map[*lndBlockSubscriber]struct{}),
			}
			b.blockRun = run
			go b.monitorBlocks(run, event)
		}

		sub := &lndBlockSubscriber{
			epochs: make(
				chan *chainsource.BlockEpoch,
				lndBlockBufferSize,
			),
		}
		if run.tip != nil {
			tip := *run.tip
			sub.epochs <- &tip
		}
		run.subs[sub] = struct{}{}
		sub.stop = context.AfterFunc(ctx, func() {
			b.cancelBlockSubscriber(run, sub)
		})
		b.blockMu.Unlock()

		return &chainsource.BlockRegistration{
			Epochs: sub.epochs,
			Cancel: func() {
				b.cancelBlockSubscriber(run, sub)
			},
		}, nil
	}
}

// cancelBlockSubscriber removes one queue atomically with fanout. The last
// cancellation also joins the shared reader and its upstream cleanup.
func (b *LNDBackend) cancelBlockSubscriber(run *lndBlockRun,
	sub *lndBlockSubscriber) {

	b.blockMu.Lock()
	if _, ok := run.subs[sub]; ok {
		delete(run.subs, sub)
		sub.stop()
		close(sub.epochs)
	}
	last := len(run.subs) == 0
	if last {
		run.cancel()
	}
	b.blockMu.Unlock()
	if last {
		<-run.done
	}
}

// monitorBlocks serializes upstream events and reconnects from the last full
// block identity. There is no height-only cache or deduplication, which would
// hide same-height reorg replacements. lnd supplies connected-block catch-up;
// disconnect notifications remain the confirmation/spend streams' concern.
func (b *LNDBackend) monitorBlocks(run *lndBlockRun,
	event *chainntnfs.BlockEpochEvent) {

	defer func() {
		run.cancel()
		if event != nil {
			event.Cancel()
		}
		b.blockMu.Lock()
		defer b.blockMu.Unlock()
		for sub := range run.subs {
			sub.stop()
			close(sub.epochs)
			delete(run.subs, sub)
		}
		b.blockRun = nil
		close(run.done)
	}()

	var cursor *chainntnfs.BlockEpoch
	backoff := lndBlockReconnectBackoff
	for run.ctx.Err() == nil {
		if event == nil {
			next, retry := b.reconnectBlocks(run, cursor, backoff)
			if !retry {
				return
			}
			event = next
			// Reset only after receiving a valid event: repeated
			// stream establishment followed by EOF must also back
			// off.
			backoff = min(backoff*2, lndBlockMaxReconnectBackoff)
			if event == nil {
				continue
			}
		}

		select {
		case epoch, ok := <-event.Epochs:
			if !ok || epoch == nil || epoch.Hash == nil {
				event.Cancel()
				event = nil

				continue
			}

			var timestamp int64
			if epoch.BlockHeader != nil {
				timestamp = epoch.BlockHeader.Timestamp.Unix()
			}
			block := chainsource.BlockEpoch{
				Hash: *epoch.Hash, Height: epoch.Height,
				Timestamp: timestamp,
			}
			b.fanoutBlock(run, block)
			cursor = &chainntnfs.BlockEpoch{
				Height: block.Height, Hash: &block.Hash,
			}
			backoff = lndBlockReconnectBackoff

		case <-run.ctx.Done():
			return
		}
	}
}

// reconnectBlocks waits without blocking shutdown, then attempts one shared
// registration. The chainntnfs API has no caller context; the remote adapter
// bounds stream establishment with lndRegistrationTimeout.
func (b *LNDBackend) reconnectBlocks(run *lndBlockRun,
	cursor *chainntnfs.BlockEpoch, backoff time.Duration) (
	*chainntnfs.BlockEpochEvent, bool) {

	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-run.ctx.Done():
		return nil, false
	}
	event, err := b.notifier.RegisterBlockEpochNtfn(cursor)
	if err != nil {
		b.logger(run.ctx).InfoS(
			run.ctx,
			"Block stream reconnect failed",
			slog.Any("err", err),
			slog.Duration("backoff", backoff),
		)
	}

	return event, true
}

// fanoutBlock gives each subscriber an independent value without allowing a
// stalled consumer to hold up the upstream reader or grow memory unboundedly.
func (b *LNDBackend) fanoutBlock(run *lndBlockRun,
	block chainsource.BlockEpoch) {

	b.blockMu.Lock()
	defer b.blockMu.Unlock()
	run.tip = &block
	for sub := range run.subs {
		copyBlock := block
		select {
		case sub.epochs <- &copyBlock:
		default:
			delete(run.subs, sub)
			sub.stop()
			close(sub.epochs)
			b.logger(run.ctx).InfoS(run.ctx,
				"Closing slow block subscriber",
			)
		}
	}
	if len(run.subs) == 0 {
		run.cancel()
	}
}

// stopBlocks prevents new subscriptions and joins the active shared reader.
func (b *LNDBackend) stopBlocks() {
	b.blockMu.Lock()
	b.blocksStopped = true
	run := b.blockRun
	if run != nil {
		run.cancel()
	}
	b.blockMu.Unlock()
	if run != nil {
		<-run.done
	}
}
