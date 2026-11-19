package waved

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrInsufficientArkChannelLiquidity means no active private channel
	// has enough balance on the requested sending side.
	ErrInsufficientArkChannelLiquidity = fmt.Errorf("insufficient active " +
		"Ark channel liquidity")
)

const (
	arkChannelArkKeyFamily            keychain.KeyFamily = 220
	arkChannelBackingKeyFamily        keychain.KeyFamily = 221
	arkChannelFunderKeyFamily         keychain.KeyFamily = 223
	arkChannelPaymentCleanupTimeout                      = 30 * time.Second
	maxArkChannelIdempotencyKeyLength                    = 128
)

// HubArkChannelControllerConfig contains the hub-only signer, publisher, and
// authenticated client transport needed to compose one native endpoint.
type HubArkChannelControllerConfig struct {
	Process ArkChannelControllerConfig

	RemoteNode    [33]byte
	PeerSender    lnruntime.PeerEventSender
	Info          lnruntime.FundingPeerInfo
	CloseObserver lnruntime.CooperativeCloseObserver
	CloseDefender lnruntime.CooperativeCloseDefender
	PaymentBridge lnruntime.PaymentBridgeCoordinator

	// NodeFactory attaches this peer to a process-owned native engine.
	// Nil retains a dedicated client-style engine and database.
	NodeFactory lnruntime.NativeNodeFactory

	// RecoveryDeltaFloor covers the longest persisted obligation across
	// every peer sharing the engine, including peers not yet attached.
	RecoveryDeltaFloor uint32

	// DeferStart lets the process prepare every durable peer and backing
	// before any shared lnd funding or on-chain restoration starts.
	DeferStart bool
}

// NativeArkChannelController owns one Ark FSM and one modular lnd endpoint.
type NativeArkChannelController struct {
	party arkchannel.Party
	cfg   ArkChannelControllerConfig

	prePONROOR  prePONRResultController
	coordinator *arkchannel.Coordinator
	remote      lnruntime.ProcessFundingPeer
	fundingPeer lnruntime.RecoveryCounterparty
	paymentPeer lnruntime.ProcessPaymentPeer
	peerInfo    lnruntime.FundingPeerInfo
	clientPeer  clientArkChannelPeer
	keys        nativeArkChannelKeys

	mu            sync.RWMutex
	node          *lnruntime.NativeNode
	service       *arkchannel.Service
	clientClose   *lnruntime.ClientCooperativeCloseProcess
	hubClose      *lnruntime.HubCooperativeCloseProcess
	paymentBridge lnruntime.PaymentBridgeCoordinator
	startAttempt  *arkChannelStartAttempt
	stopped       bool

	lifecycleCtx        context.Context //nolint:containedctx // owned
	lifecycleCancel     context.CancelFunc
	lifecycleWG         sync.WaitGroup
	workersOnce         sync.Once
	expiryPayoutWatches sync.Map
	stopOnce            sync.Once
	stopErr             error

	clientStarter func(context.Context) error
	nodeFactory   lnruntime.NativeNodeFactory
	deferHubStart bool
	hubStartMu    sync.Mutex
	hubStarted    bool
}

// arkChannelStartAttempt is one shared client endpoint startup result.
type arkChannelStartAttempt struct {
	done chan struct{}
	err  error
}

var _ contractcourt.AuxChannelLifecycle = (*NativeArkChannelController)(nil)

// nativeArkChannelKeys are fixed wallet roles restored by locator on restart.
type nativeArkChannelKeys struct {
	ark     keychain.KeyDescriptor
	backing keychain.KeyDescriptor
	funder  keychain.KeyDescriptor
}

// loggedArkChannelForceCloser makes the irreversible lnd handoff observable
// while preserving lnd's idempotent close API as the implementation boundary.
type loggedArkChannelForceCloser struct {
	node *lnruntime.NativeNode
	log  btclog.Logger
}

// EnsureForceCloseChannel resumes an interrupted close or starts the peer's
// close after source-spend recovery materializes the channel.
func (c *loggedArkChannelForceCloser) EnsureForceCloseChannel(
	channelPoint wire.OutPoint) error {

	ctx := context.Background()
	c.log.InfoS(ctx, "Ensuring Ark channel force close",
		btclog.Fmt("channel_point", "%v", channelPoint),
	)
	err := c.node.EnsureForceCloseChannel(channelPoint)
	if err != nil {
		c.log.WarnS(ctx, "Ark channel force close failed",
			err,
			btclog.Fmt("channel_point", "%v", channelPoint),
		)

		return err
	}
	c.log.InfoS(ctx, "Ark channel force close ensured",
		btclog.Fmt("channel_point", "%v", channelPoint),
	)

	return nil
}

// LightningPaymentResult is the atomic public-payment result returned after
// the private source settled with the same preimage.
type LightningPaymentResult struct {
	PaymentHash   lntypes.Hash
	Preimage      lntypes.Preimage
	PrivateAmount btcutil.Amount
	Fee           btcutil.Amount
	ChannelID     arkchannel.ID
}

// ArkChannelMaterializationDelta sizes one native arbitrator for both current
// admissions and every persisted channel it must protect. A shared engine's
// caller must include every peer, not just the first endpoint to start.
func ArkChannelMaterializationDelta(records []arkchannel.Record,
	floor uint32) uint32 {

	for _, record := range records {
		floor = max(floor, record.Snapshot.Terms.VTXO.FunderDelay)
	}

	return floor
}

// NewHubFundingPeerInfo derives the immutable channel policy advertised by a
// real operator Wavelength process. The key roles share the same deterministic
// locators used when the hub endpoint is restored after restart.
func NewHubFundingPeerInfo(ctx context.Context,
	cfg ArkChannelControllerConfig) (lnruntime.FundingPeerInfo, error) {

	if err := validateArkChannelProcessConfig(cfg); err != nil {
		return lnruntime.FundingPeerInfo{}, err
	}
	if cfg.OperatorTerms == nil || cfg.OperatorTerms.PubKey == nil {
		return lnruntime.FundingPeerInfo{}, fmt.Errorf("Ark operator " +
			"terms are required")
	}
	keys, err := deriveNativeArkChannelKeys(ctx, cfg, arkchannel.PartyHub)
	if err != nil {
		return lnruntime.FundingPeerInfo{}, err
	}
	channelDelay := cfg.OperatorTerms.VTXOExitDelay
	if channelDelay > ^uint32(0)-arkscript.DefaultChannelReactionWindow {
		return lnruntime.FundingPeerInfo{}, fmt.Errorf("Ark channel " +
			"delay exceeds sequence range")
	}
	info := lnruntime.FundingPeerInfo{
		ChannelDelay: channelDelay,
		FunderDelay: channelDelay +
			arkscript.DefaultChannelReactionWindow,
		MinimumExitDelay: channelDelay,
	}
	copy(info.HubNodeKey[:], cfg.IdentityKey.PubKey.SerializeCompressed())
	copy(info.HubArkKey[:], keys.ark.PubKey.SerializeCompressed())
	copy(info.HubChannelKey[:], keys.backing.PubKey.SerializeCompressed())
	copy(
		info.ArkOperatorKey[:],
		cfg.OperatorTerms.PubKey.SerializeCompressed(),
	)

	return info, info.Validate()
}

// NewClientArkChannelController constructs a lazy client endpoint. Startup
// restores existing channels locally. First use authenticates hub
// policy over the mailbox before starting native lnd components.
func NewClientArkChannelController(ctx context.Context,
	cfg ArkChannelControllerConfig) (*NativeArkChannelController, error) {

	cfg = withArkChannelControllerDefaults(cfg)

	if err := validateClientArkChannelProcessConfig(cfg); err != nil {
		return nil, err
	}
	var observers []arkchannel.RecordObserver
	if cfg.RecordObserver != nil {
		observers = append(observers, cfg.RecordObserver)
	}
	coordinator, err := arkchannel.NewCoordinator(cfg.Store, observers...)
	if err != nil {
		return nil, err
	}
	remote, err := lnruntime.NewMailboxFundingPeer(cfg.PeerRPC)
	if err != nil {
		return nil, err
	}
	keys, err := deriveNativeArkChannelKeys(
		ctx, cfg, arkchannel.PartyClient,
	)
	if err != nil {
		return nil, err
	}

	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, cfg: cfg, prePONROOR: cfg.OOR,
		coordinator: coordinator, remote: remote,
		fundingPeer: remote, paymentPeer: remote, keys: keys,
	}
	controller.initLifecycle(ctx)

	return controller, nil
}

// NewHubArkChannelController eagerly composes one authenticated client
// endpoint for swapserver's channel directory.
func NewHubArkChannelController(ctx context.Context,
	cfg HubArkChannelControllerConfig) (*NativeArkChannelController,
	error) {

	cfg.Process = withArkChannelControllerDefaults(cfg.Process)

	if err := validateArkChannelProcessConfig(cfg.Process); err != nil {
		return nil, err
	}
	if _, err := btcec.ParsePubKey(cfg.RemoteNode[:]); err != nil {
		return nil, fmt.Errorf("parse client channel node: %w", err)
	}
	if cfg.PeerSender == nil || cfg.CloseObserver == nil ||
		cfg.CloseDefender == nil {
		return nil, fmt.Errorf("complete hub channel process is " +
			"required")
	}
	if err := cfg.Info.Validate(); err != nil {
		return nil, err
	}
	var observers []arkchannel.RecordObserver
	if cfg.Process.RecordObserver != nil {
		observers = append(observers, cfg.Process.RecordObserver)
	}
	coordinator, err := arkchannel.NewCoordinator(
		cfg.Process.Store, observers...,
	)
	if err != nil {
		return nil, err
	}
	keys, err := deriveNativeArkChannelKeys(
		ctx, cfg.Process, arkchannel.PartyHub,
	)
	if err != nil {
		return nil, err
	}
	controller := &NativeArkChannelController{
		party: arkchannel.PartyHub, cfg: cfg.Process,
		coordinator: coordinator, peerInfo: cfg.Info, keys: keys,
		paymentBridge: cfg.PaymentBridge,
		nodeFactory:   cfg.NodeFactory, deferHubStart: cfg.DeferStart,
	}
	controller.initLifecycle(ctx)
	if err := controller.startHub(
		ctx, cfg.RemoteNode, cfg.PeerSender, cfg.CloseObserver,
		cfg.CloseDefender, cfg.RecoveryDeltaFloor,
	); err != nil {

		controller.lifecycleCancel()

		return nil, err
	}

	return controller, nil
}

// initLifecycle creates the process-owned cancellation root used by startup,
// recovery, and maintenance workers.
func (c *NativeArkChannelController) initLifecycle(parent context.Context) {
	c.lifecycleCtx, c.lifecycleCancel = context.WithCancel(
		context.WithoutCancel(parent),
	)
}

// validateArkChannelProcessConfig rejects incomplete process composition.
func validateArkChannelProcessConfig(cfg ArkChannelControllerConfig) error {
	switch {
	case cfg.Store == nil:
		return fmt.Errorf("Ark channel store is required")

	case cfg.Wallet == nil:
		return fmt.Errorf("Ark channel wallet is required")

	case cfg.ChainBackend == nil || cfg.ChainNotifier == nil:
		return fmt.Errorf("Ark channel chain backend is required")

	case cfg.FeeEstimator == nil:
		return fmt.Errorf("Ark channel fee estimator is required")

	case cfg.Materializer == nil || cfg.Recovery == nil:
		return fmt.Errorf("Ark channel recovery runtime is required")

	case cfg.IdentityKey.PubKey == nil:
		return fmt.Errorf("Ark channel identity key is required")

	case cfg.OORDestination == nil:
		return fmt.Errorf("Ark channel OOR destination key is required")

	case cfg.NetParams == nil:
		return fmt.Errorf("Ark channel network is required")

	case cfg.ChannelDataDir == "":
		return fmt.Errorf("Ark channel data directory is required")

	default:
		return nil
	}
}

// validateClientArkChannelProcessConfig checks the funding and publication
// dependencies that only the client-owned promotion process may execute.
func validateClientArkChannelProcessConfig(
	cfg ArkChannelControllerConfig) error {

	if err := validateArkChannelProcessConfig(cfg); err != nil {
		return err
	}
	switch {
	case cfg.Peer == nil || cfg.PeerRPC == nil || cfg.PeerSender == nil:
		return fmt.Errorf("Ark channel peer transport is required")

	case cfg.OOR == nil || cfg.Materializer == nil:
		return fmt.Errorf("Ark channel OOR and unroller are required")

	case cfg.PrepareOOR == nil:
		return fmt.Errorf("Ark channel OOR preparer is required")

	case cfg.LookupOOR == nil:
		return fmt.Errorf("Ark channel OOR lookup is required")

	default:
		return nil
	}
}

// deriveNativeArkChannelKeys restores stable process-owned policy roles.
func deriveNativeArkChannelKeys(ctx context.Context,
	cfg ArkChannelControllerConfig,
	party arkchannel.Party) (nativeArkChannelKeys, error) {

	derive := func(family keychain.KeyFamily) (keychain.KeyDescriptor,
		error) {

		desc, err := cfg.Wallet.DeriveKey(
			ctx,
			keychain.KeyLocator{
				Family: family, Index: cfg.KeyIndex,
			},
		)
		if err != nil {
			return keychain.KeyDescriptor{}, err
		}

		return *desc, nil
	}
	arkKey, err := derive(arkChannelArkKeyFamily)
	if err != nil {
		return nativeArkChannelKeys{}, err
	}
	backingKey, err := derive(arkChannelBackingKeyFamily)
	if err != nil {
		return nativeArkChannelKeys{}, err
	}
	keys := nativeArkChannelKeys{ark: arkKey, backing: backingKey}
	if party == arkchannel.PartyClient {
		keys.funder, err = derive(arkChannelFunderKeyFamily)
		if err != nil {
			return nativeArkChannelKeys{}, err
		}
	}

	return keys, nil
}

// ensureClientStarted loads hub policy and starts the local native endpoint.
func (c *NativeArkChannelController) ensureClientStarted(
	ctx context.Context) error {

	c.mu.Lock()
	if c.node != nil {
		c.mu.Unlock()

		return nil
	}
	if c.stopped {
		c.mu.Unlock()

		return fmt.Errorf("Ark channel controller is stopped")
	}
	if c.lifecycleCtx == nil {
		c.mu.Unlock()

		return fmt.Errorf("Ark channel controller is not started")
	}
	attempt := c.startAttempt
	if attempt == nil {
		attempt = &arkChannelStartAttempt{done: make(chan struct{})}
		c.startAttempt = attempt
		c.lifecycleWG.Add(1)
		//nolint:contextcheck // startup uses the controller lifecycle
		go c.runClientStart(attempt)
	}
	c.mu.Unlock()

	select {
	case <-attempt.done:
		return attempt.err

	case <-ctx.Done():
		return ctx.Err()
	}
}

// runClientStart executes one bounded process-owned startup attempt.
func (c *NativeArkChannelController) runClientStart(
	attempt *arkChannelStartAttempt) {

	defer c.lifecycleWG.Done()
	startCtx, cancel := context.WithTimeout(
		c.lifecycleCtx, defaultArkChannelClientStartTimeout,
	)
	defer cancel()

	starter := c.startClient
	if c.clientStarter != nil {
		starter = c.clientStarter
	}
	err := starter(startCtx)

	c.mu.Lock()
	attempt.err = err
	if c.startAttempt == attempt {
		c.startAttempt = nil
	}
	close(attempt.done)
	c.mu.Unlock()
}

// startClient loads hub policy and composes the local native endpoint.
func (c *NativeArkChannelController) startClient(ctx context.Context) error {
	delivery, err := c.refreshDelivery()
	if err != nil {
		return err
	}
	peer, err := c.clientStartupPeer(ctx)
	if err != nil {
		return err
	}
	remoteKey, err := btcec.ParsePubKey(peer.nodeKey[:])
	if err != nil {
		return err
	}
	node, err := c.newNode(
		ctx, arkchannel.PartyClient, remoteKey, c.cfg.PeerSender,
		peer.materializationDelta,
	)
	if err != nil {
		return err
	}
	negotiator, err := node.NewNegotiator(c.fundingPeer, c.cfg.Recovery)
	if err != nil {
		_ = node.Stop()

		return err
	}
	closeEndpoint, err := lnruntime.NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyClient, node.Runtime(), nil,
		keychain.KeyDescriptor{}, delivery,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	publisher := lnruntime.CooperativeClosePublisherFunc(func(
		ctx context.Context, id arkchannel.ID, terms arkchannel.Terms,
		source arkchannel.VTXOBinding,
		request arkchannel.CooperativeCloseRequest,
		settlement arkchannel.CooperativeClose) error {

		return c.cfg.OOR.SettleCooperativeClose(
			ctx, id, terms, source, request, settlement, c.keys.ark,
			delivery.CooperativeCloseDelivery,
		)
	})
	clientClose, err := lnruntime.NewClientCooperativeCloseProcess(
		closeEndpoint, c.cfg.Peer, publisher, delivery,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	service, err := c.newService(node, negotiator, clientClose)
	if err != nil {
		_ = node.Stop()

		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		_ = node.Stop()

		return fmt.Errorf("Ark channel controller is stopped")
	}
	c.service = service
	c.mu.Unlock()
	cleanup := func() {
		_ = node.Stop()

		c.mu.Lock()
		if c.service == service {
			c.service = nil
		}
		c.mu.Unlock()
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, restoreNativeArkChannelBackings(ctx, node, service),
		c.cfg.Log, "Ark channel backing restore failed",
	); err != nil {

		cleanup()

		return err
	}
	if err := node.Start(); err != nil {
		cleanup()

		return err
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, c.restoreRecoveryWatches(ctx, service), c.cfg.Log,
		"Ark channel recovery watch restore failed",
	); err != nil {

		cleanup()

		return err
	}
	resumeIDs, err := nativeArkChannelResumeIDs(ctx, service)
	if err != nil {
		cleanup()

		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		cleanup()

		return fmt.Errorf("Ark channel controller is stopped")
	}
	c.clientPeer = peer
	c.node = node
	c.clientClose = clientClose
	c.mu.Unlock()
	//nolint:contextcheck // recovery uses the controller lifecycle
	c.startNativeArkChannelResume(service, node, resumeIDs)
	//nolint:contextcheck // expiry protection owns the controller lifecycle
	c.startLifecycleWorker(func(ctx context.Context) {
		c.runSourceExpiryGuard(ctx, service, node)
	})

	return nil
}

// startHub starts one operator endpoint for an authenticated client.
func (c *NativeArkChannelController) startHub(ctx context.Context,
	remoteNode [33]byte, sender lnruntime.PeerEventSender,
	closeObserver lnruntime.CooperativeCloseObserver,
	closeDefender lnruntime.CooperativeCloseDefender,
	sharedFloor uint32) error {

	remoteKey, err := btcec.ParsePubKey(remoteNode[:])
	if err != nil {
		return err
	}
	records, err := c.coordinator.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	delta := ArkChannelMaterializationDelta(
		records, max(c.peerInfo.FunderDelay, sharedFloor),
	)
	node, err := c.newNode(
		ctx, arkchannel.PartyHub, remoteKey, sender, delta,
	)
	if err != nil {
		return err
	}
	delivery, err := c.refreshDelivery()
	if err != nil {
		_ = node.Stop()

		return err
	}
	closeEndpoint, err := lnruntime.NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyHub, node.Runtime(), c.cfg.Wallet.BtcWallet,
		c.keys.ark, delivery,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	hubClose, err := lnruntime.NewHubCooperativeCloseProcess(
		closeEndpoint, delivery, closeObserver, closeDefender,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	service, err := c.newService(
		node, node.FundingEndpoint(),
		&lnruntime.HubCooperativeCloseExecutor{
			HubCooperativeCloseProcess: hubClose,
		},
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	c.service = service
	cleanup := func() {
		c.service = nil
		_ = node.Stop()
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, restoreNativeArkChannelBackingRecords(node, records),
		c.cfg.Log, "Ark channel backing restore failed",
	); err != nil {

		cleanup()

		return err
	}
	c.mu.Lock()
	c.node, c.hubClose = node, hubClose
	c.mu.Unlock()
	if c.deferHubStart {
		return nil
	}
	if err := c.StartHub(ctx); err != nil {
		cleanup()

		return err
	}

	return nil
}

// StartHub starts a prepared hub peer after all durable peers are registered.
// A shared engine is started once; links and Ark recovery stay peer-scoped.
func (c *NativeArkChannelController) StartHub(ctx context.Context) error {
	c.hubStartMu.Lock()
	defer c.hubStartMu.Unlock()
	if c.hubStarted {
		return nil
	}
	c.mu.RLock()
	node, service := c.node, c.service
	stopped := c.stopped
	c.mu.RUnlock()
	if c.party != arkchannel.PartyHub || stopped || node == nil {
		return fmt.Errorf("prepared native hub endpoint is required")
	}
	if err := node.Start(); err != nil {
		return err
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, c.restoreRecoveryWatches(ctx, service), c.cfg.Log,
		"Ark channel recovery watch restore failed",
	); err != nil {
		return err
	}
	resumeIDs, err := nativeArkChannelResumeIDs(ctx, service)
	if err != nil {
		return err
	}
	//nolint:contextcheck // recovery uses the controller lifecycle
	c.startNativeArkChannelResume(service, node, resumeIDs)
	//nolint:contextcheck // expiry protection owns the controller lifecycle
	c.startLifecycleWorker(func(ctx context.Context) {
		c.runSourceExpiryGuard(ctx, service, node)
	})
	c.hubStarted = true

	return nil
}

// arkChannelBackingRestorer registers virtual funding before lnd restores its
// pending funding manager state.
type arkChannelBackingRestorer interface {
	RestoreBacking(arkchannel.Terms, arkchannel.Backing) error
}

// restoreNativeArkChannelBackings reconstructs the notifier's in-memory map
// from durable FSM records before any lnd subsystem starts.
func restoreNativeArkChannelBackings(ctx context.Context,
	restorer arkChannelBackingRestorer, service *arkchannel.Service) error {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return err
	}

	return restoreNativeArkChannelBackingRecords(restorer, records)
}

// restoreNativeArkChannelBackingRecords registers every signed backing in one
// pre-start snapshot of the durable channel store.
func restoreNativeArkChannelBackingRecords(restorer arkChannelBackingRestorer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		backing := record.Snapshot.Backing
		if backing == nil ||
			record.Snapshot.Phase == arkchannel.PhaseClosed {

			continue
		}
		if err := restorer.RestoreBacking(
			record.Snapshot.Terms, *backing,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"restore backing: %w", err,
				),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// tolerateNativeArkChannelFailures reports durable per-channel failures without
// tearing down independent links that recovered successfully.
func tolerateNativeArkChannelFailures(ctx context.Context, err error,
	log btclog.Logger, message string) error {

	if err == nil {
		return nil
	}
	var failures *arkchannel.ResumeFailures
	if !errors.As(err, &failures) {
		return err
	}
	if log == nil {
		log = btclog.Disabled
	}
	for _, failure := range failures.Failures {
		log.WarnS(
			ctx, message, failure.Err, btclog.Fmt(
				"channel_id", "%x", failure.ChannelID[:],
			),
		)
	}

	return nil
}

// arkChannelForceCloseResumer reconciles one materialized channel with lnd's
// durable commitment-broadcast state.
type arkChannelForceCloseResumer interface {
	ResumeForceCloseChannel(wire.OutPoint) error
}

// resumeOnchainArkChannelRecords attempts every materialized channel and
// returns only isolated failures after the complete recovery pass.
func resumeOnchainArkChannelRecords(node arkChannelForceCloseResumer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		if !shouldResumeOnchainArkChannel(record.Snapshot) {
			continue
		}
		if record.Snapshot.Backing == nil {
			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"materialized channel has no " +
						"backing",
				),
			})

			continue
		}
		if err := node.ResumeForceCloseChannel(
			record.Snapshot.Backing.ChannelPoint,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf("resume materialized channel: "+
					"%w",
					err),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// shouldResumeOnchainArkChannel reports whether backing publication crossed
// the durable handoff but lnd may still need its commitment broadcast marker.
func shouldResumeOnchainArkChannel(snapshot arkchannel.Snapshot) bool {
	return snapshot.Phase == arkchannel.PhaseOnChain
}

// restoreRecoveryWatches arms persisted ancestry before replaying any channel
// action that could activate or publish native lnd state.
func (c *NativeArkChannelController) restoreRecoveryWatches(ctx context.Context,
	service *arkchannel.Service) error {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return err
	}

	return c.cfg.Recovery.RestoreWatches(ctx, records)
}

// newNode composes native lnd state over one authenticated peer sender.
func (c *NativeArkChannelController) newNode(ctx context.Context,
	party arkchannel.Party, remoteKey *btcec.PublicKey,
	sender lnruntime.PeerEventSender, materializationDelta uint32) (
	*lnruntime.NativeNode, error) {

	transport, err := lnruntime.NewDurablePeerTransport(
		lnruntime.DurablePeerTransportConfig{
			Sender: sender,
			CorrelationKey: hex.EncodeToString(
				c.cfg.IdentityKey.PubKey.SerializeCompressed(),
			),
		},
	)
	if err != nil {
		return nil, err
	}
	log := c.cfg.Log
	if log == nil {
		log = btclog.Disabled
	}
	logCtx := context.WithoutCancel(ctx)
	onChannelFailure := func(channelID lnwire.ChannelID,
		scid lnwire.ShortChannelID,
		failure htlcswitch.LinkFailureError) {

		log.WarnS(logCtx, "Native Ark channel link failed",
			failure,
			btclog.Fmt("channel_id", "%x", channelID[:]),
			btclog.Fmt("scid", "%v", scid),
		)
	}
	//nolint:contextcheck // link callbacks use the controller lifecycle
	shouldDisableChannelAdds := func(channelPoint wire.OutPoint) (bool,
		error) {

		checkCtx, cancel := context.WithTimeout(
			c.lifecycleCtx, 5*time.Second,
		)
		defer cancel()
		record, err := c.coordinator.FindByChannelPoint(
			checkCtx, channelPoint,
		)
		if err != nil {
			return false, fmt.Errorf("load Ark channel refresh "+
				"lifecycle: %w", err)
		}

		if record.Snapshot.Phase == arkchannel.PhaseMaterializing {
			return true, nil
		}
		// The external hold has its own release barrier; don't also
		// acquire an ordinary flush that an unsigned abort can't undo.
		if shouldHoldArkChannelClose(record.Snapshot) {
			return false, nil
		}
		if record.Snapshot.Source == nil {
			return false, fmt.Errorf("channel source is missing")
		}
		height, _, err := c.cfg.ChainBackend.BestBlock(checkCtx)
		if err != nil {
			return false, err
		}

		return c.cfg.Recovery.SourceExpiring(
			checkCtx, record.Snapshot.Terms,
			*record.Snapshot.Source, height,
		)
	}

	//nolint:contextcheck // link callbacks use the controller lifecycle
	shouldHoldExternalClose := func(point wire.OutPoint) (bool, error) {
		checkCtx, cancel := context.WithTimeout(
			c.lifecycleCtx, 5*time.Second,
		)
		defer cancel()

		record, err := c.coordinator.FindByChannelPoint(checkCtx, point)
		if err != nil {
			return false, err
		}

		return shouldHoldArkChannelClose(record.Snapshot), nil
	}

	opener := c.cfg.OpenChannelDB
	if opener == nil {
		opener = db.OpenChannelDB
	}
	nodeCfg := lnruntime.NativeNodeConfig{
		Party: party,
		Chain: c.cfg.Wallet.BtcWallet, Notifier: c.cfg.ChainNotifier,
		WalletController: c.cfg.Wallet.BtcWallet,
		KeyRing:          c.cfg.Wallet.KeyRing(), Signer: c.
					cfg.
					Wallet.
					BtcWallet,
		FeeEstimator: c.cfg.FeeEstimator, NetParams: c.cfg.NetParams,
		IdentityKey: c.cfg.IdentityKey, BackingKey: c.keys.backing,
		RemoteNodeKey: remoteKey, Transport: transport,
		Intents: c.coordinator, OnChannelFailure: onChannelFailure,
		MaterializationDelta:     materializationDelta,
		ChannelLifecycle:         c,
		ShouldDisableChannelAdds: shouldDisableChannelAdds,
		ShouldHoldExternalClose:  shouldHoldExternalClose,
	}
	if c.nodeFactory != nil {
		return c.nodeFactory(nodeCfg)
	}
	channelDB, err := opener(c.cfg.ChannelDataDir)
	if err != nil {
		return nil, err
	}
	nodeCfg.DB, nodeCfg.OwnsDB = channelDB, true

	return lnruntime.NewNativeNode(nodeCfg)
}

// ChainWatchOwner reports whether lnd or the Ark lifecycle currently owns
// chain observation for a channel.
func (c *NativeArkChannelController) ChainWatchOwner(ctx context.Context,
	channel *chanstate.OpenChannel) (contractcourt.ChainWatchOwner, error) {

	record, err := c.coordinator.FindByChannelPoint(
		ctx, channel.FundingOutpoint,
	)
	if err != nil {
		return 0, fmt.Errorf("load Ark channel lifecycle: %w", err)
	}

	if shouldWatchArkChannel(record.Snapshot.Phase) {
		return contractcourt.ChainWatchOwnerLnd, nil
	}

	return contractcourt.ChainWatchOwnerAux, nil
}

// PrepareCommitmentPublish materializes the Ark backing before lnd publishes
// a commitment transaction.
func (c *NativeArkChannelController) PrepareCommitmentPublish(
	ctx context.Context, channelPoint wire.OutPoint) error {

	return c.materializeBeforeCommitment(ctx, channelPoint)
}

const arkChannelFinalizationRetry = time.Second

// WaitForChannelFinalization durably records lnd's terminal channel state.
// The Ark owner retains retry policy so lnd only provides the cancellation and
// ordering boundary.
func (c *NativeArkChannelController) WaitForChannelFinalization(
	ctx context.Context, channelPoint wire.OutPoint) error {

	for {
		err := c.recordFullyResolvedChannel(ctx, channelPoint)
		if err == nil {
			return nil
		}

		if c.cfg.Log != nil {
			c.cfg.Log.WarnS(ctx, "Unable to finalize Ark channel",
				err,
				btclog.Fmt("channel_point", "%v", channelPoint),
			)
		}

		timer := time.NewTimer(arkChannelFinalizationRetry)
		select {
		case <-timer.C:
			continue

		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		}
	}
}

// materializeBeforeCommitment blocks lnd's local commitment publication until
// either recovery-ready endpoint has durably published the exact backing.
func (c *NativeArkChannelController) materializeBeforeCommitment(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return fmt.Errorf("load channel publication lifecycle: %w", err)
	}
	if record.Snapshot.Phase == arkchannel.PhaseOnChain {
		return nil
	}
	if c.service == nil {
		return fmt.Errorf("Ark channel service is not ready")
	}
	record, err = c.service.Materialize(
		ctx, record.Snapshot.Terms.ID,
	)
	if err != nil {
		return fmt.Errorf("materialize channel backing: %w", err)
	}
	if record.Snapshot.Phase != arkchannel.PhaseOnChain {
		return fmt.Errorf("channel backing stopped in phase %s",
			record.Snapshot.Phase)
	}

	return nil
}

// shouldWatchArkChannel reports whether lnd must own the channel's chain and
// contract lifecycle. An unpublished outpoint remains dormant, while an
// observed publication can be handled without first contacting the peer.
func shouldWatchArkChannel(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// shouldHoldArkChannelClose preserves a released payout even when expiry
// has handed the competing backing to lnd. PhaseOnChain alone is not a hold.
func shouldHoldArkChannelClose(s arkchannel.Snapshot) bool {
	if s.CooperativeClose != nil {
		return true
	}
	switch s.Phase {
	case arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// recordFullyResolvedChannel advances the Ark FSM only after lnd has resolved
// and swept every output. Chain evidence can recover a peer that never
// received the initiating endpoint's materialization transition.
func (c *NativeArkChannelController) recordFullyResolvedChannel(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return err
	}
	switch record.Snapshot.Phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned:

		record, _, err = c.coordinator.Apply(
			ctx, record.Snapshot.Terms.ID,
			&arkchannel.BackingObserved{
				TxID: channelPoint.Hash,
			},
		)
		if err != nil {
			return err
		}

	case arkchannel.PhaseOnChain:
	case arkchannel.PhaseClosed:
		return nil

	default:
		return fmt.Errorf("cannot resolve on-chain channel from %s",
			record.Snapshot.Phase)
	}

	_, _, err = c.coordinator.Apply(
		ctx, record.Snapshot.Terms.ID, &arkchannel.ChannelClosed{},
	)

	return err
}

// newService binds the endpoint's native lnd and Ark side effects to one FSM.
func (c *NativeArkChannelController) newService(node *lnruntime.NativeNode,
	funding arkchannel.FundingCanceler,
	closer arkchannel.ChannelCooperativeCloser) (*arkchannel.Service,
	error) {

	var oor arkchannel.OORTransferController
	if c.cfg.OOR != nil {
		oor = c.cfg.OOR
	}
	var materializer arkchannel.ChannelMaterializer
	if c.cfg.Materializer != nil {
		materializer = c.cfg.Materializer
	}
	log := c.cfg.Log
	if log == nil {
		log = btclog.Disabled
	}
	forceCloser := &loggedArkChannelForceCloser{
		node: node, log: log,
	}
	executor, err := arkchannel.NewNativeExecutor(
		c.party, node.FundingActivator(), funding, oor, materializer,
		node, forceCloser, closer,
	)
	if err != nil {
		return nil, err
	}

	service, err := arkchannel.NewService(c.party, c.coordinator, executor)
	if err != nil {
		return nil, err
	}
	if err := c.cfg.Recovery.BindChannelEventSink(service); err != nil {
		return nil, err
	}

	return service, nil
}

// PromoteVTXO prepares and activates one client-funded OOR channel.
func (c *NativeArkChannelController) PromoteVTXO(ctx context.Context,
	amount btcutil.Amount, idempotencyKey string) (arkchannel.Record,
	error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, fmt.Errorf("only a client can " +
			"promote VTXO liquidity")
	}
	if amount <= 0 {
		return arkchannel.Record{}, fmt.Errorf("channel amount must " +
			"be positive")
	}
	if idempotencyKey == "" {
		return arkchannel.Record{}, fmt.Errorf("idempotency key is " +
			"required")
	}
	if len(idempotencyKey) > maxArkChannelIdempotencyKeyLength {
		return arkchannel.Record{}, fmt.Errorf("idempotency key "+
			"exceeds %d bytes", maxArkChannelIdempotencyKeyLength)
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, err
	}
	terms, existing, err := c.newPromotionTerms(
		ctx, amount, idempotencyKey,
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if existing != nil && existing.Snapshot.Source != nil {
		if _, err := c.resumeBoundPromotion(
			ctx, *existing,
		); err != nil {
			return arkchannel.Record{}, err
		}

		return c.waitPromotionCompletion(ctx, terms.ID)
	}
	if _, err := c.remote.RegisterPromotion(ctx, terms); err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.service.RegisterPromotion(ctx, terms); err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.service.StartOORPreparation(ctx, terms.ID); err != nil {
		return arkchannel.Record{}, err
	}
	binding, err := c.cfg.PrepareOOR(
		ctx, terms, arkchannel.DefaultBackingFee,
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.remote.BindPreparedOOR(
		ctx, terms.ID, binding,
	); err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.service.BindPreparedOOR(
		ctx, terms.ID, binding,
	); err != nil {
		return arkchannel.Record{}, err
	}

	return c.waitPromotionCompletion(ctx, terms.ID)
}

// waitPromotionCompletion preserves the blocking promotion contract across
// idempotent retries while the durable channel FSM finishes asynchronously.
func (c *NativeArkChannelController) waitPromotionCompletion(
	ctx context.Context, id arkchannel.ID) (arkchannel.Record, error) {

	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()

	for {
		record, err := c.service.GetChannel(ctx, id)
		if err != nil {
			return arkchannel.Record{}, err
		}
		switch record.Snapshot.Phase {
		case arkchannel.PhaseActive:
			return record, nil

		case arkchannel.PhaseFailed:
			return arkchannel.Record{}, fmt.Errorf("channel "+
				"promotion failed: %s", record.Snapshot.Failure)

		case arkchannel.PhaseRequested,
			arkchannel.PhaseNegotiating,
			arkchannel.PhaseBackingReady,
			arkchannel.PhaseActivating,
			arkchannel.PhaseCancelling:

			// A peer callback or replayable channel action still
			// owns the next durable transition.

		default:
			// Preserve idempotent lookup after the channel has
			// moved beyond its active creation lifecycle.
			return record, nil
		}

		select {
		case <-ctx.Done():
			return arkchannel.Record{}, ctx.Err()

		case <-ticker.C:
		}
	}
}

// resumeBoundPromotion resumes only channel-creation work. A later channel
// phase must be reported as-is instead of revalidating the finalized OOR.
func (c *NativeArkChannelController) resumeBoundPromotion(ctx context.Context,
	record arkchannel.Record) (arkchannel.Record, error) {

	switch record.Snapshot.Phase {
	case arkchannel.PhaseNegotiating, arkchannel.PhaseBackingReady,
		arkchannel.PhaseActivating, arkchannel.PhaseCancelling:
		return c.service.ResumeChannelAction(
			ctx, record.Snapshot.Terms.ID,
		)

	default:
		return record, nil
	}
}

// newPromotionTerms creates stable protocol identifiers and binds every key
// role to the two endpoint wallets.
func (c *NativeArkChannelController) newPromotionTerms(ctx context.Context,
	amount btcutil.Amount, idempotencyKey string) (arkchannel.Terms,
	*arkchannel.Record, error) {

	if idempotencyKey == "" {
		return arkchannel.Terms{}, nil, fmt.Errorf("idempotency key " +
			"is required")
	}
	id, pending, scid := c.promotionIdentifiers(idempotencyKey)

	record, err := c.coordinator.Get(ctx, id)
	if err == nil {
		terms := record.Snapshot.Terms
		info := fundingPeerFromTerms(terms)
		expected, err := c.newClientFundedTerms(
			id, pending, scid, amount, info,
		)
		if err != nil {
			return arkchannel.Terms{}, nil, err
		}
		if terms != expected ||
			terms.HubNodeKey != c.clientPeer.nodeKey {
			return arkchannel.Terms{}, nil, fmt.Errorf(
				"idempotency key is already bound to " +
					"different channel terms")
		}

		return terms, &record, nil
	}
	if !errors.Is(err, arkchannel.ErrNotFound) {
		return arkchannel.Terms{}, nil, err
	}
	info, err := c.clientPeer.promotionPolicy(ctx, c.remote)
	if err != nil {
		return arkchannel.Terms{}, nil, err
	}
	terms, err := c.newClientFundedTerms(id, pending, scid, amount, info)

	return terms, nil, err
}

// promotionIdentifiers derives every retry-sensitive protocol identifier from
// the daemon identity and caller-provided idempotency key.
func (c *NativeArkChannelController) promotionIdentifiers(
	idempotencyKey string) (arkchannel.ID, [32]byte, uint64) {

	derive := func(label string) [32]byte {
		hash := sha256.New()
		_, _ = hash.Write(
			[]byte("wavelength/ark-channel/promotion/v1/"),
		)
		_, _ = hash.Write([]byte(label))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(
			c.cfg.IdentityKey.PubKey.SerializeCompressed(),
		)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(idempotencyKey))
		var result [32]byte
		copy(result[:], hash.Sum(nil))

		return result
	}
	id := arkchannel.ID(derive("channel-id"))
	pending := derive("pending-channel-id")
	scidSeed := derive("reserved-scid")
	blockHeight := uint32(scidSeed[0])<<16 |
		uint32(scidSeed[1])<<8 | uint32(scidSeed[2])
	txIndex := uint32(scidSeed[3])<<16 |
		uint32(scidSeed[4])<<8 | uint32(scidSeed[5])
	if blockHeight == 0 {
		blockHeight = 1
	}
	if txIndex == 0 {
		txIndex = 1
	}
	scid := lnwire.ShortChannelID{
		BlockHeight: blockHeight, TxIndex: txIndex,
		TxPosition: 0,
	}.ToUint64()

	return id, pending, scid
}

// newClientFundedTerms binds deterministic or random protocol identifiers to
// this endpoint's fixed channel and Ark keys.
func (c *NativeArkChannelController) newClientFundedTerms(id arkchannel.ID,
	pending [32]byte, scid uint64, amount btcutil.Amount,
	info lnruntime.FundingPeerInfo) (arkchannel.Terms, error) {

	terms := arkchannel.Terms{
		ID:               id,
		Kind:             arkchannel.KindPromotion,
		Funder:           arkchannel.PartyClient,
		PendingChannelID: pending, ReservedSCID: scid,
		Capacity: amount,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: info.ChannelDelay,
			FunderDelay:  info.FunderDelay,
			MinExitDelay: info.MinimumExitDelay,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = info.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubArkKey = info.HubArkKey
	terms.VTXO.ArkOperatorKey = info.ArkOperatorKey
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubChannelKey = info.HubChannelKey
	copy(
		terms.VTXO.FunderKey[:],
		c.keys.funder.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// PayLightningInvoice asks the hub to dispatch one public invoice only after
// this endpoint's private same-hash HTLC is held.
func (c *NativeArkChannelController) PayLightningInvoice(ctx context.Context,
	paymentRequest string, maxFee btcutil.Amount) (LightningPaymentResult,
	error) {

	if c.party != arkchannel.PartyClient {
		return LightningPaymentResult{}, fmt.Errorf("public payment " +
			"is available only on the client endpoint")
	}
	if paymentRequest == "" || maxFee < 0 {
		return LightningPaymentResult{}, fmt.Errorf("valid payment " +
			"request and maximum fee are required")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return LightningPaymentResult{}, err
	}
	preparation, err := c.paymentPeer.PrepareOutgoingPayment(
		ctx, paymentRequest, maxFee,
	)
	if err != nil {
		return LightningPaymentResult{}, err
	}
	record, err := c.service.GetChannel(ctx, preparation.ChannelID)
	if err != nil {
		return LightningPaymentResult{}, c.cancelOutgoingPayment(
			ctx, preparation.PaymentHash, err,
		)
	}
	if record.Snapshot.Terms.ReservedSCID != preparation.ReservedSCID {
		err := fmt.Errorf("payment preparation changed channel SCID")

		return LightningPaymentResult{}, c.cancelOutgoingPayment(
			ctx, preparation.PaymentHash, err,
		)
	}
	preimage, err := c.node.PayInvoiceResult(
		ctx, record, preparation.PaymentHash, preparation.PrivateAmount,
	)
	if err != nil {
		return LightningPaymentResult{}, c.cancelOutgoingPayment(
			ctx, preparation.PaymentHash, err,
		)
	}
	if preimage.Hash() != preparation.PaymentHash {
		err := fmt.Errorf("payment preimage does not match preparation")

		return LightningPaymentResult{}, c.cancelOutgoingPayment(
			ctx, preparation.PaymentHash, err,
		)
	}

	return LightningPaymentResult{
		PaymentHash: preparation.PaymentHash, Preimage: preimage,
		PrivateAmount: preparation.PrivateAmount, Fee: preparation.Fee,
		ChannelID: preparation.ChannelID,
	}, nil
}

// cancelOutgoingPayment releases a prepared public-payment reservation even
// when the request that detected the failure has already been canceled.
func (c *NativeArkChannelController) cancelOutgoingPayment(ctx context.Context,
	hash lntypes.Hash, cause error) error {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), arkChannelPaymentCleanupTimeout,
	)
	defer cancel()

	err := c.paymentPeer.CancelOutgoingPayment(
		cleanupCtx, hash, cause.Error(),
	)
	if err != nil {
		return errors.Join(
			cause, fmt.Errorf("cancel outgoing payment: %w", err),
		)
	}

	return cause
}

// PrepareIncomingPayment installs the known-preimage native invoice before a
// route hint can be exposed to an external payer.
func (c *NativeArkChannelController) PrepareIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	if c.party != arkchannel.PartyClient {
		return fmt.Errorf("incoming payment preparation is client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return err
	}

	return c.AddInvoiceWithPreimage(ctx, preimage, amount)
}

// RegisterIncomingPayment binds the advertised future SCID to this
// authenticated endpoint after the private invoice is durable. It returns the
// public CLTV delta needed to safely nest the private channel payment.
func (c *NativeArkChannelController) RegisterIncomingPayment(
	ctx context.Context, hash lntypes.Hash, amount btcutil.Amount,
	reservedSCID uint64) (arkchannel.ID, uint32, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.ID{}, 0, fmt.Errorf("incoming payment " +
			"registration is client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.ID{}, 0, err
	}
	id, err := c.paymentPeer.RegisterIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
	if err != nil {
		return arkchannel.ID{}, 0, err
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.ID{}, 0, err
	}
	if record.Snapshot.Phase != arkchannel.PhaseActive {
		return arkchannel.ID{}, 0, fmt.Errorf("registered channel is " +
			"not active")
	}
	delta, err := lnruntime.ArkChannelReceiveCLTVDelta(
		record.Snapshot.Terms.VTXO,
	)

	return id, delta, err
}

// WaitIncomingPayment waits for the private hold invoice and returns the active
// channel that delivered it without releasing the shared preimage.
func (c *NativeArkChannelController) WaitIncomingPayment(ctx context.Context,
	hash lntypes.Hash) (arkchannel.ID, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.ID{}, fmt.Errorf("incoming payment wait is " +
			"client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.ID{}, err
	}

	incomingSCID, err := c.node.WaitInvoiceAcceptedChannel(ctx, hash)
	if err != nil {
		return arkchannel.ID{}, err
	}

	record, err := c.activeChannelBySCID(ctx, incomingSCID)
	if err != nil {
		return arkchannel.ID{}, err
	}

	return record.Snapshot.Terms.ID, nil
}

// activeChannelBySCID binds an accepted native invoice HTLC to one durable
// active channel instead of trusting the public route reservation.
func (c *NativeArkChannelController) activeChannelBySCID(ctx context.Context,
	scid lnwire.ShortChannelID) (arkchannel.Record, error) {

	if scid == (lnwire.ShortChannelID{}) {
		return arkchannel.Record{}, fmt.Errorf("incoming payment " +
			"channel SCID is empty")
	}
	record, err := c.cfg.Store.GetBySCID(ctx, scid.ToUint64())
	if err != nil {
		return arkchannel.Record{}, err
	}
	if record.Snapshot.Phase != arkchannel.PhaseActive {
		return arkchannel.Record{}, fmt.Errorf("incoming channel is " +
			"not active")
	}

	return record, nil
}

// SettleIncomingPayment releases the private hold invoice after the SDK
// durably records acceptance on the registered channel.
func (c *NativeArkChannelController) SettleIncomingPayment(ctx context.Context,
	preimage lntypes.Preimage) error {

	if c.party != arkchannel.PartyClient {
		return fmt.Errorf("incoming payment settlement is client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return err
	}

	return c.SettleHoldInvoice(ctx, preimage)
}

// CancelIncomingPayment cancels the registration and its private hold invoice.
func (c *NativeArkChannelController) CancelIncomingPayment(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	if c.party != arkchannel.PartyClient {
		return fmt.Errorf("incoming payment cancellation is client " +
			"only")
	}
	if reason == "" {
		return fmt.Errorf("incoming payment cancellation reason is " +
			"required")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return err
	}
	if err := c.paymentPeer.CancelIncomingPayment(
		ctx, hash, reason,
	); err != nil {
		return fmt.Errorf("cancel hub incoming payment: %w", err)
	}
	if err := c.CancelInvoice(ctx, hash); err != nil {
		return fmt.Errorf("cancel incoming hold invoice: %w", err)
	}

	return nil
}

// MaterializeAndForceClose asks lnd to force close. Its blocking publication
// barrier materializes the client-funded backing first, while the peer learns
// the lifecycle only from its already armed chain watcher.
func (c *NativeArkChannelController) MaterializeAndForceClose(
	ctx context.Context, id arkchannel.ID) (arkchannel.Record,
	chainhash.Hash, chainhash.Hash, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, chainhash.Hash{}, chainhash.Hash{},
			fmt.Errorf("only a client can request force close")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, chainhash.Hash{},
			chainhash.Hash{}, err
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.Record{}, chainhash.Hash{},
			chainhash.Hash{}, err
	}
	if record.Snapshot.Backing == nil {
		return arkchannel.Record{}, chainhash.Hash{}, chainhash.Hash{},
			fmt.Errorf("materialized channel backing is missing")
	}
	backing := record.Snapshot.Backing.Clone()
	closeTx, forceErr := c.node.ForceCloseChannel(backing.ChannelPoint)
	if forceErr == nil {
		if closeTx == nil {
			return arkchannel.Record{}, chainhash.Hash{},
				chainhash.Hash{}, fmt.Errorf("lnd returned " +
					"no force-close transaction")
		}
	}

	// Both endpoints watch the unpublished channel point before Ark
	// materializes it. Either commitment may therefore win publication,
	// even when the local ForceCloseChannel call itself succeeds. Return
	// only the transaction lnd durably classified as the channel close.
	closeTxID, waitErr := c.node.WaitForceCloseResult(
		ctx, backing.ChannelPoint,
	)
	if waitErr != nil {
		if forceErr != nil {
			return arkchannel.Record{}, chainhash.Hash{},
				chainhash.Hash{}, fmt.Errorf("force close "+
					"channel: %w; reconcile peer close: %v",
					forceErr, waitErr)
		}

		return arkchannel.Record{}, chainhash.Hash{}, chainhash.Hash{},
			fmt.Errorf("reconcile force close: %w", waitErr)
	}
	record, err = c.service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.Record{}, chainhash.Hash{},
			chainhash.Hash{}, err
	}

	return record, backing.ChannelPoint.Hash, closeTxID, nil
}

// RefreshChannel starts the client-owned 3-of-3 in-Ark refresh process.
func (c *NativeArkChannelController) RefreshChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, fmt.Errorf("only a client can " +
			"refresh a channel in Ark")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, err
	}

	return c.clientClose.RequestCooperativeClose(ctx, id)
}

// GetChannel returns the endpoint's durable Ark channel record.
func (c *NativeArkChannelController) GetChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	return c.coordinator.Get(ctx, id)
}

// ListChannels returns every channel that still needs recovery, observation,
// or operator action without requiring the native endpoint to be online.
func (c *NativeArkChannelController) ListChannels(ctx context.Context) (
	[]arkchannel.Record, error) {

	return c.coordinator.ListNonTerminal(ctx)
}

// ChannelBalance returns lnd's authoritative active-channel balances.
func (c *NativeArkChannelController) ChannelBalance(ctx context.Context,
	id arkchannel.ID) (btcutil.Amount, btcutil.Amount, error) {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return 0, 0, err
		}
	}
	c.mu.RLock()
	node := c.node
	c.mu.RUnlock()
	if node == nil {
		return 0, 0, fmt.Errorf("native Ark channel endpoint is not " +
			"ready")
	}
	record, err := c.coordinator.Get(ctx, id)
	if err != nil {
		return 0, 0, err
	}

	return node.ChannelBalance(record)
}

// SelectActiveChannel finds an ordinary native channel with enough balance on
// the requested local or remote side. lnd remains authoritative and may still
// reject a racing payment that consumes the same balance.
func (c *NativeArkChannelController) SelectActiveChannel(ctx context.Context,
	amount btcutil.Amount, localSends bool) (arkchannel.Record, error) {

	if amount <= 0 {
		return arkchannel.Record{}, fmt.Errorf("payment amount must " +
			"be positive")
	}
	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return arkchannel.Record{}, err
		}
	}
	c.mu.RLock()
	service := c.service
	node := c.node
	c.mu.RUnlock()
	if service == nil || node == nil {
		return arkchannel.Record{}, fmt.Errorf("native Ark channel " +
			"endpoint is not ready")
	}
	records, err := service.ListChannels(ctx)
	if err != nil {
		return arkchannel.Record{}, err
	}
	for _, record := range records {
		if record.Snapshot.Phase != arkchannel.PhaseActive {
			continue
		}
		local, remote, err := node.ChannelBalance(record)
		if err != nil {
			continue
		}
		available := remote
		if localSends {
			available = local
		}
		if available >= amount {
			return record, nil
		}
	}

	return arkchannel.Record{}, fmt.Errorf("%w for %d sat",
		ErrInsufficientArkChannelLiquidity, amount)
}

// AddHoldInvoice registers the private source for an outgoing bridge.
func (c *NativeArkChannelController) AddHoldInvoice(ctx context.Context,
	hash lntypes.Hash, amount btcutil.Amount) error {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return err
		}
	}

	return c.node.AddHoldInvoice(ctx, hash, amount)
}

// AddInvoiceWithPreimage registers the private hold destination for an incoming
// bridge before its BOLT 11 invoice is exposed to a payer.
func (c *NativeArkChannelController) AddInvoiceWithPreimage(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return err
		}
	}
	_, err := c.node.AddInvoiceWithPreimage(
		ctx, amount, preimage, true,
	)

	return err
}

// WaitInvoiceAccepted waits for a private hold invoice HTLC.
func (c *NativeArkChannelController) WaitInvoiceAccepted(ctx context.Context,
	hash lntypes.Hash) error {

	return c.node.WaitInvoiceAccepted(ctx, hash)
}

// AcceptedInvoiceDeadline derives the public dispatch ceiling from the
// accepted native HTLC, not an invoice preference or a bridge replay row.
func (c *NativeArkChannelController) AcceptedInvoiceDeadline(
	ctx context.Context, id arkchannel.ID, hash lntypes.Hash,
	amount btcutil.Amount) (uint32, error) {

	c.mu.RLock()
	service, node := c.service, c.node
	c.mu.RUnlock()
	if service == nil || node == nil {
		return 0, fmt.Errorf("native channel endpoint is not ready")
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return 0, err
	}

	return node.AcceptedInvoiceDeadline(ctx, record, hash, amount)
}

// WaitInvoiceSettled waits for an incoming private destination to settle.
func (c *NativeArkChannelController) WaitInvoiceSettled(ctx context.Context,
	hash lntypes.Hash) error {

	return c.node.WaitInvoiceSettled(ctx, hash)
}

// SettleHoldInvoice releases a private hold invoice with its shared preimage.
func (c *NativeArkChannelController) SettleHoldInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	return c.node.SettleHoldInvoice(ctx, preimage)
}

// CancelInvoice fails a private hold invoice before its preimage is released.
func (c *NativeArkChannelController) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	return c.node.CancelInvoice(ctx, hash)
}

// LookupChannelPayment reads the durable result without starting the endpoint
// or requiring the original channel to still have an active link.
func (c *NativeArkChannelController) LookupChannelPayment(ctx context.Context,
	id arkchannel.ID, hash lntypes.Hash, amount btcutil.Amount) (
	lntypes.Preimage, bool, error) {

	c.mu.RLock()
	service, node := c.service, c.node
	c.mu.RUnlock()
	if service == nil || node == nil {
		return lntypes.Preimage{}, false, fmt.Errorf("native channel " +
			"endpoint is not ready")
	}
	record, err := service.GetChannel(ctx, id)
	if err != nil {
		return lntypes.Preimage{}, false, err
	}

	return node.LookupChannelPayment(ctx, record, hash, amount)
}

// PayHash sends or resumes one same-hash payment over a selected private
// channel and returns the destination preimage. A new attempt must expire no
// later than maxExpiryHeight.
func (c *NativeArkChannelController) PayHash(ctx context.Context,
	id arkchannel.ID, hash lntypes.Hash, amount btcutil.Amount,
	maxExpiryHeight uint32) (lntypes.Preimage, error) {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return lntypes.Preimage{}, err
		}
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return lntypes.Preimage{}, err
	}

	return c.node.PayInvoiceResultBefore(
		ctx, record, hash, amount, maxExpiryHeight,
	)
}

// PeerMessageHandler dispatches authenticated BOLT messages into native lnd.
//
//nolint:ll // The concrete method name and interface type are both significant.
func (c *NativeArkChannelController) PeerMessageHandler() lnruntime.PeerEventHandler {
	return func(ctx context.Context, message lnwire.Message) error {
		if c.party == arkchannel.PartyClient {
			if err := c.ensureClientStarted(ctx); err != nil {
				return fmt.Errorf("start native Ark channel "+
					"endpoint: %w", err)
			}
		}
		log := c.cfg.Log
		if log == nil {
			log = btclog.Disabled
		}
		log.DebugS(ctx, "Dispatching native Ark channel peer message",
			btclog.Fmt("party", "%s", c.party),
			btclog.Fmt("message_type", "%s", message.MsgType()),
		)
		c.mu.RLock()
		node := c.node
		c.mu.RUnlock()
		if node == nil {
			return fmt.Errorf("native Ark channel endpoint is " +
				"not ready")
		}

		return node.PeerMessageHandler()(ctx, message)
	}
}

// FundingPeerService returns the generated funding protocol for a hub
// controller bound to its authenticated client.
func (c *NativeArkChannelController) FundingPeerService(remoteNode [33]byte) (
	arkchannelrpc.ArkChannelFundingPeerServiceMailboxServer, error) {

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.party != arkchannel.PartyHub || c.node == nil || c.service == nil {
		return nil, fmt.Errorf("hub Ark channel endpoint is not ready")
	}

	return lnruntime.NewFundingPeerRPCServer(
		lnruntime.FundingPeerRPCServerConfig{
			RemoteNode: remoteNode, Info: c.peerInfo,
			Service: c.service, Funding: c.node.FundingEndpoint(),
			Node: c.node, Recovery: c.cfg.Recovery,
			Bridge: c.paymentBridge,
		},
	)
}

// CooperativeClosePeerService returns the generated close protocol for a hub
// controller bound to its authenticated client.
func (c *NativeArkChannelController) CooperativeClosePeerService(
	remoteNode [33]byte) (arkchannelrpc.ArkChannelPeerServiceMailboxServer,
	error) {

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.party != arkchannel.PartyHub || c.hubClose == nil {
		return nil, fmt.Errorf("hub cooperative close endpoint is " +
			"not ready")
	}

	return lnruntime.NewCooperativeClosePeerRPCServer(
		remoteNode, c.hubClose,
	)
}

// Quiesce cancels and joins controller workers without releasing the native
// node, Ark service, recovery controller, or stores. Shared-engine shutdown
// must retain these objects until lnd has finished its lifecycle callbacks.
func (c *NativeArkChannelController) Quiesce() {
	c.mu.Lock()
	c.stopped = true
	cancel := c.lifecycleCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.lifecycleWG.Wait()
}

// Retire joins all native callbacks before releasing a terminal hub session.
// Do not retire on transport loss: a disconnected peer can still own funds.
func (c *NativeArkChannelController) Retire() error {
	c.Quiesce()
	c.mu.RLock()
	node := c.node
	c.mu.RUnlock()
	var err error
	if node != nil {
		err = node.Retire()
	}

	return errors.Join(err, c.Stop())
}

// Stop releases native lnd state before the owning wallet and database stop.
func (c *NativeArkChannelController) Stop() error {
	c.stopOnce.Do(func() {
		c.Quiesce()

		c.mu.Lock()
		node := c.node
		c.mu.Unlock()

		if node != nil {
			c.stopErr = node.Stop()
		}
		// Standalone clients also invoke auxiliary callbacks while
		// stopping their native engine. Release its references only
		// after that engine has joined.
		c.mu.Lock()
		c.node = nil
		c.service = nil
		c.clientClose = nil
		c.hubClose = nil
		c.mu.Unlock()
		if c.cfg.Recovery != nil {
			c.cfg.Recovery.Stop()
		}
	})

	return c.stopErr
}

// arkChannelRefreshDelivery returns the ordinary Ark account key that owns
// this endpoint's replacement VTXO after an in-Ark refresh.
type arkChannelRefreshDelivery struct {
	owner          *btcec.PublicKey
	minimum        btcutil.Amount
	maximum        btcutil.Amount
	policySource   func() *types.OperatorTerms
	coordinator    *arkchannel.Coordinator
	registerPolicy func(context.Context) (*types.OperatorTerms, error)
}

// refreshDelivery binds payout limits without making current client policy a
// prerequisite for restoring unilateral recovery.
func (c *NativeArkChannelController) refreshDelivery() (
	*arkChannelRefreshDelivery, error) {

	terms := c.cfg.OperatorTerms
	d := &arkChannelRefreshDelivery{
		owner:          c.cfg.OORDestination,
		policySource:   c.cfg.OperatorTermsSource,
		coordinator:    c.coordinator,
		registerPolicy: c.cfg.RegisterRefreshPolicy,
	}
	if terms == nil && d.policySource == nil {
		return nil, fmt.Errorf("refresh operator policy is unavailable")
	}
	if terms != nil {
		d.minimum = terms.MinVTXOAmountFloor()
		d.maximum = terms.MaxVTXOAmount
	}

	return d, nil
}

// ValidateCooperativeCloseOutputs rejects known operator-policy failures before
// either endpoint releases an irreversible authorization.
func (d *arkChannelRefreshDelivery) ValidateCooperativeCloseOutputs(
	proposal arkchannel.CooperativeCloseProposal) error {

	minimum, maximum := d.minimum, d.maximum
	if d.policySource != nil {
		terms := d.policySource()
		if terms == nil {
			return fmt.Errorf("refresh operator policy is " +
				"unavailable")
		}
		minimum = terms.MinVTXOAmountFloor()
		maximum = terms.MaxVTXOAmount
	}
	for _, amount := range []btcutil.Amount{
		proposal.ClientOutput, proposal.HubOutput,
	} {
		if amount == 0 {
			continue
		}
		if amount < minimum || (maximum > 0 && amount > maximum) {
			return fmt.Errorf("refresh payout %d outside limits",
				amount)
		}
	}

	return nil
}

// CooperativeCloseDelivery fixes the policy that the local wallet actually
// registered. Keep the original owner-only encoding while policies match.
func (d *arkChannelRefreshDelivery) CooperativeCloseDelivery(
	ctx context.Context, id arkchannel.ID) ([]byte, error) {

	if d == nil || d.owner == nil {
		return nil, fmt.Errorf("channel refresh OOR owner is required")
	}

	if d.registerPolicy == nil || d.coordinator == nil {
		return nil, fmt.Errorf("refresh registration is unavailable")
	}
	record, err := d.coordinator.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	current, err := d.registerPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || current.PubKey == nil ||
		current.VTXOExitDelay == 0 {
		return nil, fmt.Errorf("registered refresh policy is " +
			"incomplete")
	}
	source := record.Snapshot.Terms.VTXO
	if bytes.Equal(
		source.ArkOperatorKey[:], current.PubKey.SerializeCompressed(),
	) &&
		source.MinExitDelay == current.VTXOExitDelay {
		return d.owner.SerializeCompressed(), nil
	}

	return arkscript.EncodeStandardVTXOTemplate(
		d.owner, current.PubKey, current.VTXOExitDelay,
	)
}

// ValidateCooperativeCloseDelivery proves the replacement VTXO is assigned to
// this endpoint's ordinary Ark account.
func (d *arkChannelRefreshDelivery) ValidateCooperativeCloseDelivery(
	_ context.Context, _ arkchannel.ID, delivery []byte) error {

	owner, err := arkchannel.CooperativeCloseDeliveryOwner(delivery)
	if err != nil {
		return err
	}
	if d == nil || d.owner == nil || !sameXOnlyPubKey(d.owner, owner) {
		return fmt.Errorf("channel refresh owner is not the " +
			"configured Ark account")
	}

	return nil
}

//nolint:ll // Keep the complete delivery contract explicit for API audits.
var (
	_ ArkChannelController                        = (*NativeArkChannelController)(nil)
	_ lnruntime.CooperativeCloseDeliverySource    = (*arkChannelRefreshDelivery)(nil)
	_ lnruntime.CooperativeCloseDeliveryValidator = (*arkChannelRefreshDelivery)(nil)
)
