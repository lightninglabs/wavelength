package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/contractcourt"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	defaultCommitInterval        = 50 * time.Millisecond
	defaultCommitBatchSize       = uint32(10)
	defaultOutgoingRejectDelta   = uint32(3)
	defaultQuiescenceTimeout     = time.Minute
	forceCloseResultPollInterval = 100 * time.Millisecond
)

// durablePeerPendingCommitTicker disables lnd's socket-liveness timeout for a
// peer whose ordered messages remain durably queued while either endpoint is
// unavailable. HTLC deadlines remain enforced by lnd's contract lifecycle.
type durablePeerPendingCommitTicker struct{}

// Ticks returns a permanently parked channel, as permitted by ticker.Ticker.
func (durablePeerPendingCommitTicker) Ticks() <-chan time.Time {
	return nil
}

// Resume leaves the durable peer's disconnect timer parked.
func (durablePeerPendingCommitTicker) Resume() {}

// Pause leaves the durable peer's disconnect timer parked.
func (durablePeerPendingCommitTicker) Pause() {}

// Stop has no resources to release.
func (durablePeerPendingCommitTicker) Stop() {}

// RuntimeConfig contains the shared dependencies for lnd's native channel and
// payment subsystems. The caller continues to own the chain notifier and DB.
type RuntimeConfig struct {
	DB            *channeldb.DB
	Chain         lnwallet.BlockChainIO
	Notifier      chainntnfs.ChainNotifier
	OnionKey      sphinx.SingleKeyECDH
	Signer        input.Signer
	FeeEstimator  chainfee.Estimator
	WitnessBeacon contractcourt.WitnessBeacon
	SelfNode      route.Vertex
	Clock         clock.Clock
	Funding       *FundingConfig
	Onchain       *OnchainConfig

	LocalChannelClose      func([]byte, *htlcswitch.ChanClose)
	FetchLastChannelUpdate func(lnwire.ShortChannelID) (
		*lnwire.ChannelUpdate1, error)
	SignAliasUpdate func(*lnwire.ChannelUpdate1) (*ecdsa.Signature, error)
	IsAlias         func(lnwire.ShortChannelID) bool
}

// Runtime owns the lifecycle and wiring of native lnd subsystems without an
// lnd server, RPC surface, graph, or network peer manager.
type Runtime struct {
	cfg RuntimeConfig

	onionProcessor *hop.OnionProcessor
	htlcNotifier   *htlcswitch.HtlcNotifier
	invoices       *invoices.InvoiceRegistry
	switcher       *htlcswitch.Switch
	interceptor    *htlcswitch.InterceptableSwitch
	payments       *FixedRoutePayments
	funding        *FundingRuntime
	onchain        *OnchainRuntime
	sigPool        *lnwallet.SigPool

	mu      sync.Mutex
	started bool
	stopped bool

	reestablishMu       sync.Mutex
	awaitingReestablish map[lnwire.ChannelID]struct{}

	forceCloseMu    sync.Mutex
	forceCloseCalls map[wire.OutPoint]*forceCloseCall
}

// forceCloseCall owns one in-process force-close result. LND's force-close
// request can block while Ark materializes the channel point, so duplicate RPC
// delivery must join the existing request instead of entering the channel
// arbitrator a second time.
type forceCloseCall struct {
	done chan struct{}
	tx   *wire.MsgTx
	err  error
}

// NewRuntime composes lnd's existing invoice, switch, link-signing, and
// fixed-route payment components.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := validateRuntimeConfig(cfg); err != nil {
		return nil, err
	}

	runtimeClock := cfg.Clock
	if runtimeClock == nil {
		runtimeClock = clock.NewDefaultClock()
	}
	cfg.Clock = runtimeClock

	blockHash, blockHeight, err := cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("read channel runtime chain tip: %w",
			err)
	}

	expiryWatcher := invoices.NewInvoiceExpiryWatcher(
		runtimeClock, defaultOutgoingRejectDelta, uint32(blockHeight),
		blockHash, cfg.Notifier,
	)
	invoiceRegistry := invoices.NewRegistry(
		cfg.DB, expiryWatcher, &invoices.RegistryConfig{
			FinalCltvRejectDelta: int32(defaultOutgoingRejectDelta),
			HtlcHoldDuration:     invoices.DefaultHtlcHoldDuration,
			Clock:                runtimeClock,
			HtlcInterceptor: invoices.
				NewHtlcModificationInterceptor(),
		},
	)

	replayLog := htlcswitch.NewDecayedLog(cfg.DB, cfg.Notifier)
	onionRouter := sphinx.NewRouter(cfg.OnionKey, replayLog)
	onionProcessor := hop.NewOnionProcessor(onionRouter)
	htlcNotifier := htlcswitch.NewHtlcNotifier(runtimeClock.Now)

	stateDB := cfg.DB.ChannelStateDB()
	localChannelClose := cfg.LocalChannelClose
	if localChannelClose == nil {
		localChannelClose = func([]byte, *htlcswitch.ChanClose) {}
	}
	fetchLastUpdate := cfg.FetchLastChannelUpdate
	if fetchLastUpdate == nil {
		fetchLastUpdate = unavailableChannelUpdate
	}
	cfg.FetchLastChannelUpdate = fetchLastUpdate
	signAliasUpdate := cfg.SignAliasUpdate
	if signAliasUpdate == nil {
		signAliasUpdate = unavailableAliasSignature
	}
	cfg.SignAliasUpdate = signAliasUpdate
	isAlias := cfg.IsAlias
	if isAlias == nil {
		isAlias = func(lnwire.ShortChannelID) bool {
			return false
		}
	}
	cfg.IsAlias = isAlias

	mailboxTimeout := htlcswitch.DefaultMailboxDeliveryTimeout
	switcher, err := htlcswitch.New(htlcswitch.Config{
		FwdingLog:              cfg.DB.ForwardingLog(),
		LocalChannelClose:      localChannelClose,
		DB:                     cfg.DB,
		FetchAllOpenChannels:   stateDB.FetchAllOpenChannels,
		FetchAllChannels:       stateDB.FetchAllChannels,
		FetchClosedChannels:    stateDB.FetchClosedChannels,
		SwitchPackager:         channeldb.NewSwitchPackager(),
		ExtractErrorEncrypter:  onionProcessor.ExtractErrorEncrypter,
		FetchLastChannelUpdate: fetchLastUpdate,
		Notifier:               cfg.Notifier,
		HtlcNotifier:           htlcNotifier,
		FwdEventTicker: ticker.New(
			htlcswitch.DefaultFwdEventInterval,
		),
		LogEventTicker: ticker.New(
			htlcswitch.DefaultLogInterval,
		),
		AckEventTicker: ticker.New(
			htlcswitch.DefaultAckInterval,
		),
		Clock:                  runtimeClock,
		MailboxDeliveryTimeout: mailboxTimeout,
		MaxFeeExposure:         htlcswitch.DefaultMaxFeeExposure,
		SignAliasUpdate:        signAliasUpdate,
		IsAlias:                isAlias,
	}, uint32(blockHeight))
	if err != nil {
		return nil, fmt.Errorf("create lnd HTLC switch: %w", err)
	}
	interceptor, err := htlcswitch.NewInterceptableSwitch(
		&htlcswitch.InterceptableSwitchConfig{
			Switch:             switcher,
			Notifier:           cfg.Notifier,
			CltvRejectDelta:    defaultOutgoingRejectDelta,
			CltvInterceptDelta: defaultOutgoingRejectDelta * 2,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd interceptable switch: %w",
			err)
	}

	payments, err := NewFixedRoutePayments(FixedRoutePaymentsConfig{
		DB:       cfg.DB,
		Chain:    cfg.Chain,
		Payer:    switcher,
		SelfNode: cfg.SelfNode,
		Clock:    runtimeClock,
		GetLink:  switcher.GetLinkByShortID,
	})
	if err != nil {
		return nil, err
	}

	var fundingRuntime *FundingRuntime
	if cfg.Funding != nil {
		fundingRuntime, err = newFundingRuntime(
			cfg, switcher, *cfg.Funding,
		)
		if err != nil {
			return nil, err
		}
	}

	runtime := &Runtime{
		cfg:                 cfg,
		onionProcessor:      onionProcessor,
		htlcNotifier:        htlcNotifier,
		invoices:            invoiceRegistry,
		switcher:            switcher,
		interceptor:         interceptor,
		payments:            payments,
		funding:             fundingRuntime,
		sigPool:             lnwallet.NewSigPool(1, cfg.Signer),
		awaitingReestablish: make(map[lnwire.ChannelID]struct{}),
		forceCloseCalls:     make(map[wire.OutPoint]*forceCloseCall),
	}
	if cfg.Onchain != nil {
		runtime.onchain, err = newOnchainRuntime(
			runtime, *cfg.Onchain,
		)
		if err != nil {
			return nil, err
		}
	}

	return runtime, nil
}

// validateRuntimeConfig rejects missing stateful dependencies before any lnd
// component starts a goroutine.
func validateRuntimeConfig(cfg RuntimeConfig) error {
	switch {
	case cfg.DB == nil:
		return fmt.Errorf("channel database is required")

	case cfg.Chain == nil:
		return fmt.Errorf("chain backend is required")

	case cfg.Notifier == nil:
		return fmt.Errorf("chain notifier is required")

	case cfg.OnionKey == nil:
		return fmt.Errorf("onion key is required")

	case cfg.Signer == nil:
		return fmt.Errorf("channel signer is required")

	case cfg.FeeEstimator == nil:
		return fmt.Errorf("fee estimator is required")

	case cfg.WitnessBeacon == nil:
		return fmt.Errorf("witness beacon is required")

	default:
		return nil
	}
}

// Start starts native lnd components in dependency order.
func (r *Runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.started {
		return nil
	}
	if r.stopped {
		return fmt.Errorf("channel runtime already stopped")
	}

	if err := r.sigPool.Start(); err != nil {
		return fmt.Errorf("start lnd signature pool: %w", err)
	}
	if err := r.htlcNotifier.Start(); err != nil {
		_ = r.sigPool.Stop()

		return fmt.Errorf("start lnd HTLC notifier: %w", err)
	}
	if err := r.onionProcessor.Start(); err != nil {
		_ = r.htlcNotifier.Stop()
		_ = r.sigPool.Stop()

		return fmt.Errorf("start lnd onion processor: %w", err)
	}
	if err := r.invoices.Start(); err != nil {
		_ = r.onionProcessor.Stop()
		_ = r.htlcNotifier.Stop()
		_ = r.sigPool.Stop()

		return fmt.Errorf("start lnd invoice registry: %w", err)
	}
	if err := r.switcher.Start(); err != nil {
		_ = r.invoices.Stop()
		_ = r.onionProcessor.Stop()
		_ = r.htlcNotifier.Stop()
		_ = r.sigPool.Stop()

		return fmt.Errorf("start lnd HTLC switch: %w", err)
	}
	if err := r.interceptor.Start(); err != nil {
		_ = r.switcher.Stop()
		_ = r.invoices.Stop()
		_ = r.onionProcessor.Stop()
		_ = r.htlcNotifier.Stop()
		_ = r.sigPool.Stop()

		return fmt.Errorf("start lnd interceptable switch: %w", err)
	}
	if err := r.payments.Start(); err != nil {
		_ = r.interceptor.Stop()
		_ = r.switcher.Stop()
		_ = r.invoices.Stop()
		_ = r.onionProcessor.Stop()
		_ = r.htlcNotifier.Stop()
		_ = r.sigPool.Stop()

		return err
	}
	if r.funding != nil {
		if err := r.funding.Start(); err != nil {
			_ = r.payments.Stop()
			_ = r.interceptor.Stop()
			_ = r.switcher.Stop()
			_ = r.invoices.Stop()
			_ = r.onionProcessor.Stop()
			_ = r.htlcNotifier.Stop()
			_ = r.sigPool.Stop()

			return err
		}
	}
	if r.onchain != nil {
		if err := r.onchain.Start(); err != nil {
			if r.funding != nil {
				_ = r.funding.Stop()
			}
			_ = r.payments.Stop()
			_ = r.interceptor.Stop()
			_ = r.switcher.Stop()
			_ = r.invoices.Stop()
			_ = r.onionProcessor.Stop()
			_ = r.htlcNotifier.Stop()
			_ = r.sigPool.Stop()

			return err
		}
	}

	r.started = true

	return nil
}

// Stop shuts native lnd components down in reverse dependency order.
func (r *Runtime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return nil
	}
	r.stopped = true
	if !r.started {
		return nil
	}

	var onchainErr error
	if r.onchain != nil {
		onchainErr = r.onchain.Stop()
	}
	var fundingErr error
	if r.funding != nil {
		fundingErr = r.funding.Stop()
	}

	return errors.Join(
		onchainErr, fundingErr, r.payments.Stop(), r.interceptor.Stop(),
		r.switcher.Stop(), r.invoices.Stop(), r.onionProcessor.Stop(),
		r.htlcNotifier.Stop(), r.sigPool.Stop(),
	)
}

// LinkConfig contains per-channel callbacks owned by the Ark coordinator or
// its on-chain materializer.
type LinkConfig struct {
	Peer             lnpeer.Peer
	Policy           models.ForwardingPolicy
	ChainEvents      *contractcourt.ChainEventSubscription
	SyncStates       bool
	AddsDisabled     bool
	Aliases          []lnwire.ShortChannelID
	MaxAnchorFeeRate chainfee.SatPerKWeight

	OnChannelFailure func(lnwire.ChannelID, lnwire.ShortChannelID,
		htlcswitch.LinkFailureError)
	UpdateContractSignals func(*contractcourt.ContractSignals) error
	NotifyContractUpdate  func(*contractcourt.ContractUpdate) error
	NotifyActive          func()
	NotifyInactive        func()
}

// AddLink reconstructs lnd's LightningChannel from its persisted open-channel
// state and installs the normal channel link in the native HTLC switch.
func (r *Runtime) AddLink(state *chanstate.OpenChannel, cfg LinkConfig) (
	*lnwallet.LightningChannel, error) {

	if state == nil {
		return nil, fmt.Errorf("open channel state is required")
	}
	if cfg.Peer == nil {
		return nil, fmt.Errorf("channel peer is required")
	}
	if cfg.ChainEvents == nil {
		return nil, fmt.Errorf("channel chain events are required")
	}
	if cfg.OnChannelFailure == nil {
		return nil, fmt.Errorf("channel failure callback is required")
	}
	if cfg.UpdateContractSignals == nil {
		return nil, fmt.Errorf("contract signal callback is required")
	}
	if cfg.NotifyContractUpdate == nil {
		return nil, fmt.Errorf("contract update callback is required")
	}

	channel, err := lnwallet.NewLightningChannel(
		r.cfg.Signer, state, r.sigPool,
	)
	if err != nil {
		return nil, fmt.Errorf("restore lnd lightning channel: %w", err)
	}

	aliases := append([]lnwire.ShortChannelID(nil), cfg.Aliases...)
	noTrafficShaper := fn.None[htlcswitch.AuxTrafficShaper]()
	noChannelNegotiator := fn.None[lnwallet.AuxChannelNegotiator]()
	minUpdateTimeout := htlcswitch.DefaultMinLinkFeeUpdateTimeout
	maxUpdateTimeout := htlcswitch.DefaultMaxLinkFeeUpdateTimeout
	getAliases := func(lnwire.ShortChannelID) []lnwire.ShortChannelID {
		return append([]lnwire.ShortChannelID(nil), aliases...)
	}
	linkCfg := htlcswitch.ChannelLinkConfig{
		FwrdingPolicy:           cfg.Policy,
		Circuits:                r.switcher.CircuitModifier(),
		BestHeight:              r.switcher.BestHeight,
		ForwardPackets:          r.interceptor.ForwardPackets,
		DecodeHopIterators:      r.onionProcessor.DecodeHopIterators,
		ExtractErrorEncrypter:   r.onionProcessor.ExtractErrorEncrypter,
		FetchLastChannelUpdate:  r.cfg.FetchLastChannelUpdate,
		Peer:                    cfg.Peer,
		Registry:                r.invoices,
		PreimageCache:           r.cfg.WitnessBeacon,
		OnChannelFailure:        cfg.OnChannelFailure,
		UpdateContractSignals:   cfg.UpdateContractSignals,
		NotifyContractUpdate:    cfg.NotifyContractUpdate,
		ChainEvents:             cfg.ChainEvents,
		FeeEstimator:            r.cfg.FeeEstimator,
		SyncStates:              cfg.SyncStates,
		BatchTicker:             ticker.New(defaultCommitInterval),
		FwdPkgGCTicker:          ticker.New(time.Hour),
		PendingCommitTicker:     durablePeerPendingCommitTicker{},
		BatchSize:               defaultCommitBatchSize,
		MinUpdateTimeout:        minUpdateTimeout,
		MaxUpdateTimeout:        maxUpdateTimeout,
		OutgoingCltvRejectDelta: defaultOutgoingRejectDelta,
		MaxOutgoingCltvExpiry: htlcswitch.
			DefaultMaxOutgoingCltvExpiry,
		MaxFeeAllocation:        htlcswitch.DefaultMaxLinkFeeAllocation,
		MaxAnchorsCommitFeeRate: cfg.MaxAnchorFeeRate,
		NotifyActiveLink: func(_ wire.OutPoint) {
			if cfg.NotifyActive != nil {
				cfg.NotifyActive()
			}
		},
		NotifyActiveChannel: func(_ wire.OutPoint) {},
		NotifyInactiveChannel: func(_ wire.OutPoint) {
			if cfg.NotifyInactive != nil {
				cfg.NotifyInactive()
			}
		},
		NotifyInactiveLinkEvent: func(_ wire.OutPoint) {},
		NotifyChannelUpdate:     func(*chanstate.OpenChannel) {},
		HtlcNotifier:            r.htlcNotifier,
		GetAliases:              getAliases,
		PreviouslySentShutdown:  fn.None[lnwire.Shutdown](),
		DisallowRouteBlinding:   true,
		DisallowQuiescence:      true,
		MaxFeeExposure:          htlcswitch.DefaultMaxFeeExposure,
		ShouldFwdExpAccountability: func() bool {
			return false
		},
		AuxTrafficShaper:     noTrafficShaper,
		AuxChannelNegotiator: noChannelNegotiator,
		QuiescenceTimeout:    defaultQuiescenceTimeout,
	}

	link := htlcswitch.NewChannelLink(linkCfg, channel)
	if cfg.AddsDisabled {
		link.DisableAdds(htlcswitch.Incoming)
		link.DisableAdds(htlcswitch.Outgoing)
	}
	channelID := lnwire.NewChanIDFromOutPoint(state.FundingOutpoint)
	r.setAwaitingReestablish(channelID, cfg.SyncStates)
	if err := r.switcher.AddLink(link); err != nil {
		r.setAwaitingReestablish(channelID, false)

		return nil, fmt.Errorf("add lnd channel link: %w", err)
	}

	return channel, nil
}

// HandleChannelReestablish dispatches an expected startup handshake or
// recycles one live link when only the remote endpoint restarted.
func (r *Runtime) HandleChannelReestablish(message *lnwire.ChannelReestablish,
	peer lnpeer.Peer, configSource LinkConfigSource) error {

	if message == nil {
		return fmt.Errorf("channel reestablish message is required")
	}
	if peer == nil {
		return fmt.Errorf("channel peer is required")
	}
	if configSource == nil {
		return fmt.Errorf("link config source is required")
	}
	if r.claimExpectedReestablish(message.ChanID) {
		return r.handleChannelMessage(message.ChanID, message)
	}

	state, err := r.openChannelByID(peer, message.ChanID)
	if err != nil {
		return err
	}
	r.RemoveLink(state.FundingOutpoint)
	linkConfig, err := configSource(state)
	if err != nil {
		return fmt.Errorf("build reestablished lnd link config: %w",
			err)
	}
	linkConfig.Peer = peer
	linkConfig.SyncStates = true
	if _, err := r.AddLink(state, linkConfig); err != nil {
		return fmt.Errorf("recycle lnd channel link: %w", err)
	}
	if !r.claimExpectedReestablish(message.ChanID) {
		return fmt.Errorf("recycled lnd channel did not await " +
			"reestablish")
	}

	return r.handleChannelMessage(message.ChanID, message)
}

// openChannelByID resolves one authenticated peer's durable channel state.
func (r *Runtime) openChannelByID(peer lnpeer.Peer,
	channelID lnwire.ChannelID) (*chanstate.OpenChannel, error) {

	states, err := r.cfg.DB.ChannelStateDB().FetchOpenChannels(
		peer.IdentityKey(),
	)
	if err != nil {
		return nil, fmt.Errorf("fetch lnd peer channels: %w", err)
	}
	for _, state := range states {
		if lnwire.NewChanIDFromOutPoint(
			state.FundingOutpoint,
		) == channelID {
			return state, nil
		}
	}

	return nil, fmt.Errorf("find lnd channel link: %w",
		htlcswitch.ErrChannelLinkNotFound)
}

// setAwaitingReestablish records whether a newly installed link expects the
// next channel_reestablish instead of treating it as a remote restart signal.
func (r *Runtime) setAwaitingReestablish(channelID lnwire.ChannelID,
	expected bool) {

	r.reestablishMu.Lock()
	defer r.reestablishMu.Unlock()
	if expected {
		r.awaitingReestablish[channelID] = struct{}{}

		return
	}
	delete(r.awaitingReestablish, channelID)
}

// claimExpectedReestablish consumes one startup handshake expectation.
func (r *Runtime) claimExpectedReestablish(channelID lnwire.ChannelID) bool {
	r.reestablishMu.Lock()
	defer r.reestablishMu.Unlock()
	if _, ok := r.awaitingReestablish[channelID]; !ok {
		return false
	}
	delete(r.awaitingReestablish, channelID)

	return true
}

// HandleChannelMessage dispatches one incoming commitment or HTLC update to
// the native lnd link selected by the message's channel ID.
func (r *Runtime) HandleChannelMessage(message lnwire.LinkUpdater) error {
	return r.handleChannelMessage(message.TargetChanID(), message)
}

// HandlePeerMessage routes one authenticated BOLT message to the native lnd
// subsystem that owns it.
func (r *Runtime) HandlePeerMessage(ctx context.Context, message lnwire.Message,
	peer lnpeer.Peer) error {

	if message == nil {
		return fmt.Errorf("lnd peer message is required")
	}
	if peer == nil {
		return fmt.Errorf("lnd peer is required")
	}

	switch message := message.(type) {
	case *lnwire.OpenChannel, *lnwire.AcceptChannel,
		*lnwire.FundingCreated, *lnwire.FundingSigned,
		*lnwire.ChannelReady:

		if r.funding == nil {
			return fmt.Errorf("lnd funding runtime is disabled")
		}

		return r.funding.ProcessMessageSync(ctx, message, peer)

	case *lnwire.Warning:
		return r.handlePeerWarningOrError(
			ctx, message.ChanID, message, peer,
		)

	case *lnwire.Error:
		return r.handlePeerWarningOrError(
			ctx, message.ChanID, message, peer,
		)

	case lnwire.LinkUpdater:
		return r.HandleChannelMessage(message)

	case *lnwire.ChannelReestablish:
		return r.handleChannelMessage(message.ChanID, message)

	case *lnwire.Ping:
		if message.NumPongBytes > lnwire.MaxPongBytes {
			return nil
		}

		pong := lnwire.NewPong(make([]byte, message.NumPongBytes))

		return peer.SendMessage(false, pong)

	case *lnwire.Pong, *lnwire.NodeAnnouncement1,
		*lnwire.ChannelAnnouncement1, *lnwire.ChannelUpdate1:
		return nil

	default:
		return fmt.Errorf("unsupported lnd peer message %T", message)
	}
}

// PeerMessageHandler binds authenticated mailbox ingress to one logical lnd
// peer.
func (r *Runtime) PeerMessageHandler(peer lnpeer.Peer) PeerEventHandler {
	return func(ctx context.Context, message lnwire.Message) error {
		return r.HandlePeerMessage(ctx, message, peer)
	}
}

// handlePeerWarningOrError routes errors for pending channels through the
// funding coordinator and active-channel errors through the channel link.
func (r *Runtime) handlePeerWarningOrError(ctx context.Context,
	channelID lnwire.ChannelID, message lnwire.Message,
	peer lnpeer.Peer) error {

	if r.funding != nil && r.funding.IsPendingChannel(channelID, peer) {
		return r.funding.ProcessMessageSync(ctx, message, peer)
	}

	return r.handleChannelMessage(channelID, message)
}

// handleChannelMessage finds the native link and queues one channel update.
func (r *Runtime) handleChannelMessage(channelID lnwire.ChannelID,
	message lnwire.Message) error {

	link, err := r.switcher.GetLink(channelID)
	if err != nil {
		return fmt.Errorf("find lnd channel link: %w", err)
	}

	link.HandleChannelUpdate(message)

	return nil
}

// Invoices exposes lnd's native registry for invoice creation and
// subscriptions.
func (r *Runtime) Invoices() *invoices.InvoiceRegistry {
	return r.invoices
}

// Payments exposes fixed-route execution and lnd control-tower accounting.
func (r *Runtime) Payments() *FixedRoutePayments {
	return r.payments
}

// Funding exposes lnd's native external funding lifecycle when configured.
func (r *Runtime) Funding() *FundingRuntime {
	return r.funding
}

// GetLink returns an active native lnd channel link by short channel ID.
func (r *Runtime) GetLink(scid lnwire.ShortChannelID) (htlcswitch.ChannelLink,
	error) {

	return r.switcher.GetLinkByShortID(scid)
}

// RemoveLink stops and removes the native lnd link for a channel point.
func (r *Runtime) RemoveLink(channelPoint wire.OutPoint) {
	channelID := lnwire.NewChanIDFromOutPoint(channelPoint)
	r.setAwaitingReestablish(channelID, false)
	r.switcher.RemoveLink(channelID)
}

// WatchChannel admits an active channel to lnd's standard chain and contract
// lifecycle before the unpublished channel point can be materialized.
func (r *Runtime) WatchChannel(channel *chanstate.OpenChannel) error {
	if r.onchain == nil {
		return fmt.Errorf("on-chain lifecycle is disabled")
	}

	return r.onchain.WatchChannel(channel)
}

// HandoffChannel verifies lnd's standard chain lifecycle is armed before Ark
// publishes an active channel's backing transaction. WatchChannel is
// idempotent, so this also repairs a missed registration after restart.
func (r *Runtime) HandoffChannel(channelPoint wire.OutPoint) error {
	if r.onchain == nil {
		return fmt.Errorf("on-chain lifecycle is disabled")
	}
	state, err := r.cfg.DB.ChannelStateDB().FetchChannel(channelPoint)
	if err != nil {
		return fmt.Errorf("find channel for on-chain handoff: %w", err)
	}
	if state.IsPending {
		return fmt.Errorf("cannot hand off a pending channel")
	}
	if err := r.WatchChannel(state); err != nil {
		return fmt.Errorf("watch materialized channel: %w", err)
	}

	return nil
}

// ForceCloseChannel asks the standard lnd chain arbitrator to publish the
// latest commitment and own all output resolutions through completion.
func (r *Runtime) ForceCloseChannel(channelPoint wire.OutPoint) (*wire.MsgTx,
	error) {

	if r.onchain == nil {
		return nil, fmt.Errorf("on-chain lifecycle is disabled")
	}

	return r.runForceClose(channelPoint, func() (*wire.MsgTx, error) {
		return r.onchain.ForceClose(channelPoint)
	})
}

// WaitForceCloseResult waits until lnd has durably classified the channel as
// locally or remotely force closed. This lets an Ark close request converge
// when both endpoints race to spend the newly materialized channel point and
// the peer commitment wins.
func (r *Runtime) WaitForceCloseResult(ctx context.Context,
	channelPoint wire.OutPoint) (chainhash.Hash, error) {

	ticker := time.NewTicker(forceCloseResultPollInterval)
	defer ticker.Stop()

	stateDB := r.cfg.DB.ChannelStateDB()
	for {
		summary, err := stateDB.FetchClosedChannel(&channelPoint)
		switch {
		case err == nil:
			return forceCloseSummaryTxID(summary, channelPoint)

		case !errors.Is(err, channeldb.ErrClosedChannelNotFound):
			return chainhash.Hash{}, fmt.Errorf("find force-close "+
				"result: %w", err)
		}

		select {
		case <-ctx.Done():
			return chainhash.Hash{}, ctx.Err()

		case <-ticker.C:
		}
	}
}

// ResumeForceCloseChannel re-drives the publication edge after Ark has
// durably materialized a channel. A channel already broadcast or moved to the
// closed bucket needs no second force-close request.
func (r *Runtime) ResumeForceCloseChannel(channelPoint wire.OutPoint) error {
	// A normal force close can be waiting inside the Ark backing barrier
	// when the durable channel FSM records materialization. That transition
	// emits the same resume action used by external parent-spend recovery.
	// Let the original request continue instead of recursively waiting on
	// its channel arbitrator.
	if r.forceCloseIsActive(channelPoint) {
		return nil
	}

	stateDB := r.cfg.DB.ChannelStateDB()
	channel, err := stateDB.FetchChannel(channelPoint)
	if errors.Is(err, channeldb.ErrChannelNotFound) {
		if _, closedErr := stateDB.FetchClosedChannel(
			&channelPoint,
		); closedErr != nil {
			return fmt.Errorf("find materialized channel: %w",
				closedErr)
		}

		return nil
	}
	if err != nil {
		return fmt.Errorf("find materialized channel: %w", err)
	}
	if channel.HasChanStatus(channeldb.ChanStatusCommitBroadcasted) {
		return nil
	}
	if _, err := r.onchain.ResumeForceClose(channelPoint); err != nil {
		return fmt.Errorf("resume lnd force close: %w", err)
	}

	return nil
}

// runForceClose coalesces concurrent close requests for one channel point. A
// successful close stays cached for the runtime lifetime, while a failed close
// is removed so the durable lifecycle can retry it.
func (r *Runtime) runForceClose(channelPoint wire.OutPoint,
	forceClose func() (*wire.MsgTx, error)) (*wire.MsgTx, error) {

	r.forceCloseMu.Lock()
	if r.forceCloseCalls == nil {
		r.forceCloseCalls = make(map[wire.OutPoint]*forceCloseCall)
	}
	call, ok := r.forceCloseCalls[channelPoint]
	if !ok {
		call = &forceCloseCall{done: make(chan struct{})}
		r.forceCloseCalls[channelPoint] = call
	}
	r.forceCloseMu.Unlock()

	if ok {
		<-call.done

		return call.tx, call.err
	}

	call.tx, call.err = forceClose()

	r.forceCloseMu.Lock()
	if call.err != nil {
		delete(r.forceCloseCalls, channelPoint)
	}
	close(call.done)
	r.forceCloseMu.Unlock()

	return call.tx, call.err
}

// forceCloseIsActive reports whether the original LND force-close request is
// already responsible for advancing this channel.
func (r *Runtime) forceCloseIsActive(channelPoint wire.OutPoint) bool {
	r.forceCloseMu.Lock()
	defer r.forceCloseMu.Unlock()

	call, ok := r.forceCloseCalls[channelPoint]
	if !ok {
		return false
	}
	select {
	case <-call.done:
		return false

	default:
		return true
	}
}

// forceCloseSummaryTxID validates that a closed-channel record proves the
// exact force-close outcome awaited by the caller.
func forceCloseSummaryTxID(summary *channeldb.ChannelCloseSummary,
	channelPoint wire.OutPoint) (chainhash.Hash, error) {

	if summary == nil {
		return chainhash.Hash{}, fmt.Errorf("force-close summary is " +
			"nil")
	}
	if summary.ChanPoint != channelPoint {
		return chainhash.Hash{}, fmt.Errorf("force-close summary is "+
			"for %v, not %v", summary.ChanPoint, channelPoint)
	}
	switch summary.CloseType {
	case channeldb.LocalForceClose, channeldb.RemoteForceClose:
	default:
		return chainhash.Hash{}, fmt.Errorf("channel closed with type "+
			"%v, not a force close", summary.CloseType)
	}
	if summary.ClosingTXID == (chainhash.Hash{}) {
		return chainhash.Hash{}, fmt.Errorf("force-close transaction " +
			"ID is missing")
	}

	return summary.ClosingTXID, nil
}

// unavailableChannelUpdate is the default for a private runtime with no graph.
func unavailableChannelUpdate(lnwire.ShortChannelID) (*lnwire.ChannelUpdate1,
	error) {

	return nil, fmt.Errorf("channel graph is disabled")
}

// unavailableAliasSignature rejects graph updates in the private runtime.
func unavailableAliasSignature(*lnwire.ChannelUpdate1) (*ecdsa.Signature,
	error) {

	return nil, fmt.Errorf("channel graph is disabled")
}
