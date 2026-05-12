package darepod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
)

const (
	// defaultBoardingSweepRefreshInterval controls watcher rescans for
	// pending sweeps and unresolved input watches.
	defaultBoardingSweepRefreshInterval = time.Minute

	// defaultBoardingSweepRebroadcastInterval is the minimum time between
	// repeated rebroadcast attempts for a sweep that is already published.
	defaultBoardingSweepRebroadcastInterval = 10 * time.Minute

	// boardingSweepBroadcastLabel is attached to boarding sweep txes.
	boardingSweepBroadcastLabel = "ark boarding timeout sweep"
)

// boardingSweepWatcher resumes pending boarding sweeps, rebroadcasts their raw
// transactions, and marks boarding intents swept after confirmed input spends.
type boardingSweepWatcher struct {
	store        *db.BoardingWalletStore
	chainBackend chainsource.ChainBackend
	log          btclog.Logger
	interval     time.Duration

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu            sync.Mutex
	registrations map[wire.OutPoint]func()
	rebroadcasts  map[chainhash.Hash]time.Time
}

// newBoardingSweepWatcher creates a watcher for persisted boarding sweeps.
func newBoardingSweepWatcher(store *db.BoardingWalletStore,
	chainBackend chainsource.ChainBackend, log btclog.Logger,
	interval time.Duration) *boardingSweepWatcher {

	if interval == 0 {
		interval = defaultBoardingSweepRefreshInterval
	}

	return &boardingSweepWatcher{
		store:         store,
		chainBackend:  chainBackend,
		log:           log,
		interval:      interval,
		registrations: make(map[wire.OutPoint]func()),
		rebroadcasts:  make(map[chainhash.Hash]time.Time),
	}
}

// startBoardingSweepWatcher starts the daemon-owned watcher for pending
// boarding sweep transactions. It is idempotent so wallet-unlock paths can call
// it after the chain backend becomes available.
func (s *Server) startBoardingSweepWatcher(ctx context.Context) error {
	s.boardingSweepWatcherMu.Lock()
	defer s.boardingSweepWatcherMu.Unlock()

	if s.boardingSweepWatcher != nil {
		return nil
	}
	if s.chainBackend == nil {
		return fmt.Errorf("chain backend not initialized")
	}

	watcher := newBoardingSweepWatcher(
		s.newBoardingStore(), s.chainBackend, s.subLogger(Subsystem), 0,
	)
	if err := watcher.Start(ctx); err != nil {
		return err
	}

	s.boardingSweepWatcher = watcher
	s.log.InfoS(ctx, "Boarding sweep watcher started")

	return nil
}

// getBoardingSweepWatcher returns the started watcher, if any.
func (s *Server) getBoardingSweepWatcher() *boardingSweepWatcher {
	s.boardingSweepWatcherMu.RLock()
	defer s.boardingSweepWatcherMu.RUnlock()

	return s.boardingSweepWatcher
}

// Start begins recovery for pending sweeps and periodically refreshes watches.
func (w *boardingSweepWatcher) Start(ctx context.Context) error {
	if w.store == nil {
		return fmt.Errorf("boarding sweep store missing")
	}
	if w.chainBackend == nil {
		return fmt.Errorf("chain backend missing")
	}

	w.ctx, w.cancel = context.WithCancel(ctx)
	// The initial scan is intentionally tied to the watcher lifetime so a
	// caller returning cannot tear down spend notifications.
	if err := w.Refresh(w.ctx); err != nil { //nolint:contextcheck
		return err
	}

	w.wg.Add(1)
	go w.run()

	return nil
}

// Stop cancels pending watches and waits for background work to finish.
func (w *boardingSweepWatcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}

	w.mu.Lock()
	for _, cancel := range w.registrations {
		cancel()
	}
	w.registrations = make(map[wire.OutPoint]func())
	w.mu.Unlock()

	w.wg.Wait()
}

// Refresh reloads pending sweeps, registers missing spend watches, and
// best-effort rebroadcasts unresolved sweep transactions.
func (w *boardingSweepWatcher) Refresh(ctx context.Context) error {
	sweeps, err := w.store.ListPendingBoardingSweeps(ctx)
	if err != nil {
		return err
	}

	for _, sweep := range sweeps {
		for _, input := range sweep.Inputs {
			switch input.Status {
			case db.BoardingSweepInputStatusPending,
				db.BoardingSweepInputStatusPublished:
			default:
				continue
			}

			// Watches follow the daemon watcher lifetime, not the
			// refresh caller.
			err := w.watchInput( //nolint:contextcheck
				w.ctx, input.Outpoint,
			)
			if err != nil {
				w.log.WarnS(ctx, "Unable to watch boarding "+
					"sweep input",
					err,
					slog.String(
						"outpoint",
						input.Outpoint.String(),
					),
					slog.String("txid",
						sweep.Txid.String()))
			}
		}

		if err := w.rebroadcastSweep(ctx, sweep); err != nil {
			w.log.WarnS(ctx, "Unable to rebroadcast boarding sweep",
				err,
				slog.String("txid", sweep.Txid.String()),
			)
		}
	}

	return nil
}

// run periodically refreshes pending sweep watches until shutdown.
func (w *boardingSweepWatcher) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := w.Refresh(w.ctx); err != nil {
				w.log.WarnS(
					w.ctx,
					"Unable to refresh boarding sweeps",
					err,
				)
			}

		case <-w.ctx.Done():
			return
		}
	}
}

// watchInput registers a confirmed-spend watch for a boarding outpoint if one
// is not already active.
func (w *boardingSweepWatcher) watchInput(ctx context.Context,
	outpoint wire.OutPoint) error {

	w.mu.Lock()
	if _, ok := w.registrations[outpoint]; ok {
		w.mu.Unlock()

		return nil
	}
	w.mu.Unlock()

	intent, err := w.store.GetIntent(ctx, outpoint)
	if err != nil {
		return fmt.Errorf("load boarding intent: %w", err)
	}

	txOut, err := boardingSweepTargetOutput(*intent)
	if err != nil {
		return fmt.Errorf("boarding target output: %w", err)
	}

	heightHint := uint32(intent.ChainInfo.ConfHeight)
	// Spend notifications must live until the watcher stops, even when a
	// short-lived RPC request was the trigger for this refresh.
	registration, err := w.chainBackend.RegisterSpend( //nolint:contextcheck
		w.ctx, &outpoint, txOut.PkScript, heightHint,
	)
	if err != nil {
		return fmt.Errorf("register spend: %w", err)
	}

	w.mu.Lock()
	if _, ok := w.registrations[outpoint]; ok {
		w.mu.Unlock()
		registration.Cancel()

		return nil
	}
	w.registrations[outpoint] = registration.Cancel
	w.mu.Unlock()

	w.wg.Add(1)
	go w.monitorInput(outpoint, registration)

	return nil
}

// monitorInput waits for a confirmed spend notification for one boarding
// outpoint and records it in the sweep store.
func (w *boardingSweepWatcher) monitorInput(outpoint wire.OutPoint,
	registration *chainsource.SpendRegistration) {

	defer w.wg.Done()
	defer func() {
		w.mu.Lock()
		delete(w.registrations, outpoint)
		w.mu.Unlock()
		registration.Cancel()
	}()

	select {
	case spend, ok := <-registration.Spend:
		if !ok || spend == nil {
			w.log.DebugS(w.ctx, "Boarding sweep spend watch closed",
				slog.String("outpoint", outpoint.String()),
			)

			return
		}

		if err := w.recordSpend(w.ctx, outpoint, spend); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				w.log.DebugS(
					w.ctx,
					"Boarding sweep spend already resolved",
					slog.String(
						"outpoint", outpoint.String(),
					),
				)

				return
			}

			w.log.WarnS(
				w.ctx,
				"Unable to record boarding sweep spend",
				err,
				slog.String("outpoint", outpoint.String()),
			)
		}

	case <-w.ctx.Done():
		return
	}
}

// recordSpend updates the pending sweep state for one confirmed spend event.
func (w *boardingSweepWatcher) recordSpend(ctx context.Context,
	outpoint wire.OutPoint, spend *chainsource.SpendDetail) error {

	spendingTxid, err := boardingSpendTxid(spend)
	if err != nil {
		return err
	}

	resolved, err := w.store.MarkBoardingSweepInputSpent(
		ctx, outpoint, spendingTxid, spend.SpendingHeight,
	)
	if err != nil {
		return err
	}

	w.log.InfoS(ctx, "Boarding sweep input spent",
		slog.String("outpoint", outpoint.String()),
		slog.String("spending_txid", spendingTxid.String()),
		slog.Int("height", int(spend.SpendingHeight)),
	)
	if resolved {
		w.log.InfoS(ctx, "Boarding sweep resolved",
			slog.String("spending_txid", spendingTxid.String()),
			slog.Int("height", int(spend.SpendingHeight)),
		)
	}

	return nil
}

// rebroadcastSweep rebroadcasts the exact persisted sweep transaction. It does
// not rebuild, replace, or fee-bump the sweep.
func (w *boardingSweepWatcher) rebroadcastSweep(ctx context.Context,
	sweep db.BoardingSweepRecord) error {

	if !w.shouldRebroadcastSweep(sweep) {
		return nil
	}

	err := w.chainBackend.BroadcastTx(
		ctx, sweep.Tx, boardingSweepBroadcastLabel,
	)
	if err != nil && !chainsource.IsIgnorableBroadcastError(err) {
		return fmt.Errorf("broadcast sweep: %w", err)
	}

	if sweep.Status == db.BoardingSweepStatusPending {
		if err := w.store.MarkBoardingSweepPublished(
			ctx, sweep.Txid,
		); err != nil {
			return fmt.Errorf("mark sweep published: %w", err)
		}
	}

	w.markSweepRebroadcast(sweep.Txid)

	return nil
}

// shouldRebroadcastSweep returns true when the watcher should try publishing a
// persisted sweep transaction. Pending rows are always retried so crash-before
// MarkPublished recovery is immediate; already-published rows are rate-limited.
func (w *boardingSweepWatcher) shouldRebroadcastSweep(
	sweep db.BoardingSweepRecord) bool {

	if sweep.Status == db.BoardingSweepStatusPending {
		return true
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	last, ok := w.rebroadcasts[sweep.Txid]
	if !ok {
		return true
	}

	return time.Since(last) >= defaultBoardingSweepRebroadcastInterval
}

// markSweepRebroadcast records a rebroadcast attempt for rate limiting.
func (w *boardingSweepWatcher) markSweepRebroadcast(txid chainhash.Hash) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.rebroadcasts[txid] = time.Now()
}

// boardingSpendTxid returns the txid for a spend notification.
func boardingSpendTxid(spend *chainsource.SpendDetail) (chainhash.Hash, error) {
	switch {
	case spend == nil:
		return chainhash.Hash{}, fmt.Errorf("missing spend detail")

	case spend.SpenderTxHash != nil:
		return *spend.SpenderTxHash, nil

	case spend.SpendingTx != nil:
		return spend.SpendingTx.TxHash(), nil

	default:
		return chainhash.Hash{}, fmt.Errorf("spend missing txid")
	}
}
