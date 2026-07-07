package chainbackends

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainfees"
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
type LndClientFeeEstimator = chainfees.WalletKitEstimator

// NewLndClientFeeEstimator creates a new fee estimator backed by lndclient.
// It uses last-good fallback semantics: a WalletKit error returns the last
// successful rate (or the relay floor before any success) rather than
// propagating the error, so a transient WalletKit outage does not abort fee
// estimation on the standalone LND backend path.
func NewLndClientFeeEstimator(walletKit lndclient.WalletKitClient) (
	*LndClientFeeEstimator, error) {

	return chainfees.NewFallbackWalletKitEstimator(walletKit, nil)
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

// logger returns the configured logger, falling back to the context logger.
func (n *LndClientChainNotifier) logger(ctx context.Context) btclog.Logger {
	return n.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
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
func (n *LndClientChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
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
		slog.Int("height_hint", int(heightHint)),
	)

	// Run the registration in a goroutine with a timeout to
	// prevent hanging when LND is slow under block load.
	type regResult struct {
		confChan chan *chainntnfs.TxConfirmation
		errChan  chan error
		err      error
	}

	resultCh := make(chan regResult, 1)
	go func() {
		cc, ec, err := chainNotifier.RegisterConfirmationsNtfn(
			ctx, txid, pkScript, int32(numConfs), int32(heightHint),
			lndOpts...,
		)
		resultCh <- regResult{cc, ec, err}
	}()

	var confChan chan *chainntnfs.TxConfirmation
	var errChan chan error

	select {
	case r := <-resultCh:
		if r.err != nil {
			cancel()

			return nil, fmt.Errorf("register confirmations: %w",
				r.err)
		}

		confChan = r.confChan
		errChan = r.errChan

	case <-time.After(15 * time.Second):
		cancel()

		return nil, fmt.Errorf("register confirmations timed out " +
			"after 15s")
	}

	go func() {
		select {
		case err, ok := <-errChan:
			if ok && err != nil {
				n.logger(ctx).WarnS(
					ctx,
					"Conf notification error",
					err,
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
func (n *LndClientChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

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
					ctx,
					"Spend notification error",
					err,
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
					slog.Int("height", int(height)),
				)

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
					log.WarnS(
						ctx,
						"Failed to get block hash",
						err,
						slog.Int("height", int(height)),
					)

					continue
				}

				epoch := &chainntnfs.BlockEpoch{
					Height: height,
					Hash:   &blockHash,
				}

				select {
				case epochChan <- epoch:
				case <-ctx.Done():
					log.InfoS(
						ctx, "Context cancelled "+
							"while sending epoch",
					)

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
	// parent+child package submission. When nil, SubmitPackage
	// returns an "unsupported" error. Typically backed by a direct
	// bitcoind RPC client.
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
func NewLNDBackendFromLndClient(cfg LNDBackendFromLndClientConfig) (*LNDBackend,
	error) {

	cfg.Log.UnwrapOr(btclog.Disabled).InfoS(
		context.Background(),
		"Creating LND backend from lndclient services",
	)

	// Use explicit struct initialization instead of type cast for safety -
	// this ensures we don't silently miss fields if the types diverge.
	notifier := NewLndClientChainNotifier(LndClientChainNotifierConfig{
		LND: cfg.LND,
		Log: cfg.Log,
	})
	feeEstimator, err := NewLndClientFeeEstimator(cfg.LND.WalletKit)
	if err != nil {
		return nil, fmt.Errorf("create fee estimator: %w", err)
	}
	broadcaster := NewLndClientTxBroadcaster(cfg.LND.WalletKit)

	backend := NewLNDBackend(notifier, feeEstimator, broadcaster)
	backend.Log = cfg.Log
	backend.packageSubmitter = cfg.PackageSubmitter

	return backend, nil
}
