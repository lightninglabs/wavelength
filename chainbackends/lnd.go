package chainbackends

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/chainsource"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

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

	// wallet provides transaction broadcasting capabilities.
	wallet lnwallet.WalletController
}

// NewLNDBackend creates a new LNDBackend instance with the given lnd
// components. All parameters must be non-nil.
func NewLNDBackend(notifier chainntnfs.ChainNotifier,
	feeEstimator chainfee.Estimator,
	wallet lnwallet.WalletController) *LNDBackend {

	return &LNDBackend{
		notifier:     notifier,
		feeEstimator: feeEstimator,
		wallet:       wallet,
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
	//   - Therefore: sat/vbyte = (sat/kw) * (1 kw / 1000 wu) * (4 wu / 1 vb)
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
			return 0, chainhash.Hash{}, fmt.Errorf("block epoch "+
				"channel closed")
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

// BroadcastTx broadcasts a transaction to the network using lnd's wallet
// controller.
func (b *LNDBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	err := b.wallet.PublishTransaction(tx, label)
	if err != nil {
		return fmt.Errorf("failed to broadcast transaction: %w", err)
	}

	return nil
}

// RegisterConf registers for confirmation notifications using lnd's chain
// notifier. The registration returns a ConfRegistration with channels for
// receiving confirmation events.
func (b *LNDBackend) RegisterConf(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs uint32,
	heightHint uint32) (*chainsource.ConfRegistration, error) {

	// Register with lnd's notifier.
	event, err := b.notifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to register confirmation: %w",
			err)
	}

	// Create a channel to convert lnd's TxConfirmation to our type.
	confChan := make(chan *chainsource.TxConfirmation, 1)

	// Start a goroutine to convert and forward the confirmation.
	go func() {
		defer close(confChan)

		select {
		case lndConf, ok := <-event.Confirmed:
			if !ok {
				return
			}

			// Convert to our type.
			conf := &chainsource.TxConfirmation{
				BlockHash:   lndConf.BlockHash,
				BlockHeight: lndConf.BlockHeight,
				TxIndex:     lndConf.TxIndex,
				Tx:          lndConf.Tx,
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

	return &chainsource.BlockRegistration{
		Epochs: event.Epochs,
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
	// Stop the notifier.
	if err := b.notifier.Stop(); err != nil {
		return fmt.Errorf("failed to stop notifier: %w", err)
	}

	// Stop the fee estimator.
	if err := b.feeEstimator.Stop(); err != nil {
		return fmt.Errorf("failed to stop fee estimator: %w", err)
	}

	return nil
}

// Ensure LNDBackend implements ChainBackend at compile time.
var _ chainsource.ChainBackend = (*LNDBackend)(nil)
