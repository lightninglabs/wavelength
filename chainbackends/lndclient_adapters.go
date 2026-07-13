package chainbackends

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/chainfees"
	"github.com/lightningnetwork/lnd/chainntnfs"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// lndRegistrationTimeout bounds how long a conf/spend registration call into
// lndclient may block before we give up and return an error. lnd can be slow
// to answer a RegisterConfirmationsNtfn / RegisterSpendNtfn while it is busy
// processing a fresh block, and a registration that hangs forever would pin
// the chainsource sub-actor's Receive call and back-pressure the factory
// actor onto every other in-flight registration. Fifteen seconds is well
// above lnd's normal under-load response time yet short enough that a
// genuinely wedged backend surfaces as an error rather than a silent hang.
const lndRegistrationTimeout = 15 * time.Second

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

	// Ask lndclient to keep the confirmation stream alive past the first
	// Confirmed event and forward any subsequent reorg signal on a
	// dedicated channel. Without WithReOrgChan, lndclient's receive
	// loop tears the stream down after one delivery and any later
	// reorg is silently dropped at the gRPC layer.
	reorgPing := make(chan struct{}, 1)

	lndOpts := []lndclient.NotifierOption{
		lndclient.WithReOrgChan(reorgPing),
	}
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

	case <-time.After(lndRegistrationTimeout):
		cancel()

		return nil, fmt.Errorf("register confirmations timed out "+
			"after %s", lndRegistrationTimeout)
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
			cancel()

		case <-ctx.Done():
		}
	}()

	// Forward the confirmation lifecycle through a SINGLE goroutine so the
	// reorg ping and the (re-)confirmation reach the downstream chainntnfs
	// channels in lndclient's emission order. lndclient drives both off one
	// ordered gRPC receive loop and writes the reorg ping before the
	// replacement Confirmed, but it splits them across two channels
	// (confChan and reorgPing); if we forwarded each on its own goroutine,
	// the downstream ConfActor's select could consume a re-Confirmed before
	// the Reorged, reset confirmHeight to 0 with no further Confirmed
	// coming, and strand the watch. A single forwarder with blocking
	// hand-offs gives each observed event one shared sequence downstream;
	// the consumer then applies highest-sequence-wins across channels.
	//
	// lndclient does not preserve the reorg depth across the gRPC boundary,
	// so a sentinel value of 0 is forwarded on NegativeConf; callers over
	// this transport must not rely on the integer value.
	orderedConfirmed := make(chan *chainntnfs.TxConfirmation, 1)
	negativeConf := make(chan int32, 1)
	go func() {
		defer close(orderedConfirmed)
		defer close(negativeConf)

		forwardOrderedReorg(
			ctx, reorgPing, confChan, orderedConfirmed,
			negativeConf,
		)
	}()

	// Done is allocated but never written to because lnd's internal
	// "past reorg-safety depth" signal is not surfaced through the
	// lndclient gRPC transport. Consumers needing such a gate must
	// compute it themselves from block height.
	return &chainntnfs.ConfirmationEvent{
		Confirmed:    orderedConfirmed,
		NegativeConf: negativeConf,
		Cancel:       cancel,
		Done:         make(chan struct{}, 1),
	}, nil
}

// forwardOrderedReorg copies confirmations and reorg pings from lndclient's two
// source channels onto the downstream Confirmed / NegativeConf channels,
// forwarding each event in the order this single goroutine observes it. It does
// not bias either channel: the authoritative lifecycle ordering is
// re-established downstream by the per-registration sequence number the
// LNDBackend forwarder stamps (see chainsource.TxConfirmation.Seq), so the
// consumer applies highest-seq-wins regardless of how near-simultaneous events
// interleave here. lndclient's two-channel split makes a perfectly ordered
// merge impossible at this layer, so forwarding in natural arrival order keeps
// the stamped sequence faithful to what was actually observed rather than
// injecting an artificial reorg-first bias. lndclient does not preserve the
// reorg depth across the gRPC boundary, so a sentinel value of 0 is forwarded
// on NegativeConf; callers over this transport must not rely on the value.
func forwardOrderedReorg(ctx context.Context, reorgPing <-chan struct{},
	confChan <-chan *chainntnfs.TxConfirmation,
	outConfirmed chan<- *chainntnfs.TxConfirmation,
	outNegConf chan<- int32) {

	for confChan != nil || reorgPing != nil {
		select {
		case _, ok := <-reorgPing:
			if !ok {
				reorgPing = nil

				continue
			}

			select {
			case outNegConf <- 0:
			case <-ctx.Done():
				return
			}

		case c, ok := <-confChan:
			if !ok {
				confChan = nil

				continue
			}

			select {
			case outConfirmed <- c:
			case <-ctx.Done():
				return
			}

		case <-ctx.Done():
			return
		}
	}
}

// RegisterSpendNtfn registers for spend notifications using lndclient's
// ChainNotifier.
func (n *LndClientChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	// Ask lndclient to keep the spend stream alive past the first
	// Spend event so it can forward reorg pings. Without WithReOrgChan
	// the stream is torn down after the first delivery and any later
	// spend reorg is silently dropped at the gRPC layer.
	reorgPing := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())

	// Run the registration in a goroutine with a timeout to prevent
	// hanging when LND is slow under block load, mirroring the conf path.
	type regResult struct {
		spendChan chan *chainntnfs.SpendDetail
		errChan   chan error
		err       error
	}

	resultCh := make(chan regResult, 1)
	go func() {
		sc, ec, err := n.cfg.LND.ChainNotifier.RegisterSpendNtfn(
			ctx, outpoint, pkScript, int32(heightHint),
			lndclient.WithReOrgChan(reorgPing),
		)
		resultCh <- regResult{sc, ec, err}
	}()

	var spendChan chan *chainntnfs.SpendDetail
	var errChan chan error

	select {
	case r := <-resultCh:
		if r.err != nil {
			cancel()

			return nil, fmt.Errorf("register spend: %w", r.err)
		}

		spendChan = r.spendChan
		errChan = r.errChan

	case <-time.After(lndRegistrationTimeout):
		cancel()

		return nil, fmt.Errorf("register spend timed out after %s",
			lndRegistrationTimeout)
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
			cancel()

		case <-ctx.Done():
		}
	}()

	// Forward the spend lifecycle through a SINGLE goroutine so the reorg
	// ping and the (re-)spend reach the downstream chainntnfs channels in
	// lndclient's emission order, for the same reason as the conf path: the
	// two-channel split (spendChan and reorgPing) would otherwise let the
	// downstream SpendActor's select consume a re-Spend before the Reorged
	// and strand the watch. A single forwarder with blocking hand-offs
	// gives each observed event one shared sequence downstream; the
	// consumer then applies highest-sequence-wins across channels.
	orderedSpend := make(chan *chainntnfs.SpendDetail, 1)
	reorgChan := make(chan struct{}, 1)
	go func() {
		defer close(orderedSpend)
		defer close(reorgChan)

		forwardOrderedSpendReorg(
			ctx, reorgPing, spendChan, orderedSpend, reorgChan,
		)
	}()

	// Done is allocated but never written to because lnd's "past
	// reorg-safety depth" signal is not surfaced through the lndclient
	// gRPC transport.
	return &chainntnfs.SpendEvent{
		Spend:  orderedSpend,
		Reorg:  reorgChan,
		Done:   make(chan struct{}, 1),
		Cancel: cancel,
	}, nil
}

// forwardOrderedSpendReorg is the spend-path analogue of forwardOrderedReorg:
// it copies spends and reorg pings from lndclient's two source channels onto
// the downstream Spend / Reorg channels in the order this single goroutine
// observes them, without biasing either channel. The authoritative lifecycle
// ordering is re-established downstream by the per-registration sequence number
// the LNDBackend forwarder stamps (see chainsource.SpendDetail.Seq).
func forwardOrderedSpendReorg(ctx context.Context, reorgPing <-chan struct{},
	spendChan <-chan *chainntnfs.SpendDetail,
	outSpend chan<- *chainntnfs.SpendDetail, outReorg chan<- struct{}) {

	for spendChan != nil || reorgPing != nil {
		select {
		case _, ok := <-reorgPing:
			if !ok {
				reorgPing = nil

				continue
			}

			select {
			case outReorg <- struct{}{}:
			case <-ctx.Done():
				return
			}

		case sp, ok := <-spendChan:
			if !ok {
				spendChan = nil

				continue
			}

			select {
			case outSpend <- sp:
			case <-ctx.Done():
				return
			}

		case <-ctx.Done():
			return
		}
	}
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
