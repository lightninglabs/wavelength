package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/discovery"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	defaultFundingTargetConfs = uint32(6)
	defaultMaxLocalCSVDelay   = uint16(2016)
	defaultMaxPendingChannels = 10
)

// FundingConfig contains the application-owned dependencies needed by lnd's
// native funding manager. Protocol defaults remain private and opinionated so
// wallet callers do not need to configure Lightning daemon policy.
type FundingConfig struct {
	WalletController lnwallet.WalletController
	KeyRing          keychain.SecretKeyRing
	NetParams        *chaincfg.Params
	IdentityKey      keychain.KeyDescriptor
	ChannelAcceptor  chanacceptor.ChannelAcceptor
	RoutingPolicy    models.ForwardingPolicy

	NotifyWhenOnline func([33]byte, chan<- lnpeer.Peer)
	WatchNewChannel  func(*chanstate.OpenChannel, *btcec.PublicKey) error

	NotifyPendingOpen func(wire.OutPoint, *chanstate.OpenChannel,
		*btcec.PublicKey)
	NotifyOpen           func(wire.OutPoint, *btcec.PublicKey)
	NotifyFundingTimeout func(wire.OutPoint, *btcec.PublicKey)
	ReportShortChannelID func(wire.OutPoint) error

	MinChannelSize btcutil.Amount
	MaxChannelSize btcutil.Amount
}

// FundingOpenRequest starts an externally funded private channel with an
// explicit initial push allocation.
type FundingOpenRequest struct {
	Peer             lnpeer.Peer
	PendingChannelID lndfunding.PendingChanID
	Capacity         btcutil.Amount
	PushAmount       btcutil.Amount
	BasePSBT         *psbt.Packet
}

// FundingFlow exposes lnd's native asynchronous funding progress.
type FundingFlow struct {
	PendingChannelID lndfunding.PendingChanID
	Updates          <-chan *lnrpc.OpenStatusUpdate
	Errors           <-chan error
}

// FundingRuntime owns lnd's LightningWallet reservation worker and funding
// manager without owning the embedding process's base wallet lifecycle.
type FundingRuntime struct {
	manager  *lndfunding.Manager
	wallet   *lnwallet.LightningWallet
	notifier *VirtualFundingNotifier
	aliases  *aliasmgr.Manager
	switcher *htlcswitch.Switch

	feeEstimator chainfee.Estimator
	netParams    *chaincfg.Params
	chain        lnwallet.BlockChainIO
	stateDB      chanstate.Store
	identityKey  [33]byte
	minChanSize  btcutil.Amount
	maxChanSize  btcutil.Amount

	mu      sync.Mutex
	started bool
	stopped bool
}

// newFundingRuntime composes lnd's normal funding manager around Wavelength's
// already-running wallet and virtual chain notifier.
func newFundingRuntime(runtimeCfg RuntimeConfig, switcher *htlcswitch.Switch,
	cfg FundingConfig) (*FundingRuntime, error) {

	if err := validateFundingConfig(runtimeCfg, cfg); err != nil {
		return nil, err
	}

	virtualNotifier, ok := runtimeCfg.Notifier.(*VirtualFundingNotifier)
	if !ok {
		return nil, fmt.Errorf("funding runtime requires a virtual " +
			"funding notifier")
	}

	lightningWallet, err := lnwallet.NewLightningWallet(lnwallet.Config{
		Database:         runtimeCfg.DB.ChannelStateDB(),
		Notifier:         virtualNotifier,
		SecretKeyRing:    cfg.KeyRing,
		WalletController: cfg.WalletController,
		Signer:           runtimeCfg.Signer,
		FeeEstimator:     runtimeCfg.FeeEstimator,
		ChainIO:          runtimeCfg.Chain,
		NetParams:        *cfg.NetParams,
		CoinSelectionStrategy: basewallet.
			CoinSelectionLargest,
		ExternallyManagedWalletController: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create lnd lightning wallet: %w", err)
	}

	linkUpdater := func(shortID lnwire.ShortChannelID) error {
		link, err := switcher.GetLinkByShortID(shortID)
		if err != nil {
			return err
		}
		switcher.UpdateLinkAliases(link)

		return nil
	}
	aliasManager, err := aliasmgr.NewManager(
		runtimeCfg.DB, linkUpdater,
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd alias manager: %w", err)
	}

	minChanSize := cfg.MinChannelSize
	if minChanSize == 0 {
		minChanSize = lndfunding.MinChanFundingSize
	}
	maxChanSize := cfg.MaxChannelSize
	if maxChanSize == 0 {
		maxChanSize = lndfunding.MaxBtcFundingAmountWumbo
	}

	manager, err := lndfunding.NewFundingManager(lndfunding.Config{
		NoWumboChans:       false,
		IDKey:              cfg.IdentityKey.PubKey,
		IDKeyLoc:           cfg.IdentityKey.KeyLocator,
		Wallet:             lightningWallet,
		PublishTransaction: rejectInternalFundingPublish,
		UpdateLabel: func(chainhash.Hash, string) error {
			return nil
		},
		FeeEstimator:            runtimeCfg.FeeEstimator,
		Notifier:                virtualNotifier,
		ChannelDB:               runtimeCfg.DB.ChannelStateDB(),
		SignMessage:             cfg.KeyRing.SignMessage,
		CurrentNodeAnnouncement: emptyNodeAnnouncement,
		SendAnnouncement:        ignoreAnnouncement,
		NotifyWhenOnline:        cfg.NotifyWhenOnline,
		FindChannel: func(node *btcec.PublicKey,
			chanID lnwire.ChannelID) (*chanstate.OpenChannel,
			error) {

			return findOpenChannel(
				runtimeCfg.DB.ChannelStateDB(), node, chanID,
			)
		},
		DefaultRoutingPolicy: cfg.RoutingPolicy,
		DefaultMinHtlcIn:     1,
		NumRequiredConfs: func(btcutil.Amount,
			lnwire.MilliSatoshi) uint16 {

			return 1
		},
		RequiredRemoteDelay: func(btcutil.Amount) uint16 {
			return lndfunding.MinBtcRemoteDelay
		},
		RequiredRemoteChanReserve: defaultRemoteReserve,
		RequiredRemoteMaxValue:    defaultRemoteMaxValue,
		RequiredRemoteMaxHTLCs: func(btcutil.Amount) uint16 {
			return uint16(input.MaxHTLCNumber / 2)
		},
		WatchNewChannel:       cfg.WatchNewChannel,
		ReportShortChanID:     optionalReportSCID(cfg),
		ZombieSweeperInterval: time.Hour,
		ReservationTimeout: chanfunding.
			DefaultReservationTimeout,
		MinChanSize:                   minChanSize,
		MaxChanSize:                   maxChanSize,
		MaxPendingChannels:            defaultMaxPendingChannels,
		RejectPush:                    false,
		MaxLocalCSVDelay:              defaultMaxLocalCSVDelay,
		NotifyOpenChannelEvent:        optionalNotifyOpen(cfg),
		OpenChannelPredicate:          cfg.ChannelAcceptor,
		NotifyPendingOpenChannelEvent: cfg.NotifyPendingOpen,
		NotifyFundingTimeout:          optionalNotifyTimeout(cfg),
		MaxAnchorsCommitFeeRate: runtimeCfg.FeeEstimator.
			RelayFeePerKW(),
		DeleteAliasEdge: func(lnwire.ShortChannelID) (
			*models.ChannelEdgePolicy, error) {

			return nil, nil
		},
		AliasManager:      aliasManager,
		IsSweeperOutpoint: func(wire.OutPoint) bool { return false },
	})
	if err != nil {
		return nil, fmt.Errorf("create lnd funding manager: %w", err)
	}

	var identityKey [33]byte
	copy(identityKey[:], cfg.IdentityKey.PubKey.SerializeCompressed())

	return &FundingRuntime{
		manager:      manager,
		wallet:       lightningWallet,
		notifier:     virtualNotifier,
		aliases:      aliasManager,
		switcher:     switcher,
		feeEstimator: runtimeCfg.FeeEstimator,
		netParams:    cfg.NetParams,
		chain:        runtimeCfg.Chain,
		stateDB:      runtimeCfg.DB.ChannelStateDB(),
		identityKey:  identityKey,
		minChanSize:  minChanSize,
		maxChanSize:  maxChanSize,
	}, nil
}

// validateFundingConfig rejects missing application-owned policy and safety
// callbacks before lnd starts any funding goroutine.
func validateFundingConfig(runtimeCfg RuntimeConfig, cfg FundingConfig) error {
	switch {
	case cfg.WalletController == nil:
		return fmt.Errorf("funding wallet controller is required")

	case cfg.KeyRing == nil:
		return fmt.Errorf("funding key ring is required")

	case cfg.NetParams == nil:
		return fmt.Errorf("funding network parameters are required")

	case cfg.IdentityKey.PubKey == nil:
		return fmt.Errorf("funding identity key is required")

	case cfg.ChannelAcceptor == nil:
		return fmt.Errorf("channel intent acceptor is required")

	case cfg.NotifyWhenOnline == nil:
		return fmt.Errorf("online peer notifier is required")

	case cfg.WatchNewChannel == nil:
		return fmt.Errorf("new channel watcher is required")

	case cfg.NotifyPendingOpen == nil:
		return fmt.Errorf("pending channel callback is required")

	case cfg.MinChannelSize < 0:
		return fmt.Errorf("minimum channel size cannot be negative")

	case cfg.MaxChannelSize < 0:
		return fmt.Errorf("maximum channel size cannot be negative")

	case cfg.MinChannelSize > 0 && cfg.MaxChannelSize > 0 &&
		cfg.MinChannelSize > cfg.MaxChannelSize:
		return fmt.Errorf("minimum channel size exceeds maximum")

	case runtimeCfg.DB == nil:
		return fmt.Errorf("channel database is required")

	default:
	}

	identityKey, err := cfg.KeyRing.DeriveKey(
		cfg.IdentityKey.KeyLocator,
	)
	if err != nil {
		return fmt.Errorf("derive funding identity key: %w", err)
	}
	if !identityKey.PubKey.IsEqual(cfg.IdentityKey.PubKey) {
		return fmt.Errorf("funding identity key does not match locator")
	}

	return nil
}

// Start starts lnd's wallet reservation handler before the funding manager.
func (f *FundingRuntime) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.started {
		return nil
	}
	if f.stopped {
		return fmt.Errorf("funding runtime already stopped")
	}

	if err := f.wallet.Startup(); err != nil {
		return fmt.Errorf("start lnd lightning wallet: %w", err)
	}
	if err := f.manager.Start(); err != nil {
		_ = f.wallet.Shutdown()

		return fmt.Errorf("start lnd funding manager: %w", err)
	}

	f.started = true

	return nil
}

// Stop stops lnd's funding manager before its wallet reservation handler.
func (f *FundingRuntime) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.stopped {
		return nil
	}
	f.stopped = true
	if !f.started {
		return nil
	}

	return errors.Join(f.manager.Stop(), f.wallet.Shutdown())
}

// OpenChannel starts lnd's PSBT funding flow. The PSBT update reveals the
// negotiated funding output; Ark then builds and signs the VTXO backing spend.
func (f *FundingRuntime) OpenChannel(req FundingOpenRequest) (*FundingFlow,
	error) {

	f.mu.Lock()
	started := f.started
	stopped := f.stopped
	f.mu.Unlock()
	if !started || stopped {
		return nil, fmt.Errorf("funding runtime is not active")
	}

	if req.Peer == nil {
		return nil, fmt.Errorf("funding peer is required")
	}
	if req.PendingChannelID == (lndfunding.PendingChanID{}) {
		return nil, fmt.Errorf("pending channel ID is required")
	}
	if req.Capacity < f.minChanSize || req.Capacity > f.maxChanSize {
		return nil, fmt.Errorf("channel capacity %d outside [%d, %d]",
			req.Capacity, f.minChanSize, f.maxChanSize)
	}
	if req.PushAmount < 0 || req.PushAmount >= req.Capacity {
		return nil, fmt.Errorf("channel push amount %d outside [0, %d)",
			req.PushAmount, req.Capacity)
	}

	feeRate, err := f.feeEstimator.EstimateFeePerKW(
		defaultFundingTargetConfs,
	)
	if err != nil {
		return nil, fmt.Errorf("estimate channel funding fee: %w", err)
	}

	updates := make(chan *lnrpc.OpenStatusUpdate, 8)
	errors := make(chan error, 1)
	assembler := chanfunding.NewPsbtAssembler(
		req.Capacity, req.BasePSBT, f.netParams, false,
	)
	f.manager.InitFundingWorkflow(&lndfunding.InitFundingMsg{
		Peer:            req.Peer,
		TargetPubkey:    req.Peer.IdentityKey(),
		ChainHash:       *f.netParams.GenesisHash,
		LocalFundingAmt: req.Capacity,
		PushAmt: lnwire.NewMSatFromSatoshis(
			req.PushAmount,
		),
		FundingFeePerKw:  feeRate,
		Private:          true,
		MinHtlcIn:        1,
		MaxValueInFlight: lnwire.NewMSatFromSatoshis(req.Capacity),
		MaxHtlcs:         uint16(input.MaxHTLCNumber / 2),
		MaxLocalCsv:      defaultMaxLocalCSVDelay,
		ChanFunder:       assembler,
		PendingChanID:    req.PendingChannelID,
		Updates:          updates,
		Err:              errors,
	})

	return &FundingFlow{
		PendingChannelID: req.PendingChannelID,
		Updates:          updates,
		Errors:           errors,
	}, nil
}

// ExpectedFundingOutput returns the exact output derived by this endpoint's
// native lnd reservation. Both endpoints validate this independently before
// signing the prepared OOR channel's backing transaction.
func (f *FundingRuntime) ExpectedFundingOutput(
	pendingID lndfunding.PendingChanID) (*wire.TxOut, error) {

	for _, reservation := range f.wallet.ActiveReservations() {
		if reservation.PendingChanID() != pendingID {
			continue
		}

		output, err := reservation.FundingOutput()
		if err != nil {
			return nil, fmt.Errorf("read lnd funding output: %w",
				err)
		}

		return output, nil
	}

	return nil, fmt.Errorf("lnd funding reservation %x not found",
		pendingID[:])
}

// FinalizeBacking verifies lnd's negotiated funding output, registers the
// fully signed backing transaction before lnd can watch it, and resumes the
// native funding state machine.
func (f *FundingRuntime) FinalizeBacking(pendingID lndfunding.PendingChanID,
	packet *psbt.Packet, funding VirtualFunding) error {

	if packet == nil {
		return fmt.Errorf("funding PSBT is required")
	}
	// Keep the intent paused after verification. Passing skipFinalize would
	// wake funding.Manager before the notifier knows the backing txid.
	if err := f.wallet.PsbtFundingVerify(
		pendingID, packet, false,
	); err != nil {
		return fmt.Errorf("verify virtual channel funding PSBT: %w",
			err)
	}
	if err := f.notifier.RegisterVirtualFunding(funding); err != nil {
		return err
	}
	if err := f.wallet.PsbtFundingFinalize(
		pendingID, nil, funding.Transaction,
	); err != nil {

		_ = f.notifier.UnregisterVirtualFunding(
			funding.Transaction.TxHash(),
		)

		return fmt.Errorf("finalize virtual channel funding PSBT: %w",
			err)
	}

	return nil
}

// RegisterBacking lets the responder install the same fully signed backing
// transaction before the initiator resumes lnd's funding exchange.
func (f *FundingRuntime) RegisterBacking(funding VirtualFunding) error {
	return f.notifier.RegisterVirtualFunding(funding)
}

// CancelBacking cancels either an in-memory PSBT intent or the pending lnd
// channel that replaced it after commitment finalization.
func (f *FundingRuntime) CancelBacking(pendingID lndfunding.PendingChanID,
	channelPoint *wire.OutPoint) error {

	if channelPoint != nil {
		channel, err := f.stateDB.FetchChannel(*channelPoint)
		switch {
		case err == nil:
			return f.abandonPendingBacking(channel)

		case !errors.Is(err, channeldb.ErrChannelNotFound):
			return fmt.Errorf("find pending lnd channel: %w", err)
		}
	}

	cancelErr := f.wallet.CancelFundingIntent(pendingID)
	if cancelErr == nil {
		if channelPoint == nil {
			return nil
		}

		return f.notifier.UnregisterVirtualFunding(channelPoint.Hash)
	}
	if channelPoint == nil {
		return fmt.Errorf("cancel lnd funding intent: %w", cancelErr)
	}

	// CompleteReservation removes the PSBT intent before it persists the
	// pending channel. Re-read the channel DB to close that race.
	channel, err := f.stateDB.FetchChannel(*channelPoint)
	if err == nil {
		return f.abandonPendingBacking(channel)
	}
	if !errors.Is(err, channeldb.ErrChannelNotFound) {
		return fmt.Errorf("reconcile pending lnd channel: %w", err)
	}
	_, err = f.stateDB.FetchClosedChannel(channelPoint)
	if err == nil {
		return nil
	}
	if !errors.Is(err, channeldb.ErrClosedChannelNotFound) {
		return fmt.Errorf("find canceled lnd channel: %w", err)
	}

	return fmt.Errorf("cancel lnd funding intent: %w", cancelErr)
}

// abandonPendingBacking removes finalized channel state before Ark commits the
// prepared OOR transfer.
func (f *FundingRuntime) abandonPendingBacking(
	channel *chanstate.OpenChannel) error {

	if !channel.IsPending {
		return fmt.Errorf("cannot cancel active lnd channel %v",
			channel.FundingOutpoint)
	}
	if err := f.notifier.CancelVirtualFunding(
		channel.FundingOutpoint.Hash,
	); err != nil {
		return err
	}
	_, height, err := f.chain.GetBestBlock()
	if err != nil {
		return fmt.Errorf("read height for channel cancellation: %w",
			err)
	}
	if err := f.stateDB.AbandonChannel(
		&channel.FundingOutpoint, uint32(height),
	); err != nil {
		return fmt.Errorf("abandon pending lnd channel: %w", err)
	}

	return nil
}

// ConfirmBacking opens lnd's channel only after the Ark FSM's durable OOR and
// backing-signature gates are satisfied.
func (f *FundingRuntime) ConfirmBacking(txid chainhash.Hash) error {
	return f.notifier.ConfirmVirtualFunding(txid)
}

// ReorgBacking retracts activation when the channel's Ark ancestry reorgs.
func (f *FundingRuntime) ReorgBacking(txid chainhash.Hash, depth int32) error {
	return f.notifier.ReorgVirtualFunding(txid, depth)
}

// ProcessMessage dispatches one native funding message received over the
// application peer transport.
func (f *FundingRuntime) ProcessMessage(message lnwire.Message,
	peer lnpeer.Peer) error {

	switch message.(type) {
	case *lnwire.OpenChannel, *lnwire.AcceptChannel,
		*lnwire.FundingCreated, *lnwire.FundingSigned,
		*lnwire.ChannelReady, *lnwire.Warning, *lnwire.Error:

		f.manager.ProcessFundingMsg(message, peer)

		return nil

	default:
		return fmt.Errorf("unsupported funding message %T", message)
	}
}

// ProcessMessageSync dispatches one native funding message and waits until
// lnd's funding coordinator has handled it. Durable ingress uses this before
// acknowledging the application transport envelope.
func (f *FundingRuntime) ProcessMessageSync(ctx context.Context,
	message lnwire.Message, peer lnpeer.Peer) error {

	switch message.(type) {
	case *lnwire.OpenChannel, *lnwire.AcceptChannel,
		*lnwire.FundingCreated, *lnwire.FundingSigned,
		*lnwire.ChannelReady, *lnwire.Warning, *lnwire.Error:
		return f.manager.ProcessFundingMsgSync(ctx, message, peer)

	default:
		return fmt.Errorf("unsupported funding message %T", message)
	}
}

// IsPendingChannel reports whether lnd's funding manager still owns the
// message's temporary channel ID for this peer.
func (f *FundingRuntime) IsPendingChannel(channelID lnwire.ChannelID,
	peer lnpeer.Peer) bool {

	return f.manager.IsPendingChannel(channelID, peer)
}

// AddLocalAlias associates a reserved future SCID with an active virtual
// channel before an intercepted HTLC is resumed.
func (f *FundingRuntime) AddLocalAlias(
	alias, base lnwire.ShortChannelID) error {

	return f.aliases.AddLocalAlias(alias, base, false, true)
}

// findOpenChannel resolves funding channel IDs without constructing a graph.
func findOpenChannel(store chanstate.Store, node *btcec.PublicKey,
	chanID lnwire.ChannelID) (*chanstate.OpenChannel, error) {

	channels, err := store.FetchOpenChannels(node)
	if err != nil {
		return nil, err
	}
	for _, channel := range channels {
		if chanID.IsChanPoint(&channel.FundingOutpoint) {
			return channel, nil
		}
	}

	return nil, fmt.Errorf("channel %v not found", chanID)
}

// defaultRemoteReserve preserves lnd's standard one-percent remote reserve.
func defaultRemoteReserve(capacity, dustLimit btcutil.Amount) btcutil.Amount {
	reserve := capacity / 100
	if reserve < dustLimit {
		return dustLimit
	}

	return reserve
}

// defaultRemoteMaxValue allows all remote liquidity except its reserve.
func defaultRemoteMaxValue(capacity btcutil.Amount) lnwire.MilliSatoshi {
	reserve := lnwire.NewMSatFromSatoshis(capacity / 100)

	return lnwire.NewMSatFromSatoshis(capacity) - reserve
}

// rejectInternalFundingPublish makes accidental use of lnd's wallet-funded
// path visible instead of broadcasting outside the Ark materializer.
func rejectInternalFundingPublish(*wire.MsgTx, string) error {
	return fmt.Errorf("internal funding publication is disabled")
}

// emptyNodeAnnouncement satisfies private-channel funding without starting a
// public graph or gossiper.
func emptyNodeAnnouncement() (lnwire.NodeAnnouncement1, error) {
	return lnwire.NodeAnnouncement1{
		Features: lnwire.NewRawFeatureVector(),
	}, nil
}

// ignoreAnnouncement completes private funding gossip calls locally.
func ignoreAnnouncement(lnwire.Message,
	...discovery.OptionalMsgField) actor.Future[error] {

	promise := actor.NewPromise[error]()
	actor.CompleteWith(promise, nil)

	return promise.Future()
}

// optionalReportSCID supplies a no-op for runtimes with no confirmed-SCID
// callback.
func optionalReportSCID(cfg FundingConfig) func(wire.OutPoint) error {
	if cfg.ReportShortChannelID != nil {
		return cfg.ReportShortChannelID
	}

	return func(wire.OutPoint) error { return nil }
}

// optionalNotifyOpen supplies lnd's required open callback.
func optionalNotifyOpen(
	cfg FundingConfig) func(wire.OutPoint, *btcec.PublicKey) {

	if cfg.NotifyOpen != nil {
		return cfg.NotifyOpen
	}

	return func(wire.OutPoint, *btcec.PublicKey) {}
}

// optionalNotifyTimeout supplies lnd's required funding-timeout callback.
func optionalNotifyTimeout(
	cfg FundingConfig) func(wire.OutPoint, *btcec.PublicKey) {

	if cfg.NotifyFundingTimeout != nil {
		return cfg.NotifyFundingTimeout
	}

	return func(wire.OutPoint, *btcec.PublicKey) {}
}
