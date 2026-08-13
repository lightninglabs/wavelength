package waved

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
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
	arkChannelArkKeyFamily     keychain.KeyFamily = 220
	arkChannelBackingKeyFamily keychain.KeyFamily = 221
	arkChannelFunderKeyFamily  keychain.KeyFamily = 223

	defaultArkChannelBackingFee btcutil.Amount = 1_000
)

// HubArkChannelControllerConfig contains the hub-only signer, publisher, and
// authenticated client transport needed to compose one native endpoint.
type HubArkChannelControllerConfig struct {
	Process ArkChannelControllerConfig

	RemoteNode    [33]byte
	PeerSender    lnruntime.PeerEventSender
	Info          lnruntime.FundingPeerInfo
	CloseObserver lnruntime.CooperativeCloseObserver
	PaymentBridge lnruntime.PaymentBridgeCoordinator
}

// NativeArkChannelController owns one Ark FSM and one modular lnd endpoint.
type NativeArkChannelController struct {
	party arkchannel.Party
	cfg   ArkChannelControllerConfig

	coordinator *arkchannel.Coordinator
	remote      lnruntime.ProcessFundingPeer
	peerInfo    lnruntime.FundingPeerInfo
	keys        nativeArkChannelKeys

	mu            sync.RWMutex
	node          *lnruntime.NativeNode
	service       *arkchannel.Service
	clientClose   *lnruntime.ClientCooperativeCloseProcess
	hubClose      *lnruntime.HubCooperativeCloseProcess
	paymentBridge lnruntime.PaymentBridgeCoordinator
}

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

// NewHubFundingPeerInfo derives the immutable channel policy advertised by a
// real operator Wavelength process. The key roles share the same deterministic
// locators used when the hub endpoint is restored after restart.
func NewHubFundingPeerInfo(ctx context.Context,
	cfg ArkChannelControllerConfig) (lnruntime.FundingPeerInfo, error) {

	if err := validateArkChannelProcessConfig(cfg); err != nil {
		return lnruntime.FundingPeerInfo{}, err
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

	if err := validateClientArkChannelProcessConfig(cfg); err != nil {
		return nil, err
	}
	coordinator, err := arkchannel.NewCoordinator(cfg.Store)
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

	return &NativeArkChannelController{
		party: arkchannel.PartyClient, cfg: cfg,
		coordinator: coordinator, remote: remote, keys: keys,
	}, nil
}

// NewHubArkChannelController eagerly composes one authenticated client
// endpoint for swapserver's channel directory.
func NewHubArkChannelController(ctx context.Context,
	cfg HubArkChannelControllerConfig) (*NativeArkChannelController,
	error) {

	if err := validateArkChannelProcessConfig(cfg.Process); err != nil {
		return nil, err
	}
	if _, err := btcec.ParsePubKey(cfg.RemoteNode[:]); err != nil {
		return nil, fmt.Errorf("parse client channel node: %w", err)
	}
	if cfg.PeerSender == nil || cfg.CloseObserver == nil {
		return nil, fmt.Errorf("complete hub channel process is " +
			"required")
	}
	if err := cfg.Info.Validate(); err != nil {
		return nil, err
	}
	coordinator, err := arkchannel.NewCoordinator(cfg.Process.Store)
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
		paymentBridge: cfg.PaymentBridge,
	}
	if err := controller.startHub(
		ctx, cfg.RemoteNode, cfg.PeerSender, cfg.CloseObserver,
	); err != nil {
		return nil, err
	}

	return controller, nil
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

	case cfg.OperatorTerms == nil || cfg.OperatorTerms.PubKey == nil:
		return fmt.Errorf("Ark operator terms are required")

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
	defer c.mu.Unlock()
	if c.node != nil {
		return nil
	}
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
	negotiator, err := node.NewNegotiator(c.remote, c.cfg.Recovery)
	if err != nil {
		_ = node.Stop()

		return err
	}
	delivery := newArkChannelCloseDelivery(c.cfg.OORDestination)
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
	c.service = service
	if err := node.Start(); err != nil {
		c.service = nil
		_ = node.Stop()

		return err
	}
	if err := c.restoreRecoveryWatches(ctx, service); err != nil {
		c.service = nil
		_ = node.Stop()

		return err
	}
	if err := service.Resume(ctx); err != nil {
		_ = node.Stop()

		return err
	}
	if err := resumeOnchainArkChannels(ctx, node, service); err != nil {
		_ = node.Stop()

		return err
	}
	c.peerInfo = peerInfo
	c.node = node
	c.clientClose = clientClose

	return nil
}

// startHub starts one operator endpoint for an authenticated client.
func (c *NativeArkChannelController) startHub(ctx context.Context,
	remoteNode [33]byte, sender lnruntime.PeerEventSender,
	closeObserver lnruntime.CooperativeCloseObserver) error {

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
	negotiator, err := node.NewNegotiator(
		node.FundingEndpoint(), c.cfg.Recovery,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	delivery := newArkChannelCloseDelivery(c.cfg.OORDestination)
	closeEndpoint, err := lnruntime.NewNativeCooperativeCloseEndpoint(
		arkchannel.PartyHub, node.Runtime(), c.cfg.Wallet.BtcWallet,
		c.keys.ark, delivery,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	hubClose, err := lnruntime.NewHubCooperativeCloseProcess(
		closeEndpoint, delivery, closeObserver,
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	service, err := c.newService(
		node, negotiator, &lnruntime.HubCooperativeCloseExecutor{
			HubCooperativeCloseProcess: hubClose,
		},
	)
	if err != nil {
		_ = node.Stop()

		return err
	}
	c.service = service
	if err := node.Start(); err != nil {
		c.service = nil
		_ = node.Stop()

		return err
	}
	if err := c.restoreRecoveryWatches(ctx, service); err != nil {
		c.service = nil
		_ = node.Stop()

		return err
	}
	if err := service.Resume(ctx); err != nil {
		_ = node.Stop()

		return err
	}
	if err := resumeOnchainArkChannels(ctx, node, service); err != nil {
		_ = node.Stop()

		return err
	}
	c.node = node
	c.hubClose = hubClose

	return nil
}

// resumeOnchainArkChannels closes the crash window between durable backing
// publication and lnd's commitment-broadcast marker.
func resumeOnchainArkChannels(ctx context.Context, node *lnruntime.NativeNode,
	service *arkchannel.Service) error {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if !shouldResumeOnchainArkChannel(record.Snapshot) {
			continue
		}
		if record.Snapshot.Backing == nil {
			return fmt.Errorf("materialized channel %x has "+
				"no backing", record.Snapshot.Terms.ID[:4])
		}
		if err := node.ResumeForceCloseChannel(
			record.Snapshot.Backing.ChannelPoint,
		); err != nil {
			return fmt.Errorf("resume materialized channel %x: %w",
				record.Snapshot.Terms.ID[:4], err)
		}
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
	shouldWatchChannel := func(channelPoint wire.OutPoint) (bool, error) {
		record, err := c.coordinator.FindByChannelPoint(
			logCtx, channelPoint,
		)
		if err != nil {
			return false, fmt.Errorf("load Ark channel "+
				"lifecycle: %w", err)
		}

		return shouldWatchArkChannel(record.Snapshot.Phase), nil
	}
	recordChannelFullyResolved := func(channelPoint wire.OutPoint) error {
		return c.recordFullyResolvedChannel(
			logCtx, channelPoint,
		)
	}
	beforeCommitmentPublish := func(channelPoint wire.OutPoint) error {
		return c.materializeBeforeCommitment(
			logCtx, channelPoint,
		)
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
		ShouldWatchChannel:         shouldWatchChannel,
		BeforeCommitmentPublish:    beforeCommitmentPublish,
		RecordChannelFullyResolved: recordChannelFullyResolved,
	})
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
		c.party, node.FundingActivator(), negotiator, oor, materializer,
		node, forceCloser, closer,
	)
	if err != nil {
		return nil, err
	}

	return arkchannel.NewService(c.coordinator, executor)
}

// PromoteVTXO prepares and activates one client-funded OOR channel.
func (c *NativeArkChannelController) PromoteVTXO(ctx context.Context,
	amount btcutil.Amount) (arkchannel.Record, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, fmt.Errorf("only a client can " +
			"promote VTXO liquidity")
	}
	if amount <= 0 {
		return arkchannel.Record{}, fmt.Errorf("channel amount must " +
			"be positive")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, err
	}
	terms, err := c.newPromotionTerms(ctx, amount)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.remote.RegisterPromotion(ctx, terms); err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := c.service.RegisterPromotion(ctx, terms); err != nil {
		return arkchannel.Record{}, err
	}
	binding, err := c.cfg.PrepareOOR(
		ctx, terms, defaultArkChannelBackingFee,
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

// newPromotionTerms creates unique protocol identifiers and binds every key
// role to the two endpoint wallets.
func (c *NativeArkChannelController) newPromotionTerms(ctx context.Context,
	amount btcutil.Amount) (arkchannel.Terms, error) {

	var id arkchannel.ID
	if _, err := rand.Read(id[:]); err != nil {
		return arkchannel.Terms{}, err
	}
	var pending [32]byte
	if _, err := rand.Read(pending[:]); err != nil {
		return arkchannel.Terms{}, err
	}
	var txIndexBytes [4]byte
	if _, err := rand.Read(txIndexBytes[:3]); err != nil {
		return arkchannel.Terms{}, err
	}
	_, height, err := c.cfg.Wallet.BtcWallet.GetBestBlock()
	if err != nil {
		return arkchannel.Terms{}, err
	}
	if height < 0 {
		return arkchannel.Terms{}, fmt.Errorf("invalid channel chain " +
			"height")
	}
	txIndex := binary.BigEndian.Uint32(txIndexBytes[:]) & 0x00ffffff
	if txIndex == 0 {
		txIndex = 1
	}
	scid := lnwire.ShortChannelID{
		BlockHeight: uint32(height) + 1,
		TxIndex:     txIndex, TxPosition: 0,
	}.ToUint64()

	return c.newClientFundedTerms(
		id, pending, scid, amount, arkchannel.KindPromotion,
		lntypes.Hash{},
	)
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

// PromoteIncomingVHTLC atomically claims one server-funded fallback vHTLC
// into a client-funded channel using the invoice's already-advertised SCID.
func (c *NativeArkChannelController) PromoteIncomingVHTLC(ctx context.Context,
	paymentHash lntypes.Hash, reservedSCID uint64, capacity btcutil.Amount,
	source ArkChannelClaimSource) (arkchannel.Record, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, fmt.Errorf("only a client can " +
			"promote an incoming vHTLC")
	}
	if paymentHash == (lntypes.Hash{}) || reservedSCID == 0 ||
		capacity <= 0 || source.Amount != capacity+
		defaultArkChannelBackingFee {
		return arkchannel.Record{}, fmt.Errorf("invalid incoming " +
			"vHTLC channel terms")
	}
	if source.RecoveryID == "" || source.Outpoint == "" ||
		len(source.PolicyTemplate) == 0 ||
		len(source.SpendPath) == 0 || len(source.PkScript) == 0 {
		return arkchannel.Record{}, fmt.Errorf("complete incoming " +
			"vHTLC claim source is required")
	}
	if c.cfg.PrepareClaimRoot == nil {
		return arkchannel.Record{}, fmt.Errorf("incoming vHTLC " +
			"channel recovery root preparer is required")
	}
	if c.cfg.PrepareClaimOOR == nil {
		return arkchannel.Record{}, fmt.Errorf("incoming vHTLC " +
			"channel OOR preparer is required")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, err
	}
	terms, err := c.newReceiveClaimTerms(
		paymentHash, reservedSCID, capacity,
	)
	if err != nil {
		return arkchannel.Record{}, err
	}
	remoteRecord, err := c.remote.RegisterPromotion(ctx, terms)
	if err != nil {
		return arkchannel.Record{}, err
	}
	localRecord, err := c.service.RegisterPromotion(ctx, terms)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if localRecord.Snapshot.Phase == arkchannel.PhaseActive &&
		remoteRecord.Snapshot.Phase == arkchannel.PhaseActive {
		return localRecord, nil
	}
	recoverySource, recovery, err := c.remote.ExportReceiveClaimRecovery(
		ctx, terms.ID,
	)
	if err != nil {
		return arkchannel.Record{}, fmt.Errorf("export incoming vHTLC "+
			"channel recovery: %w", err)
	}
	if err := c.cfg.PrepareClaimRoot(
		ctx, terms, source, recoverySource, recovery,
	); err != nil {
		return arkchannel.Record{}, fmt.Errorf("prepare incoming "+
			"vHTLC channel recovery root: %w", err)
	}

	var binding arkchannel.VTXOBinding
	switch {
	case localRecord.Snapshot.Source != nil:
		binding = localRecord.Snapshot.Source.Clone()

	case remoteRecord.Snapshot.Source != nil:
		binding = remoteRecord.Snapshot.Source.Clone()

	default:
		binding, err = c.cfg.PrepareClaimOOR(
			ctx, terms, defaultArkChannelBackingFee, source,
		)
		if err != nil {
			return arkchannel.Record{}, err
		}
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

	record, err := c.service.GetChannel(ctx, terms.ID)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if record.Snapshot.Phase != arkchannel.PhaseActive {
		return arkchannel.Record{}, fmt.Errorf("incoming vHTLC "+
			"channel stopped in phase %s", record.Snapshot.Phase)
	}

	return record, nil
}

// newReceiveClaimTerms derives stable identifiers from the payment hash so a
// restart cannot negotiate a second channel for the same fallback vHTLC.
func (c *NativeArkChannelController) newReceiveClaimTerms(
	paymentHash lntypes.Hash, reservedSCID uint64,
	capacity btcutil.Amount) (arkchannel.Terms, error) {

	id := sha256.Sum256(
		append(
			[]byte("wavelength-receive-channel-id:"),
			paymentHash[:]...,
		),
	)
	pending := sha256.Sum256(
		append(
			[]byte("wavelength-receive-pending-id:"),
			paymentHash[:]...,
		),
	)

	return c.newClientFundedTerms(
		arkchannel.ID(id), pending, reservedSCID, capacity,
		arkchannel.KindReceiveClaim, paymentHash,
	)
}

// SendPayment creates a hub invoice and pays it through native lnd.
func (c *NativeArkChannelController) SendPayment(ctx context.Context,
	id arkchannel.ID, amount btcutil.Amount) (lntypes.Hash, error) {

	if c.party != arkchannel.PartyClient {
		return lntypes.Hash{}, fmt.Errorf("client payment RPC is not " +
			"available on the hub endpoint")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return lntypes.Hash{}, err
	}
	record, err := c.service.GetChannel(ctx, id)
	if err != nil {
		return lntypes.Hash{}, err
	}
	hash, err := c.remote.CreateInvoice(ctx, id, amount)
	if err != nil {
		return lntypes.Hash{}, err
	}
	if err := c.node.PayInvoice(ctx, record, hash, amount); err != nil {
		return lntypes.Hash{}, err
	}

	return hash, nil
}

// ReceivePayment creates a local invoice and asks the hub to pay it.
func (c *NativeArkChannelController) ReceivePayment(ctx context.Context,
	id arkchannel.ID, amount btcutil.Amount) (lntypes.Hash, error) {

	if c.party != arkchannel.PartyClient {
		return lntypes.Hash{}, fmt.Errorf("client payment RPC is not " +
			"available on the hub endpoint")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return lntypes.Hash{}, err
	}
	if _, err := c.service.GetChannel(ctx, id); err != nil {
		return lntypes.Hash{}, err
	}
	_, hash, err := c.node.AddInvoice(ctx, amount)
	if err != nil {
		return lntypes.Hash{}, err
	}
	if err := c.remote.PayInvoice(ctx, id, hash, amount); err != nil {
		return lntypes.Hash{}, err
	}
	settled, err := c.node.InvoiceSettled(ctx, hash)
	if err != nil {
		return lntypes.Hash{}, err
	}
	if !settled {
		return lntypes.Hash{}, fmt.Errorf("native client invoice did " +
			"not settle")
	}

	return hash, nil
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
	preparation, err := c.remote.PrepareOutgoingPayment(
		ctx, paymentRequest, maxFee,
	)
	if err != nil {
		return LightningPaymentResult{}, err
	}
	record, err := c.service.GetChannel(ctx, preparation.ChannelID)
	if err != nil {
		return LightningPaymentResult{}, err
	}
	if record.Snapshot.Terms.ReservedSCID != preparation.ReservedSCID {
		return LightningPaymentResult{}, fmt.Errorf("payment " +
			"preparation changed channel SCID")
	}
	preimage, err := c.node.PayInvoiceResult(
		ctx, record, preparation.PaymentHash, preparation.PrivateAmount,
	)
	if err != nil {
		cancelErr := c.remote.CancelOutgoingPayment(
			ctx, preparation.PaymentHash, err.Error(),
		)
		if cancelErr != nil {
			return LightningPaymentResult{}, errors.Join(
				err, fmt.Errorf("cancel outgoing payment: %w",
					cancelErr),
			)
		}

		return LightningPaymentResult{}, err
	}
	if preimage.Hash() != preparation.PaymentHash {
		return LightningPaymentResult{}, fmt.Errorf("payment " +
			"preimage does not match preparation")
	}

	return LightningPaymentResult{
		PaymentHash: preparation.PaymentHash, Preimage: preimage,
		PrivateAmount: preparation.PrivateAmount, Fee: preparation.Fee,
		ChannelID: preparation.ChannelID,
	}, nil
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

	return c.remote.RegisterIncomingPayment(
		ctx, hash, amount, reservedSCID,
	)
}

// WaitIncomingPayment waits for the known-preimage private invoice to settle.
func (c *NativeArkChannelController) WaitIncomingPayment(ctx context.Context,
	hash lntypes.Hash) error {

	if c.party != arkchannel.PartyClient {
		return fmt.Errorf("incoming payment wait is client only")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return err
	}

	return c.WaitInvoiceSettled(ctx, hash)
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

// RequestCooperativeClose starts the client-owned 3-of-3 OOR close process.
func (c *NativeArkChannelController) RequestCooperativeClose(
	ctx context.Context, id arkchannel.ID) (arkchannel.Record, error) {

	if c.party != arkchannel.PartyClient {
		return arkchannel.Record{}, fmt.Errorf("only a client can " +
			"request cooperative close")
	}
	if err := c.ensureClientStarted(ctx); err != nil {
		return arkchannel.Record{}, err
	}

	return c.clientClose.RequestCooperativeClose(ctx, id)
}

// GetChannel returns the endpoint's durable Ark channel record.
func (c *NativeArkChannelController) GetChannel(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return arkchannel.Record{}, err
		}
	}

	return c.service.GetChannel(ctx, id)
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

// AddInvoiceWithPreimage registers the private destination for an incoming
// bridge before its BOLT 11 invoice is exposed to a payer.
func (c *NativeArkChannelController) AddInvoiceWithPreimage(ctx context.Context,
	preimage lntypes.Preimage, amount btcutil.Amount) error {

	if c.party == arkchannel.PartyClient {
		if err := c.ensureClientStarted(ctx); err != nil {
			return err
		}
	}
	_, err := c.node.AddInvoiceWithPreimage(
		ctx, amount, preimage, false,
	)

	return err
}

// WaitInvoiceAccepted waits for the private outgoing source HTLC.
func (c *NativeArkChannelController) WaitInvoiceAccepted(ctx context.Context,
	hash lntypes.Hash) error {

	return c.node.WaitInvoiceAccepted(ctx, hash)
}

// WaitInvoiceSettled waits for an incoming private destination to settle.
func (c *NativeArkChannelController) WaitInvoiceSettled(ctx context.Context,
	hash lntypes.Hash) error {

	return c.node.WaitInvoiceSettled(ctx, hash)
}

// SettleHoldInvoice releases the private outgoing source with the public
// destination preimage.
func (c *NativeArkChannelController) SettleHoldInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	return c.node.SettleHoldInvoice(ctx, preimage)
}

// CancelInvoice fails a private source before a preimage is known.
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

// Stop releases native lnd state before the owning wallet and database stop.
func (c *NativeArkChannelController) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if c.node != nil {
		err = c.node.Stop()
		c.node = nil
	}
	if c.cfg.Recovery != nil {
		c.cfg.Recovery.Stop()
	}

	return err
}

// arkChannelCloseDelivery returns the ordinary Ark account key that owns this
// endpoint's replacement VTXO after an in-Ark cooperative close.
type arkChannelCloseDelivery struct {
	owner *btcec.PublicKey
}

// newArkChannelCloseDelivery constructs a fixed OOR owner-key source.
func newArkChannelCloseDelivery(
	owner *btcec.PublicKey) *arkChannelCloseDelivery {

	return &arkChannelCloseDelivery{owner: owner}
}

// CooperativeCloseDelivery returns the compressed replacement VTXO owner key.
func (d *arkChannelCloseDelivery) CooperativeCloseDelivery(_ context.Context,
	_ arkchannel.ID) ([]byte, error) {

	if d == nil || d.owner == nil {
		return nil, fmt.Errorf("cooperative close OOR owner is " +
			"required")
	}

	return d.owner.SerializeCompressed(), nil
}

// ValidateCooperativeCloseDelivery proves the replacement VTXO is assigned to
// this endpoint's ordinary Ark account.
func (d *arkChannelCloseDelivery) ValidateCooperativeCloseDelivery(
	ctx context.Context, id arkchannel.ID, owner []byte) error {

	expected, err := d.CooperativeCloseDelivery(ctx, id)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, owner) {
		return fmt.Errorf("cooperative close owner is not the " +
			"configured Ark account")
	}

	return nil
}

var _ ArkChannelController = (*NativeArkChannelController)(nil)

var _ lnruntime.CooperativeCloseDeliverySource = (*arkChannelCloseDelivery)(nil)

//nolint:ll // Keeping the complete delivery contract explicit aids API audits.
var _ lnruntime.CooperativeCloseDeliveryValidator = (*arkChannelCloseDelivery)(nil)
