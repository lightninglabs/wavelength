package chainbackends

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// lndNeutrinoBroadcastMsg mirrors the PackageMsg that a neutrino-backed lnd
// returns from WalletKit.SubmitPackage. A light client has no mempool and
// cannot validate or atomically accept a package, so lnd instead broadcasts
// each transaction individually over P2P (relying on a peer's 1p1c relay to
// reassemble the package) and reports this sentinel rather than "success". It
// is kept in sync with lnd's btcwallet.neutrinoBroadcastMsg, which is
// unexported there and so must be matched by value.
const lndNeutrinoBroadcastMsg = "broadcast-unverified"

// TxBroadcaster is a minimal interface for broadcasting transactions. This
// allows LNDBackend to work with both lnwallet.WalletController (in-process
// lnd) and lndclient wrappers (remote lnd via gRPC).
type TxBroadcaster interface {
	// PublishTransaction broadcasts the given transaction to the network.
	// The label parameter is optional and may be used for wallet tracking.
	PublishTransaction(ctx context.Context, tx *wire.MsgTx,
		label string) error
}

// PackageSubmitter atomically submits parent+child transaction packages.
// Bitcoind-backed implementations can satisfy this interface to expose v3
// package relay through the LND chain backend.
type PackageSubmitter interface {
	// SubmitPackage submits the package. The maxFeeRate parameter is
	// optional and nil leaves the node default unchanged. The context
	// controls cancellation/timeout for the underlying RPC.
	SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
		child *wire.MsgTx,
		maxFeeRate *float64) (*btcjson.SubmitPackageResult, error)
}

// LNDBackend implements the chainsource.ChainBackend interface by wrapping
// lnd's chain notification and fee estimation interfaces. This backend provides
// full-node functionality and is suitable for production deployments where lnd
// is available.
//
// The backend delegates all operations to the underlying lnd components:
// chainntnfs for notifications and chainfee for fee estimation.
type LNDBackend struct {
	// notifier provides chain notification services from lnd.
	notifier chainntnfs.ChainNotifier

	// feeEstimator provides fee estimation services from lnd.
	feeEstimator chainfee.Estimator

	// broadcaster provides transaction broadcasting capabilities.
	broadcaster TxBroadcaster

	// packageSubmitter optionally provides atomic package relay support.
	packageSubmitter PackageSubmitter

	// Log is an optional logger for this backend. If None, the backend
	// falls back to extracting a logger from context.
	Log fn.Option[btclog.Logger]
}

// NewLNDBackend creates a new LNDBackend instance with the given lnd
// components. All parameters must be non-nil.
func NewLNDBackend(notifier chainntnfs.ChainNotifier,
	feeEstimator chainfee.Estimator,
	broadcaster TxBroadcaster) *LNDBackend {

	return &LNDBackend{
		notifier:     notifier,
		feeEstimator: feeEstimator,
		broadcaster:  broadcaster,
	}
}

// SetPackageSubmitter attaches optional package relay support to the backend.
func (b *LNDBackend) SetPackageSubmitter(packageSubmitter PackageSubmitter) {
	b.packageSubmitter = packageSubmitter
}

// logger returns the configured logger, falling back to the context logger.
func (b *LNDBackend) logger(ctx context.Context) btclog.Logger {
	return b.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// EstimateFee returns the estimated fee rate in satoshis per vbyte for the
// given confirmation target. The fee estimator will provide the rate needed to
// confirm within the target number of blocks.
func (b *LNDBackend) EstimateFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	b.logger(ctx).DebugS(ctx, "Estimating fee rate",
		slog.Int("target_confs", int(targetConf)),
	)

	// Get the fee rate in sat/kw (satoshis per 1000 weight units) from
	// the estimator.
	feePerKw, err := b.feeEstimator.EstimateFeePerKW(targetConf)
	if err != nil {
		return 0, fmt.Errorf("failed to estimate fee: %w", err)
	}

	// Convert from sat/kw to sat/vbyte using the chainfee package's
	// built-in conversion method.
	//
	// The conversion is:
	//   - 1 vbyte = 4 weight units (by definition)
	//   - 1 kw = 1000 weight units
	//   - Therefore: sat/vbyte = (sat/kw) * (1kw/1000wu) * (4wu/1vb)
	//                          = (sat/kw) / 250
	//
	// The FeePerVByte() method handles this conversion correctly.
	satPerVByte := feePerKw.FeePerVByte()

	b.logger(ctx).DebugS(ctx, "Fee rate estimated",
		slog.Int("target_confs", int(targetConf)),
		slog.Int64("sat_per_kw", int64(feePerKw)),
		slog.Int64("sat_per_vbyte", int64(satPerVByte)),
	)

	return btcutil.Amount(satPerVByte), nil
}

// BestBlock returns the current best block height and hash from lnd's view of
// the blockchain. We register for a single block notification to get the
// current tip.
func (b *LNDBackend) BestBlock(ctx context.Context) (int32, chainhash.Hash,
	error) {

	b.logger(ctx).DebugS(ctx, "Querying best block from LND")

	// Register for a single block notification to get the current tip.
	event, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return 0, chainhash.Hash{}, fmt.Errorf("failed to register "+
			"for blocks: %w", err)
	}
	defer event.Cancel()

	// The first notification should be the current tip.
	select {
	case epoch, ok := <-event.Epochs:
		if !ok {
			return 0, chainhash.Hash{}, fmt.Errorf("block epoch " +
				"channel closed")
		}

		if epoch.Hash == nil {
			return 0, chainhash.Hash{}, fmt.Errorf("block epoch " +
				"has nil hash")
		}

		b.logger(ctx).InfoS(ctx, "Best block retrieved",
			slog.Int("height", int(epoch.Height)),
			btclog.Hex("hash", epoch.Hash[:]),
		)

		return epoch.Height, *epoch.Hash, nil

	case <-ctx.Done():
		return 0, chainhash.Hash{}, ctx.Err()
	}
}

// TestMempoolAccept tests whether one or more transactions would be
// accepted by the mempool. LND's WalletController does not expose a
// testmempoolaccept equivalent, so every call returns "not supported"
// here — callers that treat preflight as best-effort should log and
// continue.
func (b *LNDBackend) TestMempoolAccept(_ context.Context, _ ...*wire.MsgTx) (
	[]chainsource.MempoolAcceptResult, error) {

	// LND's WalletController doesn't provide a test mempool accept
	// interface. This would require direct RPC access to the underlying
	// Bitcoin node.
	return nil, fmt.Errorf("test mempool accept not supported by LND " +
		"backend")
}

// BroadcastTx broadcasts a transaction to the network using the configured
// broadcaster.
func (b *LNDBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	txHash := tx.TxHash()
	b.logger(ctx).InfoS(ctx, "Broadcasting transaction via LND",
		btclog.Hex("txid", txHash[:]),
		slog.String("label", label),
	)

	err := b.broadcaster.PublishTransaction(ctx, tx, label)
	if err != nil {
		return fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	b.logger(ctx).InfoS(ctx, "Transaction broadcast successfully",
		btclog.Hex("txid", txHash[:]),
	)

	return nil
}

// SubmitPackage submits a parent+child package through the configured
// PackageSubmitter. This is required for v3 package relay when a fee-paying
// child must accompany otherwise non-relayable parents.
func (b *LNDBackend) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx) error {

	if b.packageSubmitter == nil {
		return fmt.Errorf("package submission not supported by LND " +
			"backend")
	}

	result, err := b.packageSubmitter.SubmitPackage(
		ctx, parents, child, nil,
	)
	if err != nil {
		return fmt.Errorf("submit package RPC: %w", err)
	}
	if result == nil {
		return fmt.Errorf("submit package RPC returned nil result")
	}

	// Log per-tx results and collect errors.
	var txErrors []error
	for wtxid, txResult := range result.TxResults {
		b.logger(ctx).DebugS(ctx, "Package tx result",
			slog.String("wtxid", wtxid),
			slog.String("txid", txResult.TxID.String()),
		)

		if txResult.Error != nil {
			txErrors = append(
				txErrors, NewPackageTxError(
					wtxid, txResult.TxID, *txResult.Error,
				),
			)
		}
	}

	// A neutrino-backed lnd cannot return a package-accept verdict; it
	// broadcasts each tx individually over P2P and reports the best-effort
	// sentinel instead of "success". The transactions are already on the
	// wire and carry no per-tx errors, so treat this as a successful
	// broadcast and let the confirmation watch decide the outcome, rather
	// than failing the submit as if the package had been rejected.
	if result.PackageMsg == lndNeutrinoBroadcastMsg && len(txErrors) == 0 {
		b.logger(ctx).InfoS(
			ctx,
			"Package broadcast best-effort via light client",
			slog.Int("tx_count", len(parents)+1),
		)

		return nil
	}

	// On rejection, only wrap txErrors via %w when we actually have
	// per-tx errors to carry. errors.Join on an empty slice returns
	// nil, and fmt.Errorf("%w", nil) produces "%!w(<nil>)" in the
	// message, which makes diagnostics and error matching brittle.
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
		slog.Int("parent_count", len(parents)),
	)

	return nil
}

// RegisterConf registers for confirmation notifications using lnd's chain
// notifier. The registration returns a ConfRegistration with channels for
// receiving confirmation events.
//
//nolint:contextcheck // returned registration Cancel owns forwarder lifetime
func (b *LNDBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

	b.logger(ctx).DebugS(ctx, "Registering for confirmation notifications",
		slog.Int("num_confs", int(numConfs)),
		slog.Int("height_hint", int(heightHint)),
		slog.Bool("include_block", includeBlock),
	)

	// Build options for lnd's notifier. If includeBlock is true, we use
	// WithIncludeBlock() to request the full block in the confirmation.
	var opts []chainntnfs.NotifierOption
	if includeBlock {
		opts = append(opts, chainntnfs.WithIncludeBlock())
	}

	event, err := b.notifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register confirmation: %w",
			err)
	}

	// The caller context only scopes registration setup. Keep the delivery
	// forwarder alive until the registration itself is cancelled so
	// confirmations are not dropped when the originating actor request
	// context is released.
	notifyCtx, cancel := context.WithCancel(context.Background())

	// Create a channel to convert lnd's TxConfirmation to our type.
	confChan := make(chan *chainsource.TxConfirmation, 1)

	go func() {
		defer close(confChan)
		defer cancel()
		defer event.Cancel()

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

			confChan <- conf

		case <-notifyCtx.Done():
			return
		}
	}()

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Cancel: func() {
			cancel()
			event.Cancel()
		},
	}, nil
}

// RegisterSpend registers for spend notifications using lnd's chain notifier.
// The registration returns a SpendRegistration with channels for receiving
// spend events.
//
//nolint:contextcheck // returned registration Cancel owns forwarder lifetime
func (b *LNDBackend) RegisterSpend(ctx context.Context, outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainsource.SpendRegistration,
	error) {

	b.logger(ctx).DebugS(ctx, "Registering for spend notifications",
		slog.String("outpoint", outpoint.String()),
		slog.Int("height_hint", int(heightHint)),
	)

	// Register with lnd's notifier.
	event, err := b.notifier.RegisterSpendNtfn(
		outpoint, pkScript, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register spend: %w", err)
	}

	// Keep spend delivery alive independently of the actor request context.
	notifyCtx, cancel := context.WithCancel(context.Background())

	// Create a channel to convert lnd's SpendDetail to our type.
	spendChan := make(chan *chainsource.SpendDetail, 1)

	// Start a goroutine to convert and forward the spend.
	go func() {
		defer close(spendChan)
		defer cancel()
		defer event.Cancel()

		select {
		case lndSpend, ok := <-event.Spend:
			if !ok {
				return
			}

			// Convert to our type.
			spend := &chainsource.SpendDetail{
				SpentOutPoint:     lndSpend.SpentOutPoint,
				SpenderTxHash:     lndSpend.SpenderTxHash,
				SpendingTx:        lndSpend.SpendingTx,
				SpenderInputIndex: lndSpend.SpenderInputIndex,
				SpendingHeight:    lndSpend.SpendingHeight,
			}

			spendChan <- spend

		case <-notifyCtx.Done():
			return
		}
	}()

	return &chainsource.SpendRegistration{
		Spend: spendChan,
		Cancel: func() {
			cancel()
			event.Cancel()
		},
	}, nil
}

// RegisterBlocks registers for new block notifications using lnd's chain
// notifier. The registration returns a BlockRegistration with a channel for
// receiving block events.
func (b *LNDBackend) RegisterBlocks(ctx context.Context) (
	*chainsource.BlockRegistration, error) {

	b.logger(ctx).InfoS(ctx, "Registering for block epoch notifications")

	// Register with lnd's notifier. Pass nil for the best known block to
	// get the current tip immediately.
	event, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to register for blocks: %w", err)
	}

	// Create a channel to convert lnd's BlockEpoch to our type.
	epochChan := make(chan *chainsource.BlockEpoch, 10)

	// Start a goroutine to convert and forward block epochs.
	go func() {
		defer close(epochChan)
		defer event.Cancel()

		for {
			select {
			case lndEpoch, ok := <-event.Epochs:
				if !ok {
					return
				}

				// Skip epochs with nil hash (shouldn't happen
				// in practice but defensive check).
				if lndEpoch.Hash == nil {
					continue
				}

				// Extract timestamp if block header is present.
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

				// Send to our channel, respecting context
				// cancellation.
				select {
				case epochChan <- epoch:
				case <-ctx.Done():
					return
				}

			case <-ctx.Done():
				return
			}
		}
	}()

	return &chainsource.BlockRegistration{
		Epochs: epochChan,
		Cancel: event.Cancel,
	}, nil
}

// Start initializes the LND backend by starting the notifier and fee
// estimator.
func (b *LNDBackend) Start() error {
	b.logger(context.TODO()).InfoS(context.TODO(),
		"Starting LND chain backend")

	// Start the notifier.
	if err := b.notifier.Start(); err != nil {
		return fmt.Errorf("failed to start notifier: %w", err)
	}

	// Start the fee estimator.
	if err := b.feeEstimator.Start(); err != nil {
		// Try to stop the notifier since we failed.
		_ = b.notifier.Stop()

		return fmt.Errorf("failed to start fee estimator: %w", err)
	}

	b.logger(context.TODO()).InfoS(context.TODO(),
		"LND chain backend started successfully")

	return nil
}

// Stop shuts down the LND backend by stopping the notifier and fee estimator.
func (b *LNDBackend) Stop() error {
	b.logger(context.TODO()).InfoS(context.TODO(),
		"Stopping LND chain backend")

	var errs []error

	// Stop the notifier.
	if err := b.notifier.Stop(); err != nil {
		errs = append(
			errs, fmt.Errorf("failed to stop notifier: %w", err),
		)
	}

	// Stop the fee estimator.
	if err := b.feeEstimator.Stop(); err != nil {
		errs = append(
			errs, fmt.Errorf("failed to stop fee estimator: %w",
				err),
		)
	}

	b.logger(context.TODO()).InfoS(context.TODO(),
		"LND chain backend stopped")

	return errors.Join(errs...)
}

// Ensure LNDBackend implements ChainBackend at compile time.
var _ chainsource.ChainBackend = (*LNDBackend)(nil)
