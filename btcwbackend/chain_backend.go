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
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/neutrino"
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
	// neutrino's compact block filter scanning. Stored under the
	// chainntnfs interface (concrete type is
	// *neutrinonotify.NeutrinoNotifier in production) so reorg-aware
	// forwarder tests can substitute a stub that drives the full
	// Confirmed / NegativeConf / Done lifecycle without spinning up a
	// neutrino chain service.
	notifier chainntnfs.ChainNotifier

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

// Start initializes the chain backend by starting the notifier and
// fee estimator.
func (b *ChainBackend) Start() error {
	b.startOnce.Do(func() {
		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Starting neutrino chain backend",
		)

		if err := b.notifier.Start(); err != nil {
			b.startErr = fmt.Errorf("start notifier: %w", err)

			return
		}

		if err := b.feeEstimator.Start(); err != nil {
			_ = b.notifier.Stop()
			b.startErr = fmt.Errorf("start fee estimator: %w", err)

			return
		}

		b.logger(context.TODO()).InfoS(
			context.TODO(),
			"Neutrino chain backend started",
		)
	})

	return b.startErr
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

		var errs []error

		if err := b.notifier.Stop(); err != nil {
			errs = append(
				errs, fmt.Errorf("stop notifier: %w", err),
			)
		}

		if err := b.feeEstimator.Stop(); err != nil {
			errs = append(
				errs, fmt.Errorf("stop fee estimator: %w", err),
			)
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

	// Create channels to convert neutrino's confirmation lifecycle
	// (which already drives chainntnfs's NegativeConf/Done channels
	// directly — neutrino emits these natively from its compact-
	// block-filter scanner) into our backend-agnostic types.
	// NegativeConf carries a reorg depth that the cross-backend
	// chainsource layer intentionally drops, so we forward a bare
	// struct{} signal on reorgChan instead.
	confChan := make(chan *chainsource.TxConfirmation, 1)
	reorgChan := make(chan uint64, 1)
	doneChan := make(chan struct{}, 1)

	go func() {
		// Defers run in LIFO order. event.Cancel() must run first
		// so the upstream notifier stops writing to its internal
		// channels before we cancel notifyCtx (which any in-flight
		// downstream sends are still using) and finally close the
		// outgoing chans. Reversing this order would race the
		// upstream notifier against closed channels.
		defer close(confChan)
		defer close(reorgChan)
		defer close(doneChan)
		defer cancel()
		defer safeCancel()

		for {
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

			case _, ok := <-event.NegativeConf:
				if !ok {
					event.NegativeConf = nil

					continue
				}

				select {
				case reorgChan <- uint64(0):
				case <-notifyCtx.Done():
					return
				}

			case _, ok := <-event.Done:
				if !ok {
					event.Done = nil

					continue
				}

				select {
				case doneChan <- struct{}{}:
				case <-notifyCtx.Done():
					return
				}

				return

			case <-notifyCtx.Done():
				return
			}
		}
	}()

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Reorged:   reorgChan,
		Done:      doneChan,
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

	// Create channels to convert neutrino's spend lifecycle into our
	// backend-agnostic types. neutrino's Reorg carries no payload, so
	// we forward a bare struct{} signal.
	spendChan := make(chan *chainsource.SpendDetail, 1)
	reorgChan := make(chan uint64, 1)
	doneChan := make(chan struct{}, 1)

	go func() {
		// LIFO defer: event.Cancel() first so the upstream notifier
		// stops writing, then cancel notifyCtx so in-flight downstream
		// sends unblock, and finally close the outgoing chans.
		defer close(spendChan)
		defer close(reorgChan)
		defer close(doneChan)
		defer cancel()
		defer safeCancel()

		for {
			select {
			case lndSpend, ok := <-event.Spend:
				if !ok {
					return
				}

				spend := &chainsource.SpendDetail{
					SpentOutPoint: lndSpend.SpentOutPoint,
					SpenderTxHash: lndSpend.SpenderTxHash,
					SpendingTx:    lndSpend.SpendingTx,
					SpenderInputIndex: lndSpend.
						SpenderInputIndex,
					SpendingHeight: lndSpend.SpendingHeight,
				}

				select {
				case spendChan <- spend:
				case <-notifyCtx.Done():
					return
				}

			case _, ok := <-event.Reorg:
				if !ok {
					event.Reorg = nil

					continue
				}

				select {
				case reorgChan <- uint64(0):
				case <-notifyCtx.Done():
					return
				}

			case _, ok := <-event.Done:
				if !ok {
					event.Done = nil

					continue
				}

				select {
				case doneChan <- struct{}{}:
				case <-notifyCtx.Done():
					return
				}

				return

			case <-notifyCtx.Done():
				return
			}
		}
	}()

	return &chainsource.SpendRegistration{
		Spend:   spendChan,
		Reorged: reorgChan,
		Done:    doneChan,
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
