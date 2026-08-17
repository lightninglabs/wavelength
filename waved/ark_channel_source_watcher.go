package waved

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/fraud"
	"github.com/lightninglabs/wavelength/vtxo"
)

// arkChannelSourceWatcher passively watches every on-path source ancestor and
// turns the first confirmed spend into a durable channel event.
type arkChannelSourceWatcher struct {
	backend chainsource.ChainBackend
	log     btclog.Logger
	ctx     context.Context //nolint:containedctx // owned until Stop
	cancel  context.CancelFunc

	mu            sync.Mutex
	sink          arkchannel.ChannelEventSink
	registrations map[arkchannel.ID][]*chainsource.SpendRegistration
	retryDelay    time.Duration
	wg            sync.WaitGroup
}

// newArkChannelSourceWatcher constructs an endpoint-local passive watcher.
func newArkChannelSourceWatcher(backend chainsource.ChainBackend,
	log btclog.Logger) *arkChannelSourceWatcher {

	ctx, cancel := context.WithCancel(context.Background())
	if log == nil {
		log = btclog.Disabled
	}

	return &arkChannelSourceWatcher{
		backend: backend, log: log, ctx: ctx, cancel: cancel,
		registrations: make(
			map[arkchannel.ID][]*chainsource.SpendRegistration,
		),
		retryDelay: time.Second,
	}
}

// BindChannelEventSink attaches the durable channel service.
func (w *arkChannelSourceWatcher) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	if sink == nil {
		return fmt.Errorf("channel event sink is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sink == nil {
		w.sink = sink
	}

	return nil
}

// Track atomically arms all watches for one channel. A duplicate install is
// idempotent and reuses the already active registrations.
func (w *arkChannelSourceWatcher) Track(ctx context.Context, id arkchannel.ID,
	desc *vtxo.Descriptor) error {

	plan, err := fraud.BuildWatchPlan(desc)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.sink == nil {
		w.mu.Unlock()

		return fmt.Errorf("channel event sink is not bound")
	}
	if _, ok := w.registrations[id]; ok {
		w.mu.Unlock()

		return nil
	}
	w.mu.Unlock()

	registrations := make(
		[]*chainsource.SpendRegistration, 0, len(plan.Watches),
	)
	for i := range plan.Watches {
		watch := plan.Watches[i]
		outpoint := watch.Outpoint
		//nolint:contextcheck // watcher root owns registration lifetime
		registration, err := w.backend.RegisterSpend(
			w.ctx, &outpoint, watch.PkScript, watch.HeightHint,
		)
		if err != nil {
			cancelSpendRegistrations(registrations)

			return fmt.Errorf("register channel source spend "+
				"%s: %w", outpoint, err)
		}
		registrations = append(registrations, registration)
	}

	w.mu.Lock()
	if _, ok := w.registrations[id]; ok {
		w.mu.Unlock()
		cancelSpendRegistrations(registrations)

		return nil
	}
	w.registrations[id] = registrations
	w.mu.Unlock()

	for i := range registrations {
		w.wg.Add(1)
		go w.waitForSpend(id, registrations[i])
	}

	return nil
}

// waitForSpend waits for one confirmed ancestor spend or shutdown.
func (w *arkChannelSourceWatcher) waitForSpend(id arkchannel.ID,
	registration *chainsource.SpendRegistration) {

	defer w.wg.Done()
	select {
	case detail, ok := <-registration.Spend:
		if !ok || detail == nil || detail.SpentOutPoint == nil ||
			detail.SpenderTxHash == nil {
			return
		}
		w.handleSpend(id, &arkchannel.SourceSpent{
			OutPoint:     *detail.SpentOutPoint,
			SpendingTxID: *detail.SpenderTxHash,
		})

	case <-registration.Done:
	case <-w.ctx.Done():
	}
}

// handleSpend cancels sibling watches and retries the durable event until the
// FSM accepts it. The chain notifier delivers a confirmed spend once, so a
// transient database or side-effect failure must not discard that evidence.
func (w *arkChannelSourceWatcher) handleSpend(id arkchannel.ID,
	event *arkchannel.SourceSpent) {

	w.mu.Lock()
	registrations, ok := w.registrations[id]
	if ok {
		delete(w.registrations, id)
	}
	sink := w.sink
	w.mu.Unlock()
	if !ok || sink == nil {
		return
	}
	cancelSpendRegistrations(registrations)
	attempts := 0
	for {
		_, err := sink.Apply(w.ctx, id, event)
		if err == nil {
			if attempts > 0 {
				w.log.InfoS(
					w.ctx,
					"Recovered Ark channel after source "+
						"spend",
					btclog.Fmt("channel_id", "%x", id[:]),
					btclog.Fmt("attempts",
						"%d", attempts+1),
				)
			}

			return
		}
		attempts++
		if attempts == 1 || attempts%30 == 0 {
			w.log.WarnS(
				w.ctx,
				"Failed to recover Ark channel after source "+
					"spend",
				err,
				btclog.Fmt("channel_id", "%x", id[:]),
				btclog.Fmt(
					"spent_outpoint", "%v", event.OutPoint,
				),
				btclog.Fmt(
					"spending_txid", "%v",
					event.SpendingTxID,
				),
				btclog.Fmt("attempts", "%d", attempts),
			)
		}

		timer := time.NewTimer(w.retryDelay)
		select {
		case <-timer.C:
		case <-w.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			return
		}
	}
}

// Stop cancels all registrations and waits for their goroutines.
func (w *arkChannelSourceWatcher) Stop() {
	if w == nil {
		return
	}
	w.cancel()
	w.mu.Lock()
	for id, registrations := range w.registrations {
		cancelSpendRegistrations(registrations)
		delete(w.registrations, id)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// cancelSpendRegistrations releases backend resources for a watch set.
func cancelSpendRegistrations(registrations []*chainsource.SpendRegistration) {
	for _, registration := range registrations {
		if registration != nil && registration.Cancel != nil {
			registration.Cancel()
		}
	}
}
