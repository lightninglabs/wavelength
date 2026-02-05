package chainbackends

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// TxBroadcaster is a minimal interface for broadcasting transactions. This
// allows LNDBackend to work with both lnwallet.WalletController (in-process
// lnd) and lndclient wrappers (remote lnd via gRPC).
type TxBroadcaster interface {
	// PublishTransaction broadcasts the given transaction to the network.
	// The label parameter is optional and may be used for wallet tracking.
	PublishTransaction(ctx context.Context, tx *wire.MsgTx,
		label string) error
}

// PackageSubmitter is an optional interface for atomic submission of
// parent+child transaction packages. This is required for V3 transactions
// with ephemeral anchors that cannot be broadcast individually.
// chain.BitcoindRPCClient satisfies this interface.
type PackageSubmitter interface {
	// SubmitPackage atomically submits a parent+child transaction
	// package. The maxFeeRate parameter is optional (nil for no limit).
	SubmitPackage(parents []*wire.MsgTx, child *wire.MsgTx,
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

	// packageSubmitter provides optional atomic package submission.
	// Not all deployments have direct bitcoind access — when nil,
	// SubmitPackage returns an "unsupported" error.
	packageSubmitter PackageSubmitter
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

// EstimateFee returns the estimated fee rate in satoshis per vbyte for the
// given confirmation target. The fee estimator will provide the rate needed to
// confirm within the target number of blocks.
func (b *LNDBackend) EstimateFee(ctx context.Context,
	targetConf uint32) (btcutil.Amount, error) {

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

	return btcutil.Amount(satPerVByte), nil
}

// BestBlock returns the current best block height and hash from lnd's view of
// the blockchain. We register for a single block notification to get the
// current tip.
func (b *LNDBackend) BestBlock(ctx context.Context) (int32, chainhash.Hash,
	error) {

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

		return epoch.Height, *epoch.Hash, nil

	case <-ctx.Done():
		return 0, chainhash.Hash{}, ctx.Err()
	}
}

// TestMempoolAccept tests whether a transaction would be accepted by the
// mempool. Note that this is not directly supported by lnd's interfaces, so we
// return an error indicating the operation is not supported.
func (b *LNDBackend) TestMempoolAccept(ctx context.Context,
	tx *wire.MsgTx) (bool, string, error) {

	// LND's WalletController doesn't provide a test mempool accept
	// interface. This would require direct RPC access to the underlying
	// Bitcoin node.
	return false, "", fmt.Errorf("test mempool accept not supported by " +
		"LND backend")
}

// BroadcastTx broadcasts a transaction to the network using the configured
// broadcaster.
func (b *LNDBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	err := b.broadcaster.PublishTransaction(ctx, tx, label)
	if err != nil {
		return fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	return nil
}

// SubmitPackage atomically submits a parent+child transaction package via
// the optional PackageSubmitter. Returns an error if package submission is
// not supported (packageSubmitter is nil) or if the package is rejected.
func (b *LNDBackend) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx) error {

	if b.packageSubmitter == nil {
		return fmt.Errorf("package submission not supported " +
			"by this backend")
	}

	result, err := b.packageSubmitter.SubmitPackage(
		parents, child, nil,
	)
	if err != nil {
		return fmt.Errorf("submit package RPC: %w", err)
	}

	// Collect per-transaction errors for diagnostics.
	var txErrors []string
	for wtxid, txResult := range result.TxResults {
		if txResult.Error != nil {
			txErrors = append(txErrors,
				fmt.Sprintf("%s: %s", wtxid,
					*txResult.Error))
		}
	}

	// Check the overall package result.
	if result.PackageMsg != "success" {
		if len(txErrors) > 0 {
			return fmt.Errorf("package not accepted: "+
				"%s (tx errors: %v)",
				result.PackageMsg, txErrors)
		}

		return fmt.Errorf("package not accepted: %s",
			result.PackageMsg)
	}

	// Even with a successful package, individual transactions
	// may have been rejected.
	if len(txErrors) > 0 {
		return fmt.Errorf("tx rejected: %v", txErrors)
	}

	return nil
}

// RegisterConf registers for confirmation notifications using lnd's chain
// notifier. The registration returns a ConfRegistration with channels for
// receiving confirmation events.
func (b *LNDBackend) RegisterConf(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs uint32,
	heightHint uint32,
	includeBlock bool) (*chainsource.ConfRegistration, error) {

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

	// Create a channel to convert lnd's TxConfirmation to our type.
	confChan := make(chan *chainsource.TxConfirmation, 1)

	go func() {
		defer close(confChan)
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

		case <-ctx.Done():
			return
		}
	}()

	return &chainsource.ConfRegistration{
		Confirmed: confChan,
		Cancel:    event.Cancel,
	}, nil
}

// RegisterSpend registers for spend notifications using lnd's chain notifier.
// The registration returns a SpendRegistration with channels for receiving
// spend events.
func (b *LNDBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*chainsource.SpendRegistration, error) {

	// Register with lnd's notifier.
	event, err := b.notifier.RegisterSpendNtfn(
		outpoint, pkScript, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register spend: %w", err)
	}

	// Create a channel to convert lnd's SpendDetail to our type.
	spendChan := make(chan *chainsource.SpendDetail, 1)

	// Start a goroutine to convert and forward the spend.
	go func() {
		defer close(spendChan)
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

		case <-ctx.Done():
			return
		}
	}()

	return &chainsource.SpendRegistration{
		Spend:  spendChan,
		Cancel: event.Cancel,
	}, nil
}

// RegisterBlocks registers for new block notifications using lnd's chain
// notifier. The registration returns a BlockRegistration with a channel for
// receiving block events.
func (b *LNDBackend) RegisterBlocks(
	ctx context.Context) (*chainsource.BlockRegistration, error) {

	// Register with lnd's notifier. Pass nil for the best known block to
	// get the current tip immediately.
	event, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to register for blocks: %w",
			err)
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

	return nil
}

// Stop shuts down the LND backend by stopping the notifier and fee estimator.
func (b *LNDBackend) Stop() error {
	var errs []error

	// Stop the notifier.
	if err := b.notifier.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("failed to stop notifier: %w",
			err))
	}

	// Stop the fee estimator.
	if err := b.feeEstimator.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("failed to stop fee "+
			"estimator: %w", err))
	}

	return errors.Join(errs...)
}

// Ensure LNDBackend implements ChainBackend at compile time.
var _ chainsource.ChainBackend = (*LNDBackend)(nil)
