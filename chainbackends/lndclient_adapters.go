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
	// Confirmed event and forward any subsequent reorg signal (with depth)
	// and the terminal Done signal on dedicated channels. Without these the
	// receive loop tears the stream down after one delivery and any later
	// reorg is silently dropped at the gRPC layer. WithReOrgDepthChan
	// carries both the reorg signal and its depth, and WithDoneChan
	// delivers lnd's "past reorg-safety depth" signal that this transport
	// previously could not surface.
	reorgDepth := make(chan int32, 1)
	doneChan := make(chan struct{}, 1)

	lndOpts := []lndclient.NotifierOption{
		lndclient.WithReOrgDepthChan(reorgDepth),
		lndclient.WithDoneChan(doneChan),
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
	// coming, and strand the watch. Draining a pending reorg with priority
	// on each iteration, and handing every event off with a blocking send,
	// makes the ConfActor observe exactly the forwarder's (lndclient's)
	// order.
	//
	// lndclient now forwards the real reorg depth on NegativeConf.
	orderedConfirmed := make(chan *chainntnfs.TxConfirmation, 1)
	negativeConf := make(chan int32, 1)
	go func() {
		defer close(orderedConfirmed)
		defer close(negativeConf)

		forwardOrderedReorg(
			ctx, reorgDepth, confChan, orderedConfirmed,
			negativeConf,
		)
	}()

	// Done is now driven by lnd's "past reorg-safety depth" signal, which
	// lndclient surfaces via WithDoneChan. It is a terminal, post-finality
	// signal so it is forwarded directly rather than through the ordered
	// confirmation forwarder.
	return &chainntnfs.ConfirmationEvent{
		Confirmed:    orderedConfirmed,
		NegativeConf: negativeConf,
		Cancel:       cancel,
		Done:         doneChan,
	}, nil
}

// forwardOrderedReorg copies confirmations and reorg pings from lndclient's two
// source channels onto the downstream Confirmed / NegativeConf channels in
// lndclient's emission order. A pending reorg is drained with priority before
// any confirmation on each iteration, and every forwarded event is handed off
// with a blocking send, so the single downstream consumer observes the events
// strictly in order (it never has two of our channels ready at once). This
// preserves the reorg-before-reconfirmation ordering lndclient guarantees from
// its single ordered receive loop, which the two-channel split would otherwise
// lose at the consumer's select.
func forwardOrderedReorg(ctx context.Context, reorgDepth <-chan int32,
	confChan <-chan *chainntnfs.TxConfirmation,
	outConfirmed chan<- *chainntnfs.TxConfirmation,
	outNegConf chan<- int32) {

	for confChan != nil || reorgDepth != nil {
		// Priority: forward a reorg that is already pending before
		// looking at confirmations, so a reorg lndclient wrote before
		// the replacement Confirmed is delivered first.
		if reorgDepth != nil {
			select {
			case depth, ok := <-reorgDepth:
				if !ok {
					reorgDepth = nil

					continue
				}

				select {
				case outNegConf <- depth:
				case <-ctx.Done():
					return
				}

				continue

			default:
			}
		}

		select {
		case depth, ok := <-reorgDepth:
			if !ok {
				reorgDepth = nil

				continue
			}

			select {
			case outNegConf <- depth:
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
	// Spend event so it can forward reorg pings and the terminal Done
	// signal. Without these the stream is torn down after the first
	// delivery and any later spend reorg is silently dropped at the gRPC
	// layer. The spend notifier does not track a reorg depth, so the bare
	// WithReOrgChan signal is sufficient on this path.
	reorgPing := make(chan struct{}, 1)
	doneChan := make(chan struct{}, 1)

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
			lndclient.WithDoneChan(doneChan),
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

		case <-ctx.Done():
		}
	}()

	// Forward the spend lifecycle through a SINGLE goroutine so the reorg
	// ping and the (re-)spend reach the downstream chainntnfs channels in
	// lndclient's emission order, for the same reason as the conf path: the
	// two-channel split (spendChan and reorgPing) would otherwise let the
	// downstream SpendActor's select consume a re-Spend before the Reorged
	// and strand the watch. Draining a pending reorg with priority and
	// using blocking hand-offs makes the SpendActor observe lndclient's
	// order.
	orderedSpend := make(chan *chainntnfs.SpendDetail, 1)
	reorgChan := make(chan struct{}, 1)
	go func() {
		defer close(orderedSpend)
		defer close(reorgChan)

		forwardOrderedSpendReorg(
			ctx, reorgPing, spendChan, orderedSpend, reorgChan,
		)
	}()

	// Done is now driven by lnd's "past reorg-safety depth" signal, which
	// lndclient surfaces via WithDoneChan. It is a terminal, post-finality
	// signal so it is forwarded directly rather than through the ordered
	// spend forwarder.
	return &chainntnfs.SpendEvent{
		Spend:  orderedSpend,
		Reorg:  reorgChan,
		Done:   doneChan,
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
