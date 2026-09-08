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
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrInsufficientArkChannelLiquidity means no active private channel
	// has enough balance on the requested sending side.
	ErrInsufficientArkChannelLiquidity = fmt.Errorf("insufficient active " +
		"Ark channel liquidity")

	// ErrReceiveChannelFallback means channel creation was abandoned before
	// the hub's prepared OOR crossed its signing point of no return.
	ErrReceiveChannelFallback = fmt.Errorf("receive channel can safely " +
		"fall back to vHTLC")
)

const (
	arkChannelArkKeyFamily            keychain.KeyFamily = 220
	arkChannelBackingKeyFamily        keychain.KeyFamily = 221
	arkChannelFunderKeyFamily         keychain.KeyFamily = 223
	arkChannelPaymentCleanupTimeout                      = 30 * time.Second
	arkChannelReceiveCleanupTimeout                      = 30 * time.Second
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
}

// NativeArkChannelController owns one Ark FSM and one modular lnd endpoint.
type NativeArkChannelController struct {
	party arkchannel.Party
	cfg   ArkChannelControllerConfig

	coordinator *arkchannel.Coordinator
	remote      lnruntime.ProcessFundingPeer
	fundingPeer lnruntime.FundingCounterparty
	paymentPeer lnruntime.ProcessPaymentPeer
	peerInfo    lnruntime.FundingPeerInfo
	remoteNode  [33]byte
	keys        nativeArkChannelKeys

	mu            sync.RWMutex
	node          *lnruntime.NativeNode
	service       *arkchannel.Service
	clientClose   *lnruntime.ClientCooperativeCloseProcess
	hubClose      *lnruntime.HubCooperativeCloseProcess
	fundingWire   *lnruntime.FundingWire
	paymentBridge lnruntime.PaymentBridgeCoordinator
	startAttempt  *arkChannelStartAttempt
	stopped       bool

	lifecycleCtx    context.Context //nolint:containedctx // process owned
	lifecycleCancel context.CancelFunc
	lifecycleWG     sync.WaitGroup
	workersOnce     sync.Once
	stopOnce        sync.Once
	stopErr         error

	clientStarter func(context.Context) error
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

// ResumeForceCloseChannel records entry and completion around lnd's durable
// commitment-publication edge.
func (c *loggedArkChannelForceCloser) ResumeForceCloseChannel(
	channelPoint wire.OutPoint) error {

	ctx := context.Background()
	c.log.InfoS(ctx, "Resuming Ark channel force close",
		btclog.Fmt("channel_point", "%v", channelPoint),
	)
	err := c.node.ResumeForceCloseChannel(channelPoint)
	if err != nil {
		c.log.WarnS(ctx, "Ark channel force close failed",
			err,
			btclog.Fmt("channel_point", "%v", channelPoint),
		)

		return err
	}
	c.log.InfoS(ctx, "Ark channel force close resumed",
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

// ArkChannelPaymentResult reports the payment hash and terminal state observed
// in lnd's authoritative payment or invoice store.
type ArkChannelPaymentResult struct {
	PaymentHash lntypes.Hash
	Settled     bool
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
	keys, err := deriveNativeArkChannelKeys(ctx, cfg)
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
	copy(info.HubFunderKey[:], keys.funder.PubKey.SerializeCompressed())
	copy(
		info.ArkOperatorKey[:],
		cfg.OperatorTerms.PubKey.SerializeCompressed(),
	)

	return info, info.Validate()
}

// NewClientArkChannelController constructs a lazy client endpoint. The first
// lifecycle request loads hub policy over the already-running mailbox and
// starts the native lnd components.
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
	keys, err := deriveNativeArkChannelKeys(ctx, cfg)
	if err != nil {
		return nil, err
	}

	controller := &NativeArkChannelController{
		party: arkchannel.PartyClient, cfg: cfg,
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
	if cfg.Process.FundingOOR == nil || cfg.Process.PrepareOOR == nil ||
		cfg.Process.LookupOOR == nil ||
		cfg.Process.ReserveReceiveCapital == nil {
		return nil, fmt.Errorf("complete hub OOR and capital control " +
			"is required")
	}
	if _, ok := cfg.Process.FundingOOR.(prePONRResultController); !ok {
		return nil, fmt.Errorf("result-bearing hub OOR controller is " +
			"required")
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
	keys, err := deriveNativeArkChannelKeys(ctx, cfg.Process)
	if err != nil {
		return nil, err
	}
	controller := &NativeArkChannelController{
		party: arkchannel.PartyHub, cfg: cfg.Process,
		coordinator: coordinator, peerInfo: cfg.Info, keys: keys,
		paymentBridge: cfg.PaymentBridge, remoteNode: cfg.RemoteNode,
	}
	controller.initLifecycle(ctx)
	if err := controller.startHub(
		ctx, cfg.RemoteNode, cfg.PeerSender, cfg.CloseObserver,
		cfg.CloseDefender,
	); err != nil {

		controller.lifecycleCancel()

		return nil, err
	}
	//nolint:contextcheck // controller owns its process-lifetime context
	controller.Start()

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
	cfg ArkChannelControllerConfig) (nativeArkChannelKeys, error) {

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
	funderKey, err := derive(arkChannelFunderKeyFamily)
	if err != nil {
		return nativeArkChannelKeys{}, err
	}

	return nativeArkChannelKeys{
		ark: arkKey, backing: backingKey, funder: funderKey,
	}, nil
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
//
//nolint:funlen // Composition keeps all provisional resources in one owner.
func (c *NativeArkChannelController) startClient(ctx context.Context) error {
	peerInfo, err := c.remote.GetPeerInfo(ctx)
	if err != nil {
		return fmt.Errorf("load Ark channel hub policy: %w", err)
	}
	remoteKey, err := btcec.ParsePubKey(peerInfo.HubNodeKey[:])
	if err != nil {
		return err
	}
	node, err := c.newNode(
		ctx, arkchannel.PartyClient, remoteKey, c.cfg.PeerSender,
	)
	if err != nil {
		return err
	}
	fundingWire, err := lnruntime.NewFundingWire(node.Peer())
	if err != nil {
		_ = node.Stop()

		return err
	}
	negotiator, err := node.NewNegotiator(c.fundingPeer, c.cfg.Recovery)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	delivery := newArkChannelRefreshDelivery(c.cfg.OORDestination)
	closeEndpoint, err := lnruntime.NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyClient, node.Runtime(), nil,
		keychain.KeyDescriptor{}, delivery,
	)
	if err != nil {
		fundingWire.Close()
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
		)
	})
	clientClose, err := lnruntime.NewClientCooperativeCloseProcess(
		closeEndpoint, c.cfg.Peer, publisher, delivery,
	)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	service, err := c.newService(node, negotiator, clientClose)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	if err := fundingWire.BindServer(lnruntime.FundingWireServerConfig{
		Service: service, Funding: node.FundingEndpoint(),
	}); err != nil {

		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		fundingWire.Close()
		_ = node.Stop()

		return fmt.Errorf("Ark channel controller is stopped")
	}
	c.service = service
	c.fundingWire = fundingWire
	c.mu.Unlock()
	cleanup := func() {
		fundingWire.Close()
		_ = node.Stop()

		c.mu.Lock()
		if c.service == service {
			c.service = nil
		}
		if c.fundingWire == fundingWire {
			c.fundingWire = nil
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
	if err := tolerateNativeArkChannelFailures(
		ctx,
		c.maintainPrePONRChannels(
			ctx, service, c.cfg.Clock.Now(),
		),
		c.cfg.Log, "Ark channel pre-PONR maintenance failed",
	); err != nil {

		cleanup()

		return err
	}
	if err := resumeNativeArkChannels(ctx, service, c.cfg.Log); err != nil {
		cleanup()

		return err
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, resumeOnchainArkChannels(ctx, node, service), c.cfg.Log,
		"Ark channel force-close resume failed",
	); err != nil {

		cleanup()

		return err
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		cleanup()

		return fmt.Errorf("Ark channel controller is stopped")
	}
	c.peerInfo = peerInfo
	c.node = node
	c.clientClose = clientClose
	c.mu.Unlock()

	return nil
}

// startHub starts one operator endpoint for an authenticated client.
func (c *NativeArkChannelController) startHub(ctx context.Context,
	remoteNode [33]byte, sender lnruntime.PeerEventSender,
	closeObserver lnruntime.CooperativeCloseObserver,
	closeDefender lnruntime.CooperativeCloseDefender) error {

	remoteKey, err := btcec.ParsePubKey(remoteNode[:])
	if err != nil {
		return err
	}
	node, err := c.newNode(
		ctx, arkchannel.PartyHub, remoteKey, sender,
	)
	if err != nil {
		return err
	}
	fundingWire, err := lnruntime.NewFundingWire(node.Peer())
	if err != nil {
		_ = node.Stop()

		return err
	}
	negotiator, err := node.NewNegotiator(
		fundingWire.Counterparty(), c.cfg.Recovery,
	)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	delivery := newArkChannelRefreshDelivery(c.cfg.OORDestination)
	closeEndpoint, err := lnruntime.NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyHub, node.Runtime(), c.cfg.Wallet.BtcWallet,
		c.keys.ark, delivery,
	)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	hubClose, err := lnruntime.NewHubCooperativeCloseProcess(
		closeEndpoint, delivery, closeObserver, closeDefender,
	)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	service, err := c.newService(
		node, negotiator, &lnruntime.HubCooperativeCloseExecutor{
			HubCooperativeCloseProcess: hubClose,
		},
	)
	if err != nil {
		fundingWire.Close()
		_ = node.Stop()

		return err
	}
	c.service = service
	c.fundingWire = fundingWire
	cleanup := func() {
		c.service = nil
		c.fundingWire = nil
		fundingWire.Close()
		_ = node.Stop()
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
	if err := tolerateNativeArkChannelFailures(
		ctx,
		c.maintainPrePONRChannels(
			ctx, service, c.cfg.Clock.Now(),
		),
		c.cfg.Log, "Ark channel pre-PONR maintenance failed",
	); err != nil {

		cleanup()

		return err
	}
	if err := resumeNativeArkChannels(ctx, service, c.cfg.Log); err != nil {
		cleanup()

		return err
	}
	if err := tolerateNativeArkChannelFailures(
		ctx, resumeOnchainArkChannels(ctx, node, service), c.cfg.Log,
		"Ark channel force-close resume failed",
	); err != nil {

		cleanup()

		return err
	}
	c.node = node
	c.hubClose = hubClose

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

// resumeNativeArkChannels logs isolated channel failures while preserving a
// functioning endpoint for every channel that recovered successfully.
func resumeNativeArkChannels(ctx context.Context, service *arkchannel.Service,
	log btclog.Logger) error {

	return tolerateNativeArkChannelFailures(
		ctx, service.Resume(ctx), log,
		"Ark channel action resume failed",
	)
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

// resumeOnchainArkChannels closes the crash window between durable backing
// publication and lnd's commitment-broadcast marker.
func resumeOnchainArkChannels(ctx context.Context,
	node arkChannelForceCloseResumer, service *arkchannel.Service) error {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return err
	}

	return resumeOnchainArkChannelRecords(node, records)
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
	sender lnruntime.PeerEventSender) (*lnruntime.NativeNode, error) {

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
	shouldDisableChannelAdds := func(channelPoint wire.OutPoint) (bool,
		error) {

		record, err := c.coordinator.FindByChannelPoint(
			logCtx, channelPoint,
		)
		if err != nil {
			return false, fmt.Errorf("load Ark channel refresh "+
				"lifecycle: %w", err)
		}

		return shouldRestoreArkChannelAddsDisabled(
			record.Snapshot.Phase,
		), nil
	}

	return lnruntime.NewNativeNode(lnruntime.NativeNodeConfig{
		DataDir: c.cfg.ChannelDataDir, Party: party,
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
		ChannelLifecycle:         c,
		ShouldDisableChannelAdds: shouldDisableChannelAdds,
	})
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

// shouldRestoreArkChannelAddsDisabled reports whether a durable cooperative
// close artifact requires a restored lnd link to remain quiesced.
func shouldRestoreArkChannelAddsDisabled(phase arkchannel.Phase) bool {
	switch phase {
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
	negotiator *lnruntime.ChannelNegotiator,
	closer arkchannel.ChannelCooperativeCloser) (*arkchannel.Service,
	error) {

	var oor arkchannel.OORTransferController
	if c.cfg.FundingOOR != nil {
		oor = c.cfg.FundingOOR
	} else if c.cfg.OOR != nil {
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
		c.party, node.FundingActivator(), negotiator, oor, materializer,
		node, forceCloser, closer,
	)
	if err != nil {
		return nil, err
	}

	return arkchannel.NewService(c.party, c.coordinator, executor)
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
	terms, err := c.newPromotionTerms(amount, idempotencyKey)
	if err != nil {
		return arkchannel.Record{}, err
	}
	existing, err := c.coordinator.Get(ctx, terms.ID)
	if err == nil && existing.Snapshot.Terms != terms {
		return arkchannel.Record{}, fmt.Errorf("idempotency key is " +
			"already bound to different channel terms")
	}
	if err != nil && !errors.Is(err, arkchannel.ErrNotFound) {
		return arkchannel.Record{}, err
	}
	if err == nil && existing.Snapshot.Source != nil {
		return c.resumeBoundPromotion(ctx, existing)
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

	return c.service.GetChannel(ctx, terms.ID)
}

// resumeBoundPromotion resumes only channel-creation work. A later channel
// phase must be reported as-is instead of revalidating the finalized OOR.
func (c *NativeArkChannelController) resumeBoundPromotion(ctx context.Context,
	record arkchannel.Record) (arkchannel.Record, error) {

	switch record.Snapshot.Phase {
	case arkchannel.PhaseRequested:
		_, err := c.remote.BindPreparedOOR(
			ctx, record.Snapshot.Terms.ID,
			record.Snapshot.Source.Clone(),
		)
		if err != nil {
			return arkchannel.Record{}, err
		}

		return c.service.ResumeChannelAction(
			ctx, record.Snapshot.Terms.ID,
		)

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
func (c *NativeArkChannelController) newPromotionTerms(amount btcutil.Amount,
	idempotencyKey string) (arkchannel.Terms, error) {

	if idempotencyKey == "" {
		return arkchannel.Terms{}, fmt.Errorf("idempotency key is " +
			"required")
	}
	id, pending, scid := c.promotionIdentifiers(idempotencyKey)

	return c.newClientFundedTerms(
		id, pending, scid, amount, arkchannel.KindPromotion,
		lntypes.Hash{},
	)
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
	kind arkchannel.Kind, paymentHash lntypes.Hash) (arkchannel.Terms,
	error) {

	terms := arkchannel.Terms{
		ID: id, Kind: kind, Funder: arkchannel.PartyClient,
		PendingChannelID: pending, ReservedSCID: scid,
		Capacity: amount, PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: c.peerInfo.ChannelDelay,
			FunderDelay:  c.peerInfo.FunderDelay,
			MinExitDelay: c.peerInfo.MinimumExitDelay,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubArkKey = c.peerInfo.HubArkKey
	terms.VTXO.ArkOperatorKey = c.peerInfo.ArkOperatorKey
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubChannelKey = c.peerInfo.HubChannelKey
	copy(
		terms.VTXO.FunderKey[:],
		c.keys.funder.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// newReceiveIntentTerms binds one deterministic invoice reservation to the
// hub's Ark funding key and the client's channel endpoint keys.
func (c *NativeArkChannelController) newReceiveIntentTerms(
	paymentHash lntypes.Hash, reservedSCID uint64,
	capacity btcutil.Amount) (arkchannel.Terms, error) {

	pendingChannelID := arkchannel.ReceiveIntentPendingID(paymentHash)
	terms := arkchannel.Terms{
		ID:               arkchannel.ReceiveIntentID(paymentHash),
		Kind:             arkchannel.KindReceiveIntent,
		Funder:           arkchannel.PartyHub,
		PendingChannelID: pendingChannelID,
		ReservedSCID:     reservedSCID, Capacity: capacity,
		PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay:   c.peerInfo.ChannelDelay,
			FunderDelay:    c.peerInfo.FunderDelay,
			MinExitDelay:   c.peerInfo.MinimumExitDelay,
			HubArkKey:      c.peerInfo.HubArkKey,
			HubChannelKey:  c.peerInfo.HubChannelKey,
			ArkOperatorKey: c.peerInfo.ArkOperatorKey,
			FunderKey:      c.peerInfo.HubFunderKey,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// SendPayment creates a hub invoice and pays it through native lnd.
func (c *NativeArkChannelController) SendPayment(ctx context.Context,
	id arkchannel.ID, amount btcutil.Amount) (ArkChannelPaymentResult,
	error) {

	if c.party != arkchannel.PartyClient {
		return ArkChannelPaymentResult{}, fmt.Errorf("client payment " +
			"RPC is not available on the hub endpoint")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return ArkChannelPaymentResult{}, err
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return ArkChannelPaymentResult{}, err
	}
	hash, err := c.paymentPeer.CreateInvoice(ctx, id, amount)
	if err != nil {
		return ArkChannelPaymentResult{}, err
	}
	preimage, err := c.node.PayInvoiceResult(ctx, record, hash, amount)
	if err != nil {
		return ArkChannelPaymentResult{}, err
	}
	settled := preimage.Hash() == hash
	if !settled {
		return ArkChannelPaymentResult{}, fmt.Errorf("native client " +
			"payment returned the wrong preimage")
	}

	return ArkChannelPaymentResult{
		PaymentHash: hash, Settled: settled,
	}, nil
}

// ReceivePayment creates a local invoice and asks the hub to pay it.
func (c *NativeArkChannelController) ReceivePayment(ctx context.Context,
	id arkchannel.ID, amount btcutil.Amount) (ArkChannelPaymentResult,
	error) {

	if c.party != arkchannel.PartyClient {
		return ArkChannelPaymentResult{}, fmt.Errorf("client payment " +
			"RPC is not available on the hub endpoint")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return ArkChannelPaymentResult{}, err
	}
	if _, err := c.service.GetChannel(ctx, id); err != nil {
		return ArkChannelPaymentResult{}, err
	}
	_, hash, err := c.node.AddInvoice(ctx, amount)
	if err != nil {
		return ArkChannelPaymentResult{}, err
	}
	if err := c.paymentPeer.PayInvoice(ctx, id, hash, amount); err != nil {
		return ArkChannelPaymentResult{}, err
	}
	settled, err := c.node.InvoiceSettled(ctx, hash)
	if err != nil {
		return ArkChannelPaymentResult{}, err
	}
	if !settled {
		return ArkChannelPaymentResult{}, fmt.Errorf("native client " +
			"invoice did not settle")
	}

	return ArkChannelPaymentResult{
		PaymentHash: hash, Settled: settled,
	}, nil
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
// authenticated endpoint after the private invoice is durable.
func (c *NativeArkChannelController) RegisterIncomingPayment(
	ctx context.Context, hash lntypes.Hash, amount btcutil.Amount,
	reservedSCID uint64) error {

	if c.party != arkchannel.PartyClient {
		return fmt.Errorf("incoming payment registration is client " +
			"only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return err
	}

	capacity, err := c.paymentPeer.RegisterIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
	if err != nil {
		return err
	}
	terms, err := c.newReceiveIntentTerms(
		hash, reservedSCID, capacity,
	)
	if err != nil {
		return err
	}
	if _, err := c.remote.RegisterReceiveIntent(ctx, terms); err != nil {
		return err
	}
	_, err = c.service.RegisterReceiveIntent(ctx, terms)

	return err
}

// WaitIncomingPayment waits for the private hold invoice to be accepted and its
// receive channel to become active without releasing the shared preimage.
func (c *NativeArkChannelController) WaitIncomingPayment(ctx context.Context,
	hash lntypes.Hash) (arkchannel.ID, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.ID{}, fmt.Errorf("incoming payment wait is " +
			"client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.ID{}, err
	}

	err := waitIncomingPaymentReady(
		ctx,
		func(ctx context.Context) error {
			return c.WaitInvoiceAccepted(ctx, hash)
		},
		func(ctx context.Context) error {
			return c.syncReceiveIntent(ctx, hash)
		},
	)
	if err != nil {
		return arkchannel.ID{}, err
	}

	id := arkchannel.ReceiveIntentID(hash)
	if id == (arkchannel.ID{}) {
		return arkchannel.ID{}, fmt.Errorf("receive channel ID is " +
			"empty")
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.ID{}, fmt.Errorf("load active receive "+
			"channel: %w", err)
	}
	if record.Snapshot.Phase != arkchannel.PhaseActive ||
		record.Snapshot.Terms.Kind != arkchannel.KindReceiveIntent ||
		record.Snapshot.Terms.PaymentHash != hash {
		return arkchannel.ID{}, fmt.Errorf("receive channel %x is "+
			"not active", id[:4])
	}

	return id, nil
}

// waitIncomingPaymentReady joins the two independent durable readiness
// barriers. Returning after only one would either expose an unusable channel or
// release the preimage before its channel exists.
func waitIncomingPaymentReady(ctx context.Context,
	waitInvoice func(context.Context) error,
	syncIntent func(context.Context) error) error {

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	invoiceResult := make(chan error, 1)
	go func() {
		invoiceResult <- waitInvoice(waitCtx)
	}()
	syncResult := make(chan error, 1)
	go func() {
		syncResult <- syncIntent(waitCtx)
	}()

	for invoiceResult != nil || syncResult != nil {
		select {
		case err := <-invoiceResult:
			invoiceResult = nil
			if err != nil {
				return fmt.Errorf("wait for incoming hold "+
					"invoice: %w", err)
			}

		case err := <-syncResult:
			syncResult = nil
			if err != nil {
				return fmt.Errorf("synchronize receive "+
					"channel: %w", err)
			}

		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}

	return nil
}

// SettleIncomingPayment releases the private hold invoice after the SDK
// durably chooses the channel rail as the winner.
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

// CancelIncomingPayment prevents private settlement and abandons any paired
// receive intent that remains before the channel funding point of no return.
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
	if err := c.CancelInvoice(ctx, hash); err != nil {
		return fmt.Errorf("cancel incoming hold invoice: %w", err)
	}

	return c.cancelReceiveIntent(ctx, hash, reason)
}

// cancelReceiveIntent applies the same pre-PONR failure to every existing
// endpoint. Intermediate post-PONR states must finish activation before the
// caller can safely commit another winning rail.
func (c *NativeArkChannelController) cancelReceiveIntent(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	id := arkchannel.ReceiveIntentID(hash)
	remote, remoteErr := c.remote.GetFundingChannel(ctx, id)
	remoteMissing := isArkChannelNotFound(remoteErr)
	if remoteErr != nil && !remoteMissing {
		return fmt.Errorf("load remote receive intent: %w", remoteErr)
	}
	local, localErr := c.service.GetChannel(ctx, id)
	localMissing := isArkChannelNotFound(localErr)
	if localErr != nil && !localMissing {
		return fmt.Errorf("load local receive intent: %w", localErr)
	}
	if remoteMissing && localMissing {
		return nil
	}
	if !localMissing && (local.Snapshot.Terms.Kind !=
		arkchannel.KindReceiveIntent ||
		local.Snapshot.Terms.PaymentHash != hash) {
		return fmt.Errorf("channel is not this payment's receive " +
			"intent")
	}
	if (!remoteMissing && !receiveIntentPhaseKnown(remote.Phase)) ||
		(!localMissing &&
			!receiveIntentPhaseKnown(local.Snapshot.Phase)) {
		return fmt.Errorf("receive channel has an unknown phase")
	}

	if (!remoteMissing && receiveIntentIsCommitting(remote.Phase)) ||
		(!localMissing &&
			receiveIntentIsCommitting(local.Snapshot.Phase)) {
		return fmt.Errorf("receive channel is crossing its funding " +
			"safety boundary")
	}
	if receiveIntentEndpointsDisagree(
		remoteMissing, remote.Phase, localMissing, local.Snapshot.Phase,
	) {
		return fmt.Errorf("receive channel endpoints have not " +
			"converged")
	}

	event := &arkchannel.Fail{Reason: reason}
	if !remoteMissing && receiveIntentCanFail(remote.Phase) {
		if _, err := c.remote.ApplyChannelEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("cancel remote receive intent: %w",
				err)
		}
	}
	if !localMissing && receiveIntentCanFail(local.Snapshot.Phase) {
		if _, err := c.service.ApplyLocalEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("cancel local receive intent: %w",
				err)
		}
	}

	return nil
}

// isArkChannelNotFound recognizes local and transport-preserved missing rows.
func isArkChannelNotFound(err error) bool {
	return errors.Is(err, arkchannel.ErrNotFound) ||
		status.Code(err) == codes.NotFound
}

// receiveIntentCanFail identifies states where replaying Fail is safe.
func receiveIntentCanFail(phase arkchannel.Phase) bool {
	return phase == arkchannel.PhaseRequested ||
		phase == arkchannel.PhaseNegotiating ||
		phase == arkchannel.PhaseCancelling
}

// receiveIntentIsCommitting identifies the short post-PONR activation window.
func receiveIntentIsCommitting(phase arkchannel.Phase) bool {
	return phase == arkchannel.PhaseBackingReady ||
		phase == arkchannel.PhaseActivating
}

// receiveIntentIsRetained identifies reusable or later channel lifecycle
// states.
func receiveIntentIsRetained(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActive, arkchannel.PhaseMaterializing,
		arkchannel.PhaseOnChain, arkchannel.PhaseClosed,
		arkchannel.PhaseCoopClosing, arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// receiveIntentPhaseKnown rejects corrupt or future phase values
// conservatively.
func receiveIntentPhaseKnown(phase arkchannel.Phase) bool {
	return receiveIntentCanFail(phase) ||
		receiveIntentIsCommitting(phase) ||
		receiveIntentIsRetained(phase) ||
		phase == arkchannel.PhaseFailed
}

// receiveIntentEndpointsDisagree detects a retained/pre-PONR split.
func receiveIntentEndpointsDisagree(remoteMissing bool, remote arkchannel.Phase,
	localMissing bool, local arkchannel.Phase) bool {

	remoteRetained := !remoteMissing && receiveIntentIsRetained(remote)
	localRetained := !localMissing && receiveIntentIsRetained(local)

	return remoteRetained != localRetained
}

// syncReceiveIntent binds a hub-prepared source locally and lets the common
// funding FSM negotiate, commit, recover, and activate the channel.
func (c *NativeArkChannelController) syncReceiveIntent(ctx context.Context,
	hash lntypes.Hash) error {

	id := arkchannel.ReceiveIntentID(hash)
	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()
	for {
		remote, err := c.remote.GetFundingChannel(ctx, id)
		if err != nil {
			if err := c.waitReceiveIntentSyncRetry(
				ctx, ticker, hash, "load remote intent", err,
			); err != nil {
				return err
			}

			continue
		}
		local, err := c.service.GetChannel(ctx, id)
		if err != nil {
			if err := c.waitReceiveIntentSyncRetry(
				ctx, ticker, hash, "load local intent", err,
			); err != nil {
				return err
			}

			continue
		}
		if remote.Phase == arkchannel.PhaseFailed {
			return c.mirrorReceiveIntentFailure(
				ctx, local, remote,
			)
		}
		if remote.Phase == arkchannel.PhaseCancelling {
			reason := remote.Failure
			if reason == "" {
				reason = "hub receive channel is cancelling"
			}
			if _, err := c.remote.ApplyChannelEvent(
				ctx, id, &arkchannel.Fail{
					Reason: reason,
				},
			); err != nil {

				if err := c.waitReceiveIntentSyncRetry(
					ctx, ticker, hash, "resume remote "+
						"intent cancellation", err,
				); err != nil {
					return err
				}
			}

			continue
		}
		if local.Snapshot.Phase == arkchannel.PhaseCancelling {
			reason := local.Snapshot.Failure
			if reason == "" {
				reason = "local receive channel is cancelling"
			}
			if _, err := c.service.ApplyLocalEvent(
				ctx, id, &arkchannel.Fail{
					Reason: reason,
				},
			); err != nil {

				if err := c.waitReceiveIntentSyncRetry(
					ctx, ticker, hash, "resume local "+
						"intent cancellation", err,
				); err != nil {
					return err
				}
			}

			continue
		}
		if local.Snapshot.Phase == arkchannel.PhaseActive {
			return nil
		}
		if remote.Source != nil && local.Snapshot.Source == nil {
			_, err := c.service.ApplyPeerEvent(
				ctx, id, &arkchannel.BindVTXO{
					Binding: *remote.Source,
				},
			)
			if err != nil {
				failErr := c.failReceiveIntent(ctx, id, err)
				if errors.Is(
					failErr, ErrReceiveChannelFallback,
				) {
					return failErr
				}
				if retryErr := c.waitReceiveIntentSyncRetry(
					ctx, ticker, hash, "bind prepared "+
						"source", failErr,
				); retryErr != nil {
					return retryErr
				}
			}

			continue
		}
		if receiveIntentNeedsPeerReady(remote, local) {
			err := c.syncReceiveIntentPeerReady(
				ctx, id, remote, local,
			)
			if err != nil {
				if err := c.waitReceiveIntentSyncRetry(
					ctx, ticker, hash, "record peer "+
						"readiness", err,
				); err != nil {
					return err
				}
			}

			continue
		}
		if receiveIntentRecoveryInvalid(remote, local) {
			return fmt.Errorf("finalized receive channel is " +
				"missing funding artifacts")
		}
		if receiveIntentNeedsRecovery(remote, local) {
			err := c.installReceiveIntentRecovery(
				ctx, id, remote, local,
			)
			if err != nil {
				if err := c.waitReceiveIntentSyncRetry(
					ctx, ticker, hash, "install "+
						"recovery package", err,
				); err != nil {
					return err
				}
			}

			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:
		}
	}
}

// receiveIntentNeedsPeerReady identifies the paired requested-state barrier.
func receiveIntentNeedsPeerReady(remote lnruntime.FundingChannelState,
	local arkchannel.Record) bool {

	return remote.Source != nil && local.Snapshot.Source != nil &&
		(local.Snapshot.Phase == arkchannel.PhaseRequested ||
			remote.Phase == arkchannel.PhaseRequested)
}

// syncReceiveIntentPeerReady records peer readiness at both endpoints.
func (c *NativeArkChannelController) syncReceiveIntentPeerReady(
	ctx context.Context, id arkchannel.ID,
	remote lnruntime.FundingChannelState, local arkchannel.Record) error {

	event := &arkchannel.FundingPeerReady{}
	if local.Snapshot.Phase == arkchannel.PhaseRequested {
		if _, err := c.service.RecordLocalEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("record local peer readiness: %w",
				err)
		}
	}
	if remote.Phase == arkchannel.PhaseRequested {
		if _, err := c.remote.ApplyChannelEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("record remote peer readiness: %w",
				err)
		}
	}

	return nil
}

// receiveIntentNeedsRecovery identifies a finalized source not yet protected by
// the client's recovery package.
func receiveIntentNeedsRecovery(remote lnruntime.FundingChannelState,
	local arkchannel.Record) bool {

	return remote.OORFinalized && !local.Snapshot.ClientRecoveryReady
}

// receiveIntentRecoveryInvalid detects a finalized source missing
// prerequisites.
func receiveIntentRecoveryInvalid(remote lnruntime.FundingChannelState,
	local arkchannel.Record) bool {

	return receiveIntentNeedsRecovery(remote, local) &&
		(local.Snapshot.Source == nil || local.Snapshot.Backing == nil)
}

// installReceiveIntentRecovery imports the finalized source and records the
// paired recovery barrier.
func (c *NativeArkChannelController) installReceiveIntentRecovery(
	ctx context.Context, id arkchannel.ID,
	remote lnruntime.FundingChannelState, local arkchannel.Record) error {

	recovery, err := c.remote.ExportRecoveryPackage(ctx, id)
	if err != nil {
		return fmt.Errorf("export recovery package: %w", err)
	}
	if err := c.cfg.Recovery.InstallRecoveryPackage(
		ctx, id, local.Snapshot.Terms, *local.Snapshot.Source, recovery,
	); err != nil {
		return fmt.Errorf("install recovery package: %w", err)
	}
	event := &arkchannel.RecoveryPackageInstalled{
		Party: arkchannel.PartyClient,
	}
	if _, err := c.remote.ApplyChannelEvent(ctx, id, event); err != nil {
		return fmt.Errorf("record remote recovery package: %w", err)
	}
	if _, err := c.service.ApplyLocalEvent(ctx, id, event); err != nil {
		return fmt.Errorf("record local recovery package: %w", err)
	}

	return nil
}

// waitReceiveIntentSyncRetry keeps transient mailbox, store, and recovery
// failures from permanently disabling channel synchronization.
func (c *NativeArkChannelController) waitReceiveIntentSyncRetry(
	ctx context.Context, ticker *time.Ticker, hash lntypes.Hash,
	operation string, err error) error {

	if c.cfg.Log != nil {
		c.cfg.Log.WarnS(ctx, "Receive channel synchronization failed; "+
			"retrying", err,
			btclog.Hex("payment_hash", hash[:]),
			btclog.Fmt("operation", "%s", operation),
		)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-ticker.C:
		return nil
	}
}

// failReceiveIntent abandons the funder's prepared OOR and waits until its
// authoritative abort has driven both endpoint FSMs through lnd cleanup.
func (c *NativeArkChannelController) failReceiveIntent(ctx context.Context,
	id arkchannel.ID, cause error) error {

	reason := cause.Error()
	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		local, err := c.service.GetChannel(ctx, id)
		if err == nil {
			switch local.Snapshot.Phase {
			case arkchannel.PhaseRequested,
				arkchannel.PhaseNegotiating,
				arkchannel.PhaseCancelling,
				arkchannel.PhaseFailed:

				err = c.driveReceiveIntentFailure(
					ctx, id, local, reason,
				)

			default:
				return errors.Join(
					cause, fmt.Errorf("receive intent "+
						"crossed its funding safety "+
						"boundary at %s",
						local.Snapshot.Phase),
				)
			}
		}
		if err != nil {
			lastErr = err
		}

		local, localErr := c.service.GetChannel(ctx, id)
		remote, remoteErr := c.remote.GetFundingChannel(ctx, id)
		if localErr == nil && remoteErr == nil {
			localSourceAborted := local.Snapshot.Source == nil ||
				local.Snapshot.OORAborted
			remoteSourceAborted := remote.Source == nil ||
				remote.OORAborted
			if local.Snapshot.Phase == arkchannel.PhaseFailed &&
				remote.Phase == arkchannel.PhaseFailed &&
				localSourceAborted && remoteSourceAborted {
				return fmt.Errorf("%w: %w",
					ErrReceiveChannelFallback, cause)
			}
			if receiveIntentIsCommitting(remote.Phase) ||
				receiveIntentIsRetained(remote.Phase) {
				return errors.Join(
					cause, fmt.Errorf("remote receive "+
						"intent crossed its funding "+
						"safety boundary at %s",
						remote.Phase),
				)
			}
		} else {
			lastErr = errors.Join(localErr, remoteErr)
		}

		select {
		case <-ctx.Done():
			return errors.Join(cause, lastErr, ctx.Err())

		case <-ticker.C:
		}
	}
}

// driveReceiveIntentFailure first makes the observing endpoint terminal or
// cancelling, then asks the endpoint that funded the source to abort it.
func (c *NativeArkChannelController) driveReceiveIntentFailure(
	ctx context.Context, id arkchannel.ID, local arkchannel.Record,
	reason string) error {

	if local.Snapshot.Failure != "" {
		reason = local.Snapshot.Failure
	}
	if _, err := c.service.ApplyLocalEvent(
		ctx, id, &arkchannel.Fail{
			Reason: reason,
		},
	); err != nil {
		return err
	}
	if local.Snapshot.Terms.Funder == c.party {
		return nil
	}

	return c.remote.FailReceiveIntent(ctx, id, reason)
}

// failReceiveIntentDetached gives pre-PONR cleanup a bounded process-owned
// lifetime after the intercepted-payment request has ended.
func (c *NativeArkChannelController) failReceiveIntentDetached(
	ctx context.Context, id arkchannel.ID, cause error) error {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), arkChannelReceiveCleanupTimeout,
	)
	defer cancel()

	return c.failReceiveIntent(cleanupCtx, id, cause)
}

// receiveNegotiationFailureIsReplayable protects outcomes that may already
// have produced durable work. Only explicit peer rejections and local errors
// are definitive enough to abandon a still-negotiating channel.
func receiveNegotiationFailureIsReplayable(err error) bool {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, lnruntime.ErrFundingNegotiationAmbiguous) {
		return true
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return false
	}

	switch grpcStatus.Code() {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
		codes.PermissionDenied, codes.Unauthenticated,
		codes.FailedPrecondition, codes.OutOfRange,
		codes.Unimplemented:
		return false

	default:
		return true
	}
}

// handleReceiveNegotiationFailure converts only a definitive failure whose
// durable source remains pre-PONR into the vHTLC fallback signal.
func (c *NativeArkChannelController) handleReceiveNegotiationFailure(
	ctx context.Context, id arkchannel.ID, cause error) error {

	if receiveNegotiationFailureIsReplayable(cause) {
		return cause
	}

	return c.failReceiveIntentDetached(ctx, id, cause)
}

// mirrorReceiveIntentFailure applies the hub's terminal pre-PONR failure to
// the local channel record so restart recovery has no abandoned reservation.
func (c *NativeArkChannelController) mirrorReceiveIntentFailure(
	ctx context.Context, local arkchannel.Record,
	remote lnruntime.FundingChannelState) error {

	reason := remote.Failure
	if reason == "" {
		reason = "hub receive channel failed"
	}
	if local.Snapshot.Phase == arkchannel.PhaseFailed {
		return fmt.Errorf("%w: %s", ErrReceiveChannelFallback, reason)
	}
	record := local
	if local.Snapshot.Phase == arkchannel.PhaseRequested ||
		local.Snapshot.Phase == arkchannel.PhaseNegotiating {

		var err error
		record, err = c.service.ApplyLocalEvent(
			ctx, local.Snapshot.Terms.ID, &arkchannel.Fail{
				Reason: reason,
			},
		)
		if err != nil {
			return err
		}
	}
	if remote.OORAborted && record.Snapshot.Source != nil &&
		(record.Snapshot.Phase == arkchannel.PhaseCancelling ||
			record.Snapshot.Phase == arkchannel.PhaseBackingReady) {

		_, err := c.service.ApplyPeerEvent(
			ctx, local.Snapshot.Terms.ID, &arkchannel.OORAborted{
				SessionID: record.Snapshot.Source.OORSessionID,
				Reason:    reason,
			},
		)
		if err != nil {
			return err
		}
	} else if record.Snapshot.Phase != arkchannel.PhaseFailed {
		return fmt.Errorf("hub receive channel failed without a " +
			"definitive pre-PONR OOR abort")
	}

	return fmt.Errorf("%w: %s", ErrReceiveChannelFallback, reason)
}

// prepareIncomingChannelSource durably orders capital admission before wallet
// selection and binds the exact prepared OOR output to the channel FSM.
func (c *NativeArkChannelController) prepareIncomingChannelSource(
	ctx context.Context, record arkchannel.Record) error {

	snapshot := record.Snapshot
	if snapshot.Phase != arkchannel.PhaseRequested ||
		snapshot.Source != nil {
		return nil
	}
	if c.cfg.PrepareOOR == nil {
		return fmt.Errorf("hub channel OOR preparer is unavailable")
	}
	if _, err := c.service.StartOORPreparation(
		ctx, snapshot.Terms.ID,
	); err != nil {
		return err
	}
	if err := c.cfg.ReserveReceiveCapital(ctx, snapshot.Terms); err != nil {
		if !errors.Is(err, ErrReceiveChannelFallback) {
			return err
		}
		_, failErr := c.service.ApplyLocalEvent(
			ctx, snapshot.Terms.ID, &arkchannel.Fail{
				Reason: err.Error(),
			},
		)

		return errors.Join(err, failErr)
	}

	binding, err := c.cfg.PrepareOOR(
		ctx, snapshot.Terms, arkchannel.DefaultBackingFee,
	)
	if err == nil {
		_, err = c.service.BindPreparedOOR(
			ctx, snapshot.Terms.ID, binding,
		)

		return err
	}
	if errors.Is(err, arkchannel.ErrOORPreparationAmbiguous) {
		return fmt.Errorf("reconcile ambiguous receive channel OOR: %w",
			err)
	}
	_, failErr := c.service.ApplyLocalEvent(
		ctx, snapshot.Terms.ID, &arkchannel.Fail{
			Reason: err.Error(),
		},
	)

	return errors.Join(
		fmt.Errorf("%w: %w", ErrReceiveChannelFallback, err), failErr,
	)
}

// ManifestIncomingChannel binds hub-owned Ark liquidity to a registered
// receive intent and drives native lnd funding after the client records the
// exact prepared source.
func (c *NativeArkChannelController) ManifestIncomingChannel(
	ctx context.Context, hash lntypes.Hash, amount, capacity btcutil.Amount,
	reservedSCID uint64) (arkchannel.Record, error) {

	if c.party != arkchannel.PartyHub {
		return arkchannel.Record{}, fmt.Errorf("receive channel " +
			"funding is hub only")
	}
	id := arkchannel.ReceiveIntentID(hash)
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return arkchannel.Record{}, err
	}
	terms := record.Snapshot.Terms
	if terms.Kind != arkchannel.KindReceiveIntent ||
		terms.Funder != arkchannel.PartyHub ||
		terms.PaymentHash != hash || terms.Capacity != capacity ||
		terms.ReservedSCID != reservedSCID || capacity < amount {
		return arkchannel.Record{}, fmt.Errorf("receive intent does " +
			"not match intercepted payment")
	}
	if err := c.prepareIncomingChannelSource(ctx, record); err != nil {
		return arkchannel.Record{}, err
	}

	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()
	for {
		record, err = c.service.GetChannel(ctx, id)
		if err != nil {
			return arkchannel.Record{}, err
		}
		switch record.Snapshot.Phase {
		case arkchannel.PhaseNegotiating:
			_, err = c.service.ResumeChannelAction(ctx, id)
			if err != nil {
				return arkchannel.Record{},
					c.handleReceiveNegotiationFailure(
						ctx, id, err,
					)
			}

			continue

		case arkchannel.PhaseActive:
			return record, nil

		case arkchannel.PhaseFailed:
			return arkchannel.Record{}, fmt.Errorf("%w: %s",
				ErrReceiveChannelFallback,
				record.Snapshot.Failure)

		case arkchannel.PhaseRequested, arkchannel.PhaseBackingReady,
			arkchannel.PhaseActivating, arkchannel.PhaseCancelling:

			// The opposite endpoint or a replayable FSM action
			// still has work in flight. Poll the durable record
			// below.

		case arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
			arkchannel.PhaseClosed, arkchannel.PhaseCoopClosing,
			arkchannel.PhaseCoopCloseSigned,
			arkchannel.PhaseCoopClosePublished:
			return arkchannel.Record{}, fmt.Errorf("receive "+
				"channel entered unexpected phase %s before "+
				"activation", record.Snapshot.Phase)

		default:
			return arkchannel.Record{}, fmt.Errorf("receive "+
				"channel has unknown phase %d",
				record.Snapshot.Phase)
		}

		select {
		case <-ctx.Done():
			return arkchannel.Record{}, ctx.Err()

		case <-ticker.C:
		}
	}
}

// AbandonReceiveIntent removes an unused pre-funding reservation. Active
// channels are retained because their liquidity is reusable after this
// particular incoming payment takes another route.
func (c *NativeArkChannelController) AbandonReceiveIntent(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	if c.party != arkchannel.PartyHub {
		return fmt.Errorf("receive intent abandonment is hub only")
	}
	if reason == "" {
		return fmt.Errorf("receive intent abandonment reason is " +
			"required")
	}
	id := arkchannel.ReceiveIntentID(hash)
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if record.Snapshot.Terms.Kind != arkchannel.KindReceiveIntent ||
		record.Snapshot.Terms.PaymentHash != hash {
		return fmt.Errorf("channel is not this payment's receive " +
			"intent")
	}
	switch record.Snapshot.Phase {
	case arkchannel.PhaseRequested:
		_, err := c.service.ApplyLocalEvent(
			ctx, id, &arkchannel.Fail{
				Reason: reason,
			},
		)

		return err

	case arkchannel.PhaseFailed, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
		arkchannel.PhaseClosed, arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return nil

	default:
		return fmt.Errorf("cannot abandon receive intent from %s",
			record.Snapshot.Phase)
	}
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
	closeTx, err := c.node.ForceCloseChannel(backing.ChannelPoint)
	var closeTxID chainhash.Hash
	if err == nil {
		if closeTx == nil {
			return arkchannel.Record{}, chainhash.Hash{},
				chainhash.Hash{}, fmt.Errorf("lnd returned " +
					"no force-close transaction")
		}
		closeTxID = closeTx.TxHash()
	} else {
		// Both endpoints watch the unpublished channel point before Ark
		// materializes it. Either endpoint may therefore win the
		// commitment publication race. Only lnd's durable close summary
		// can turn the losing broadcast error into success.
		var waitErr error
		closeTxID, waitErr = c.node.WaitForceCloseResult(
			ctx, backing.ChannelPoint,
		)
		if waitErr != nil {
			return arkchannel.Record{}, chainhash.Hash{},
				chainhash.Hash{}, fmt.Errorf("force close "+
					"channel: %w; reconcile peer close: %v",
					err, waitErr)
		}
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

// PayHash sends or resumes one same-hash payment over a selected private
// channel and returns the destination preimage.
func (c *NativeArkChannelController) PayHash(ctx context.Context,
	id arkchannel.ID, hash lntypes.Hash, amount btcutil.Amount) (
	lntypes.Preimage, error) {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return lntypes.Preimage{}, err
		}
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return lntypes.Preimage{}, err
	}

	return c.node.PayInvoiceResult(ctx, record, hash, amount)
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
		fundingWire := c.fundingWire
		c.mu.RUnlock()
		if node == nil {
			return fmt.Errorf("native Ark channel endpoint is " +
				"not ready")
		}

		if fundingWire != nil && fundingWire.Handles(message) {
			return fundingWire.Handle(ctx, message)
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

// Stop releases native lnd state before the owning wallet and database stop.
func (c *NativeArkChannelController) Stop() error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopped = true
		cancel := c.lifecycleCancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.lifecycleWG.Wait()

		c.mu.Lock()
		fundingWire := c.fundingWire
		node := c.node
		c.fundingWire = nil
		c.node = nil
		c.service = nil
		c.clientClose = nil
		c.hubClose = nil
		c.mu.Unlock()

		if fundingWire != nil {
			fundingWire.Close()
		}
		if node != nil {
			c.stopErr = node.Stop()
		}
		if c.cfg.Recovery != nil {
			c.cfg.Recovery.Stop()
		}
	})

	return c.stopErr
}

// arkChannelRefreshDelivery returns the ordinary Ark account key that owns
// this endpoint's replacement VTXO after an in-Ark refresh.
type arkChannelRefreshDelivery struct {
	owner *btcec.PublicKey
}

// newArkChannelRefreshDelivery constructs a fixed OOR owner-key source.
func newArkChannelRefreshDelivery(
	owner *btcec.PublicKey) *arkChannelRefreshDelivery {

	return &arkChannelRefreshDelivery{owner: owner}
}

// CooperativeCloseDelivery returns the compressed replacement VTXO owner key.
func (d *arkChannelRefreshDelivery) CooperativeCloseDelivery(_ context.Context,
	_ arkchannel.ID) ([]byte, error) {

	if d == nil || d.owner == nil {
		return nil, fmt.Errorf("channel refresh OOR owner is required")
	}

	return d.owner.SerializeCompressed(), nil
}

// ValidateCooperativeCloseDelivery proves the replacement VTXO is assigned to
// this endpoint's ordinary Ark account.
func (d *arkChannelRefreshDelivery) ValidateCooperativeCloseDelivery(
	ctx context.Context, id arkchannel.ID, owner []byte) error {

	expected, err := d.CooperativeCloseDelivery(ctx, id)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, owner) {
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
