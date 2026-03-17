package chainbackends

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// LndClientTxBroadcaster implements TxBroadcaster using
// lndclient.WalletKitClient.
type LndClientTxBroadcaster struct {
	walletKit lndclient.WalletKitClient
}

// NewLndClientTxBroadcaster creates a new broadcaster backed by lndclient.
func NewLndClientTxBroadcaster(
	walletKit lndclient.WalletKitClient) *LndClientTxBroadcaster {

	return &LndClientTxBroadcaster{
		walletKit: walletKit,
	}
}

// PublishTransaction broadcasts the given transaction to the network via lnd.
func (b *LndClientTxBroadcaster) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, label string) error {

	return b.walletKit.PublishTransaction(ctx, tx, label)
}

// Ensure LndClientTxBroadcaster implements TxBroadcaster.
var _ TxBroadcaster = (*LndClientTxBroadcaster)(nil)

// LndClientFeeEstimator implements chainfee.Estimator using lndclient.
type LndClientFeeEstimator struct {
	walletKit lndclient.WalletKitClient
}

// NewLndClientFeeEstimator creates a new fee estimator backed by lndclient.
func NewLndClientFeeEstimator(
	walletKit lndclient.WalletKitClient) *LndClientFeeEstimator {

	return &LndClientFeeEstimator{
		walletKit: walletKit,
	}
}

// Start is a no-op for lndclient-backed estimators since the connection is
// already established.
func (e *LndClientFeeEstimator) Start() error {
	return nil
}

// Stop is a no-op for lndclient-backed estimators.
func (e *LndClientFeeEstimator) Stop() error {
	return nil
}

// EstimateFeePerKW returns the estimated fee rate in satoshis per kilo-weight
// unit for the given confirmation target.
func (e *LndClientFeeEstimator) EstimateFeePerKW(
	numBlocks uint32) (chainfee.SatPerKWeight, error) {

	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()
	satPerVByte, err := e.walletKit.EstimateFeeRate(
		ctx, int32(numBlocks),
	)
	if err != nil {
		return 0, fmt.Errorf("estimate fee rate: %w", err)
	}

	satPerKw := chainfee.SatPerVByte(satPerVByte).FeePerKWeight()

	return satPerKw, nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed. For remote lnd, we return a reasonable default.
func (e *LndClientFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	// Default relay fee of 1 sat/vbyte = 250 sat/kw.
	return chainfee.SatPerKWeight(250)
}

// Ensure LndClientFeeEstimator implements chainfee.Estimator.
var _ chainfee.Estimator = (*LndClientFeeEstimator)(nil)

// LndClientChainNotifierConfig holds configuration for LndClientChainNotifier.
type LndClientChainNotifierConfig struct {
	// LND is the lndclient services connection.
	LND *lndclient.LndServices

	// Log is an optional logger for this notifier. If None, the notifier
	// falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]
}

// WithLogger returns a new config with the given logger set.
func (c LndClientChainNotifierConfig) WithLogger(
	log btclog.Logger) LndClientChainNotifierConfig {

	c.Log = fn.Some(log)

	return c
}

// LndClientChainNotifier implements chainntnfs.ChainNotifier using lndclient.
// This adapter bridges the lndclient.ChainNotifierClient interface to the
// chainntnfs.ChainNotifier interface expected by LNDBackend.
type LndClientChainNotifier struct {
	// cfg holds all notifier configuration including lnd services and
	// optional logger.
	cfg LndClientChainNotifierConfig
}

// NewLndClientChainNotifier creates a new chain notifier backed by lndclient.
// The config must include LND; use WithLogger() to inject a specific logger.
func NewLndClientChainNotifier(
	cfg LndClientChainNotifierConfig) *LndClientChainNotifier {

	return &LndClientChainNotifier{
		cfg: cfg,
	}
}

// logger returns the configured logger, falling back to the context logger
// and then to the package-level lndClientLog registered under the LNDC
// subsystem.
func (n *LndClientChainNotifier) logger(ctx context.Context) btclog.Logger {
	return n.cfg.Log.UnwrapOr(lndClientLog)
}

// Start is a no-op for lndclient-backed notifiers since the connection is
// already established.
func (n *LndClientChainNotifier) Start() error {
	return nil
}

// Started returns true since lndclient connections are always ready.
func (n *LndClientChainNotifier) Started() bool {
	return true
}

// Stop is a no-op for lndclient-backed notifiers.
func (n *LndClientChainNotifier) Stop() error {
	return nil
}

// RegisterConfirmationsNtfn registers for confirmation notifications using
// lndclient's ChainNotifier.
func (n *LndClientChainNotifier) RegisterConfirmationsNtfn(
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	notifierOpts := chainntnfs.DefaultNotifierOptions()
	for _, opt := range opts {
		opt(notifierOpts)
	}

	var lndOpts []lndclient.NotifierOption
	if notifierOpts.IncludeBlock {
		lndOpts = append(lndOpts, lndclient.WithIncludeBlock())
	}

	ctx, cancel := context.WithCancel(context.Background())
	chainNotifier := n.cfg.LND.ChainNotifier

	n.logger(ctx).InfoS(ctx, "Calling lndclient RegisterConfirmationsNtfn",
		slog.Int("pkscript_len", len(pkScript)),
		slog.Int("num_confs", int(numConfs)),
		slog.Int("height_hint", int(heightHint)))

	confChan, errChan, err := chainNotifier.RegisterConfirmationsNtfn(
		ctx, txid, pkScript, int32(numConfs), int32(heightHint),
		lndOpts...,
	)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("register confirmations: %w", err)
	}

	go func() {
		select {
		case err, ok := <-errChan:
			if ok && err != nil {
				n.logger(ctx).WarnS(
					ctx, "Conf notification error", err,
				)
			}

		case <-ctx.Done():
		}
	}()

	return &chainntnfs.ConfirmationEvent{
		Confirmed: confChan,
		Cancel:    cancel,
		Done:      make(chan struct{}, 1),
	}, nil
}

// RegisterSpendNtfn registers for spend notifications using lndclient's
// ChainNotifier.
func (n *LndClientChainNotifier) RegisterSpendNtfn(
	outpoint *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	ctx, cancel := context.WithCancel(context.Background())
	spendChan, errChan, err := n.cfg.LND.ChainNotifier.RegisterSpendNtfn(
		ctx, outpoint, pkScript, int32(heightHint),
	)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("register spend: %w", err)
	}

	go func() {
		select {
		case err, ok := <-errChan:
			if ok && err != nil {
				n.logger(ctx).WarnS(
					ctx, "Spend notification error", err,
				)
			}

		case <-ctx.Done():
		}
	}()

	return &chainntnfs.SpendEvent{
		Spend:  spendChan,
		Reorg:  make(chan struct{}, 1),
		Done:   make(chan struct{}, 1),
		Cancel: cancel,
	}, nil
}

// RegisterBlockEpochNtfn registers for block epoch notifications using
// lndclient's ChainNotifier.
func (n *LndClientChainNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	ctx, cancel := context.WithCancel(context.Background())
	chainNotifier := n.cfg.LND.ChainNotifier
	heightChan, errChan, err := chainNotifier.RegisterBlockEpochNtfn(ctx)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("register block epoch: %w", err)
	}

	log := n.logger(ctx)
	log.InfoS(ctx, "Registered for block epoch notifications")

	// Convert height-only channel to full BlockEpoch channel.
	epochChan := make(chan *chainntnfs.BlockEpoch, 10)

	go func() {
		defer close(epochChan)
		defer log.InfoS(ctx, "Block epoch forwarder goroutine exiting")

		for {
			select {
			case height, ok := <-heightChan:
				if !ok {
					log.InfoS(ctx, "Height channel closed")
					return
				}

				log.InfoS(ctx, "Received height from lndclient",
					slog.Int("height", int(height)))

				// lndclient only provides height, so we need to
				// fetch the hash via ChainKit.
				//
				// TODO(rosabeef): why doesn't lndclient also
				// give the block height?
				chainKit := n.cfg.LND.ChainKit
				blockHash, err := chainKit.GetBlockHash(
					ctx, int64(height),
				)
				if err != nil {
					log.WarnS(ctx, "Failed to get block hash",
						err, slog.Int("height", int(height)))
					continue
				}

				epoch := &chainntnfs.BlockEpoch{
					Height: height,
					Hash:   &blockHash,
				}

				select {
				case epochChan <- epoch:
				case <-ctx.Done():
					log.InfoS(ctx, "Context cancelled while "+
						"sending epoch")
					return
				}

			case err, ok := <-errChan:
				if ok && err != nil {
					log.WarnS(ctx, "Block epoch error", err)
				}

				return

			case <-ctx.Done():
				log.InfoS(ctx, "Context cancelled")
				return
			}
		}
	}()

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: cancel,
	}, nil
}

// Ensure LndClientChainNotifier implements chainntnfs.ChainNotifier.
var _ chainntnfs.ChainNotifier = (*LndClientChainNotifier)(nil)

// LNDBackendFromLndClientConfig holds configuration for creating an LNDBackend
// from lndclient services.
type LNDBackendFromLndClientConfig struct {
	// LND is the lndclient services connection.
	LND *lndclient.LndServices

	// Log is an optional logger for the backend components. If None, the
	// backend falls back to extracting a logger from context or uses
	// btclog.Disabled.
	Log fn.Option[btclog.Logger]

	// PackageSubmitter is an optional package submitter for atomic
	// parent+child package submission. When nil, SubmitPackage will
	// return an "unsupported" error.
	PackageSubmitter PackageSubmitter
}

// WithLogger returns a new config with the given logger set.
func (c LNDBackendFromLndClientConfig) WithLogger(
	log btclog.Logger) LNDBackendFromLndClientConfig {

	c.Log = fn.Some(log)
	return c
}

// NewLNDBackendFromLndClient creates a new LNDBackend using lndclient
// services. This is a convenience function for creating a backend from a
// remote lnd connection. The config must include LND; use WithLogger() to
// inject a specific logger.
func NewLNDBackendFromLndClient(cfg LNDBackendFromLndClientConfig) *LNDBackend {
	log.InfoS(context.TODO(),
		"Creating LND backend from lndclient services")

	// Use explicit struct initialization instead of type cast for safety -
	// this ensures we don't silently miss fields if the types diverge.
	notifier := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: cfg.LND,
		Log: cfg.Log,
	})
	feeEstimator := NewLndClientFeeEstimator(cfg.LND.WalletKit)
	broadcaster := NewLndClientTxBroadcaster(cfg.LND.WalletKit)

	backend := NewLNDBackend(notifier, feeEstimator, broadcaster)
	backend.Log = cfg.Log
	backend.packageSubmitter = cfg.PackageSubmitter

	return backend
}
