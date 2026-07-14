//go:build !js || !wasm

package btcwbackend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/neutrinonotify"
	"github.com/lightningnetwork/lnd/channeldb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ChainBackend implements chainsource.ChainBackend using neutrino's
// native chain notification system and a WebAPI-based fee estimator.
// This provides event-driven confirmation, spend, and block
// notifications without polling — neutrino's compact block filters
// enable efficient client-side filtering.
type ChainBackend struct {
	// neutrinoCS is the running neutrino chain service used for
	// broadcasting transactions and querying the best block.
	neutrinoCS *neutrino.ChainService

	// notifier provides chain notification services backed by
	// neutrino's compact block filter scanning.
	notifier *neutrinonotify.NeutrinoNotifier

	// feeEstimator provides fee estimation from a web API since
	// neutrino has no mempool visibility.
	feeEstimator *chainfee.WebAPIEstimator

	// packageSubmitter optionally provides direct package relay. Neutrino
	// cannot submit v3 parent+child packages atomically over P2P.
	packageSubmitter chainbackends.PackageSubmitter

	// hintDB is the kvdb backend for the height hint cache. We keep
	// a reference so we can close it on Stop().
	hintDB kvdb.Backend

	// Log is an optional logger for this backend.
	Log fn.Option[btclog.Logger]

	// startOnce ensures Start() logic runs exactly once, even if
	// called from both Wallet.Start() and daemon wiring.
	startOnce sync.Once

	// startErr caches the error from the first Start() call so
	// subsequent calls return the same result.
	startErr error

	// stopOnce ensures Stop() logic runs exactly once, preventing
	// double-close panics on the hint DB and notifier.
	stopOnce sync.Once

	// quit is closed by Stop() to unblock a Start() that is still
	// waiting for the neutrino backend to become current, so shutdown
	// during the initial sync wait does not hang.
	quit chan struct{}

	// startStopMu serializes the notifier/fee-estimator start-vs-stop
	// transition. Start() only starts them, and Stop() only reads
	// notifierStarted to decide whether to tear them down, while
	// holding this lock. Without it, a Stop() racing the moment the
	// backend becomes current could observe notifierStarted=false,
	// skip teardown, and then let Start() start (and leak) the
	// subsystems after Stop() has already returned.
	startStopMu sync.Mutex

	// notifierStarted records whether Start() started the notifier and
	// fee estimator, so Stop() knows whether to tear them down. Guarded
	// by startStopMu.
	notifierStarted bool
}

// NewChainBackend creates a new neutrino-backed chain backend. The
// neutrino service must already be started before calling this.
func NewChainBackend(svc *NeutrinoService, feeURL string,
	feeMinTimeout, feeMaxTimeout time.Duration, hintDBPath string,
	logger btclog.Logger) (*ChainBackend, error) {

	// Open the height hint cache database, creating it if it does
	// not yet exist (first run).
	hintDB, err := kvdb.Open(
		kvdb.BoltBackendName, hintDBPath, true, defaultDBTimeout, false,
	)
	if err != nil {
		hintDB, err = kvdb.Create(
			kvdb.BoltBackendName, hintDBPath, true,
			defaultDBTimeout, false,
		)
		if err != nil {
			return nil, fmt.Errorf("create hint cache db: %w", err)
		}
	}

	hintCache, err := channeldb.NewHeightHintCache(
		channeldb.CacheConfig{
			QueryDisable: false,
		},
		hintDB,
	)
	if err != nil {
		_ = hintDB.Close()

		return nil, fmt.Errorf("create height hint cache: %w", err)
	}

	// Create the NeutrinoNotifier for event-driven chain
	// notifications.
	notifier := neutrinonotify.New(
		svc.ChainService(), hintCache, hintCache, svc.BlockCache(),
	)

	// Create the fee estimator. For neutrino, we must use a web API
	// since there is no mempool access.
	feeSource := chainfee.SparseConfFeeSource{URL: feeURL}
	feeEstimator, err := chainfee.NewWebAPIEstimator(
		feeSource, false, feeMinTimeout, feeMaxTimeout,
	)
	if err != nil {
		_ = hintDB.Close()

		return nil, fmt.Errorf("create fee estimator: %w", err)
	}

	return &ChainBackend{
		neutrinoCS:   svc.ChainService(),
		notifier:     notifier,
		feeEstimator: feeEstimator,
		hintDB:       hintDB,
		Log:          fn.Some(logger),
		quit:         make(chan struct{}),
	}, nil
}

// SetPackageSubmitter attaches optional package relay support to the backend.
func (b *ChainBackend) SetPackageSubmitter(
	packageSubmitter chainbackends.PackageSubmitter) {

	b.packageSubmitter = packageSubmitter
}

// logger returns the configured logger, falling back to the context
// logger.
func (b *ChainBackend) logger(ctx context.Context) btclog.Logger {
	return b.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Chain-sync wait tuning. The neutrino notifier snapshots its
// block-epoch rescan start height from the chain service's best block
// exactly once, at notifier.Start(). Starting it before the backend
// has synced its block/filter headers anchors that snapshot far behind
// the tip, condemning the notifier to a slow, fragile historical
// rescan; so Start() blocks until IsCurrent first. These are named
// consts (not literals) so the cadence can be tuned in one place.
const (
	// chainSyncPollInterval is how often Start() re-checks whether the
	// neutrino backend has become current while waiting to start the
	// notifier. Header/filter-header sync (what IsCurrent gates on)
	// typically completes within tens of seconds, so a 1s cadence is
	// responsive without busy-looping.
	chainSyncPollInterval = time.Second

	// chainSyncLogInterval bounds how often Start() emits a progress
	// log line while waiting for sync. Matches the daemon's neutrino
	// sync-wait heartbeat so operators see roughly one line every 30s
	// rather than per-poll spam.
	chainSyncLogInterval = 30 * time.Second
)

// errChainBackendStopped is returned by Start() when the backend is
// stopped (via Stop closing quit) before the neutrino service becomes
// current, so a caller racing shutdown gets a clear signal instead of
// a nil error paired with a half-started backend.
var errChainBackendStopped = errors.New("chain backend stopped before " +
	"neutrino became current")

// Start initializes the chain backend. It first waits for the neutrino
// backend to become current (see the chain-sync tuning consts for why)
// and then starts the notifier and fee estimator.
func (b *ChainBackend) Start() error {
	b.startOnce.Do(func() {
		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Starting neutrino chain backend",
		)

		// Gate notifier start on the backend being current so the
		// notifier's one-shot rescan start height is the real chain
		// tip, not a stale mid-sync height.
		if !b.awaitChainCurrent(context.TODO()) {
			b.startErr = errChainBackendStopped

			return
		}

		// Take startStopMu to make "not stopped -> start subsystems ->
		// mark started" atomic against a concurrent Stop(). Re-check
		// quit under the lock: if Stop() already fired while we were
		// waiting above, bail without starting anything, so we never
		// leak a notifier/fee estimator past a completed Stop().
		b.startStopMu.Lock()

		select {
		case <-b.quit:
			b.startStopMu.Unlock()
			b.startErr = errChainBackendStopped

			return

		default:
		}

		if err := b.notifier.Start(); err != nil {
			b.startStopMu.Unlock()
			b.startErr = fmt.Errorf("start notifier: %w", err)

			return
		}

		if err := b.feeEstimator.Start(); err != nil {
			_ = b.notifier.Stop()
			b.startStopMu.Unlock()
			b.startErr = fmt.Errorf("start fee estimator: %w", err)

			return
		}

		// Record that the notifier and fee estimator are up so Stop()
		// knows to tear them down (and, conversely, skips them when a
		// shutdown interrupts the sync wait above).
		b.notifierStarted = true
		b.startStopMu.Unlock()

		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Neutrino chain backend started",
		)
	})

	return b.startErr
}

// awaitChainCurrent blocks until the neutrino backend has synced its
// block and filter headers (IsCurrent) or Stop() closes b.quit. It
// emits a progress log line at roughly chainSyncLogInterval cadence
// while waiting. It returns false only if the backend was stopped
// before it became current. ctx is used only for logging: the wait is
// intentionally cancellable solely via Stop() (b.quit), matching the
// backend's shutdown model, not via ctx cancellation.
func (b *ChainBackend) awaitChainCurrent(ctx context.Context) bool {
	if b.neutrinoCS.IsCurrent() {
		return true
	}

	b.logger(ctx).InfoS(ctx, "Waiting for neutrino backend to sync "+
		"before starting block notifier")

	logEvery := int(chainSyncLogInterval / chainSyncPollInterval)

	return waitUntilCurrent(
		b.neutrinoCS.IsCurrent, b.quit, chainSyncPollInterval,
		func(attempt int) {
			if logEvery > 0 && attempt%logEvery == 0 {
				b.logger(ctx).InfoS(ctx, "Still waiting for "+
					"neutrino backend to sync before "+
					"starting block notifier")
			}
		},
	)
}

// waitUntilCurrent blocks until isCurrent reports true or quit is
// closed, polling at pollInterval. onWait, if non-nil, is invoked after
// each poll that still reports not-current with a 1-based attempt
// counter, letting callers throttle progress logging without this
// helper needing a clock. Returns true if the backend became current,
// false if quit fired first. Split out from Start so the wait logic is
// unit-testable without a live neutrino service.
func waitUntilCurrent(isCurrent func() bool, quit <-chan struct{},
	pollInterval time.Duration, onWait func(attempt int)) bool {

	if isCurrent() {
		return true
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for attempt := 1; ; attempt++ {
		select {
		case <-quit:
			return false

		case <-ticker.C:
			if isCurrent() {
				return true
			}

			if onWait != nil {
				onWait(attempt)
			}
		}
	}
}

// Stop shuts down the chain backend by stopping the notifier, fee
// estimator, and closing the hint cache database.
func (b *ChainBackend) Stop() error {
	var stopErr error

	b.stopOnce.Do(func() {
		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Stopping neutrino chain backend",
		)

		// Unblock a Start() still waiting for the backend to become
		// current so shutdown doesn't hang on the sync wait. Close
		// before taking startStopMu so any Start() that has not yet
		// entered its critical section observes the closed quit once
		// it acquires the lock and skips starting the subsystems.
		close(b.quit)

		// Read notifierStarted under startStopMu so we serialize
		// against a Start() that is mid-transition: either it has
		// already finished (we observe true and tear down) or it has
		// not yet started the subsystems (it will observe the closed
		// quit above and bail). startOnce/stopOnce guarantee each body
		// runs once, so no further Start() can start them after this.
		b.startStopMu.Lock()
		notifierStarted := b.notifierStarted
		b.startStopMu.Unlock()

		var errs []error

		// Only tear down the notifier and fee estimator if Start()
		// actually started them; a shutdown during the pre-notifier
		// sync wait leaves them unstarted.
		if notifierStarted {
			if err := b.notifier.Stop(); err != nil {
				errs = append(
					errs, fmt.Errorf("stop notifier: %w",
						err),
				)
			}

			if err := b.feeEstimator.Stop(); err != nil {
				errs = append(
					errs, fmt.Errorf("stop fee "+
						"estimator: %w", err),
				)
			}
		}

		if err := b.hintDB.Close(); err != nil {
			errs = append(
				errs, fmt.Errorf("close hint db: %w", err),
			)
		}

		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Neutrino chain backend stopped",
		)

		stopErr = errors.Join(errs...)
	})

	return stopErr
}

// EstimateFee returns the estimated fee rate in satoshis per vbyte
// for the given confirmation target. The fee estimator queries a web
// API since neutrino has no mempool visibility.
func (b *ChainBackend) EstimateFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	b.logger(ctx).DebugS(ctx, "Estimating fee rate",
		slog.Int("target_confs", int(targetConf)),
	)

	feePerKw, err := b.feeEstimator.EstimateFeePerKW(targetConf)
	if err != nil {
		return 0, fmt.Errorf("estimate fee: %w", err)
	}

	// Convert from sat/kw to sat/vbyte.
	satPerVByte := feePerKw.FeePerVByte()

	b.logger(ctx).DebugS(ctx, "Fee rate estimated",
		slog.Int("target_confs", int(targetConf)),
		slog.Int64("sat_per_vbyte", int64(satPerVByte)),
	)

	return btcutil.Amount(satPerVByte), nil
}

// BestBlock returns the current best block height and hash from
// neutrino's view of the blockchain.
func (b *ChainBackend) BestBlock(ctx context.Context) (int32, chainhash.Hash,
	error) {

	b.logger(ctx).DebugS(ctx, "Querying best block from neutrino")

	bs, err := b.neutrinoCS.BestBlock()
	if err != nil {
		return 0, chainhash.Hash{}, fmt.Errorf("neutrino best "+
			"block: %w", err)
	}

	b.logger(ctx).DebugS(ctx, "Best block retrieved",
		slog.Int("height", int(bs.Height)),
		btclog.Hex("hash", bs.Hash[:]),
	)

	return bs.Height, bs.Hash, nil
}

// TestMempoolAccept is not supported by the neutrino backend since
// neutrino does not maintain a mempool.
func (b *ChainBackend) TestMempoolAccept(_ context.Context, _ ...*wire.MsgTx) (
	[]chainsource.MempoolAcceptResult, error) {

	return nil, fmt.Errorf("test mempool accept not supported by " +
		"neutrino backend")
}

// BroadcastTx broadcasts a transaction to the Bitcoin P2P network
// via neutrino's connected peers.
func (b *ChainBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	txHash := tx.TxHash()
	b.logger(ctx).InfoS(ctx, "Broadcasting transaction via neutrino",
		slog.String("txid", txHash.String()),
		slog.String("label", label),
	)

	if err := b.neutrinoCS.SendTransaction(tx); err != nil {
		return fmt.Errorf("broadcast transaction: %w", err)
	}

	b.logger(ctx).InfoS(ctx, "Transaction broadcast successfully",
		slog.String("txid", txHash.String()),
	)

	return nil
}

// SubmitPackage submits a parent+child package through the configured direct
// package submitter.
func (b *ChainBackend) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx) error {

	if b.packageSubmitter == nil {
		return fmt.Errorf("package submission not supported by " +
			"neutrino backend")
	}

	result, err := b.packageSubmitter.SubmitPackage(
		ctx, parents, child, nil,
	)
	if err != nil {
		return fmt.Errorf("submit package RPC: %w", err)
	}

	return b.handlePackageResult(ctx, len(parents), result)
}

// handlePackageResult validates bitcoind's package relay result.
func (b *ChainBackend) handlePackageResult(ctx context.Context, parentCount int,
	result *btcjson.SubmitPackageResult) error {

	if result == nil {
		return fmt.Errorf("submit package RPC returned nil result")
	}

	var txErrors []error
	for wtxid, txResult := range result.TxResults {
		b.logger(ctx).DebugS(ctx, "Package tx result",
			slog.String("wtxid", wtxid),
			slog.String("txid", txResult.TxID.String()),
		)

		if txResult.Error != nil {
			pkgErr := chainbackends.NewPackageTxError(
				wtxid, txResult.TxID, *txResult.Error,
			)
			txErrors = append(txErrors, pkgErr)
		}
	}

	if result.PackageMsg != "success" {
		if len(txErrors) == 0 {
			return fmt.Errorf("package not accepted: %s",
				result.PackageMsg)
		}

		return fmt.Errorf("package not accepted: %s: %w",
			result.PackageMsg, errors.Join(txErrors...))
	}

	if len(txErrors) > 0 {
		return fmt.Errorf("package tx rejected: %w",
			errors.Join(txErrors...))
	}

	b.logger(ctx).InfoS(ctx, "Submitted transaction package",
		slog.Int("parent_count", parentCount),
	)

	return nil
}

// RegisterConf registers for confirmation notifications using
// neutrino's chain notifier. The registration returns a
// ConfRegistration with channels for receiving confirmation events.
//
//nolint:contextcheck // returned registration Cancel owns forwarder lifetime
func (b *ChainBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

	b.logger(ctx).DebugS(ctx, "Registering for confirmation notifications",
		slog.Int("num_confs", int(numConfs)),
		slog.Int("height_hint", int(heightHint)),
		slog.Bool("include_block", includeBlock),
	)

	var opts []chainntnfs.NotifierOption
	if includeBlock {
		opts = append(opts, chainntnfs.WithIncludeBlock())
	}

	event, err := b.notifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("register confirmation: %w", err)
	}

	// The caller context only scopes registration setup. Keep the
	// delivery forwarder alive until the registration itself is
	// cancelled.
	notifyCtx, cancel := context.WithCancel(context.Background())

	// Wrap event.Cancel in a sync.Once to prevent double-cancel
	// panics. Both the forwarding goroutine and the returned Cancel
	// closure call this, but lnd's notificationDispatcher panics
	// if Cancel is sent twice for the same registration ID
	// (nil map lookup after delete).
	var cancelOnce sync.Once
	safeCancel := func() {
		cancelOnce.Do(event.Cancel)
	}

	confChan := make(chan *chainsource.TxConfirmation, 1)

	go func() {
		defer close(confChan)
		defer cancel()
		defer safeCancel()

		select {
		case lndConf, ok := <-event.Confirmed:
			if !ok {
				return
			}

			conf := &chainsource.TxConfirmation{
				BlockHash:   lndConf.BlockHash,
				BlockHeight: lndConf.BlockHeight,
				TxIndex:     lndConf.TxIndex,
				Tx:          lndConf.Tx,
				Block:       lndConf.Block,
			}

			select {
			case confChan <- conf:
			case <-notifyCtx.Done():
				return
			}

		case <-notifyCtx.Done():
			return
		}
	}()

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Cancel: func() {
			cancel()
			safeCancel()
		},
	}, nil
}

// RegisterSpend registers for spend notifications using neutrino's
// chain notifier.
//
//nolint:contextcheck // returned registration Cancel owns forwarder lifetime
func (b *ChainBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint uint32) (
	*chainsource.SpendRegistration, error) {

	b.logger(ctx).DebugS(ctx, "Registering for spend notifications",
		slog.String("outpoint", outpoint.String()),
		slog.Int("height_hint", int(heightHint)),
	)

	event, err := b.notifier.RegisterSpendNtfn(
		outpoint, pkScript, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend: %w", err)
	}

	notifyCtx, cancel := context.WithCancel(context.Background())

	// Wrap event.Cancel in a sync.Once to prevent double-cancel
	// panics — same rationale as RegisterConf.
	var cancelOnce sync.Once
	safeCancel := func() {
		cancelOnce.Do(event.Cancel)
	}

	spendChan := make(chan *chainsource.SpendDetail, 1)

	go func() {
		defer close(spendChan)
		defer cancel()
		defer safeCancel()

		select {
		case lndSpend, ok := <-event.Spend:
			if !ok {
				return
			}

			spend := &chainsource.SpendDetail{
				SpentOutPoint:     lndSpend.SpentOutPoint,
				SpenderTxHash:     lndSpend.SpenderTxHash,
				SpendingTx:        lndSpend.SpendingTx,
				SpenderInputIndex: lndSpend.SpenderInputIndex,
				SpendingHeight:    lndSpend.SpendingHeight,
			}

			select {
			case spendChan <- spend:
			case <-notifyCtx.Done():
				return
			}

		case <-notifyCtx.Done():
			return
		}
	}()

	return &chainsource.SpendRegistration{
		Spend: spendChan,
		Cancel: func() {
			cancel()
			safeCancel()
		},
	}, nil
}

// RegisterBlocks registers for new block notifications using
// neutrino's chain notifier.
//
//nolint:contextcheck // returned registration Cancel owns forwarder lifetime
func (b *ChainBackend) RegisterBlocks(ctx context.Context) (
	*chainsource.BlockRegistration, error) {

	b.logger(ctx).InfoS(
		ctx, "Registering for block epoch notifications",
	)

	event, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, fmt.Errorf("register blocks: %w", err)
	}

	// Use an independent context so the forwarding goroutine
	// outlives the caller's context and can be cancelled via
	// the returned Cancel function.
	notifyCtx, cancel := context.WithCancel(context.Background())

	// Wrap event.Cancel in a sync.Once to prevent double-cancel
	// panics — same rationale as RegisterConf.
	var cancelOnce sync.Once
	safeCancel := func() {
		cancelOnce.Do(event.Cancel)
	}

	epochChan := make(chan *chainsource.BlockEpoch, 10)

	go func() {
		defer close(epochChan)
		defer cancel()
		defer safeCancel()

		for {
			select {
			case lndEpoch, ok := <-event.Epochs:
				if !ok {
					return
				}

				if lndEpoch.Hash == nil {
					continue
				}

				var timestamp int64
				if lndEpoch.BlockHeader != nil {
					ts := lndEpoch.BlockHeader.Timestamp
					timestamp = ts.Unix()
				}

				epoch := &chainsource.BlockEpoch{
					Hash:      *lndEpoch.Hash,
					Height:    lndEpoch.Height,
					Timestamp: timestamp,
				}

				select {
				case epochChan <- epoch:
				case <-notifyCtx.Done():
					return
				}

			case <-notifyCtx.Done():
				return
			}
		}
	}()

	return &chainsource.BlockRegistration{
		Epochs: epochChan,
		Cancel: func() {
			cancel()
			safeCancel()
		},
	}, nil
}

// Compile-time check that ChainBackend implements
// chainsource.ChainBackend.
var _ chainsource.ChainBackend = (*ChainBackend)(nil)
