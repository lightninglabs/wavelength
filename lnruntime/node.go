package lnruntime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	channelDBFileName       = "channel.db"
	channelDBTimeout        = 30 * time.Second
	privatePaymentCLTVDelta = uint32(40)
	maximumBlockHeight      = ^uint32(0)
)

// TerminalPaymentError means lnd's control tower has durably terminated a
// payment without a preimage. Retrying the same hash cannot create a new
// attempt, but a higher-level atomic bridge may still choose another rail.
type TerminalPaymentError struct {
	Reason string
}

// Error returns the terminal lnd payment reason.
func (e *TerminalPaymentError) Error() string {
	if e == nil || e.Reason == "" {
		return "native payment failed"
	}

	return "native payment failed: " + e.Reason
}

// PaymentNotStartedError means lnd rejected a route and its control tower
// confirms that no attempt owns the payment hash. A caller may safely choose
// another delivery rail because this node cannot reveal the preimage later.
type PaymentNotStartedError struct {
	Err error
}

// Error returns the underlying route admission failure.
func (e *PaymentNotStartedError) Error() string {
	if e == nil || e.Err == nil {
		return "native payment was not started"
	}

	return "native payment was not started: " + e.Err.Error()
}

// Unwrap exposes the underlying route admission failure.
func (e *PaymentNotStartedError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// NativeNodeConfig contains the process-owned dependencies for one modular
// lnd channel endpoint.
type NativeNodeConfig struct {
	DataDir string
	DB      *channeldb.DB

	Party            arkchannel.Party
	Chain            lnwallet.BlockChainIO
	Notifier         chainntnfs.ChainNotifier
	WalletController lnwallet.WalletController
	KeyRing          keychain.SecretKeyRing
	Signer           input.Signer
	FeeEstimator     chainfee.Estimator
	NetParams        *chaincfg.Params
	IdentityKey      keychain.KeyDescriptor
	BackingKey       keychain.KeyDescriptor
	RemoteNodeKey    *btcec.PublicKey
	Transport        MessageTransport
	Intents          ChannelIntentSource

	OnChannelOpened            func(*chanstate.OpenChannel)
	OnChannelFailure           LinkFailureHandler
	ShouldWatchChannel         func(wire.OutPoint) (bool, error)
	BeforeCommitmentPublish    func(wire.OutPoint) error
	RecordChannelFullyResolved func(wire.OutPoint) error
}

// NativeNode owns one persistent channel database and the native lnd runtime,
// logical peer, and funding endpoint attached to it.
type NativeNode struct {
	cfg NativeNodeConfig

	db              *channeldb.DB
	closeDB         bool
	notifier        *VirtualFundingNotifier
	runtime         *Runtime
	peer            *Peer
	fundingEndpoint *NativeFundingEndpoint

	mu      sync.Mutex
	started bool
	stopped bool
}

// NewNativeNode composes one endpoint without starting its lnd goroutines.
func NewNativeNode(cfg NativeNodeConfig) (*NativeNode, error) {
	if err := validateNativeNodeConfig(cfg); err != nil {
		return nil, err
	}

	db := cfg.DB
	closeDB := false
	if db == nil {
		var err error
		db, err = openNativeChannelDB(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		closeDB = true
	}

	notifier, err := NewVirtualFundingNotifier(cfg.Notifier)
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}
	witnessBeacon, err := NewWitnessBeacon(db)
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}
	acceptor, err := NewIntentAcceptor(cfg.Party, cfg.Intents)
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}

	var peer *Peer
	runtime, err := NewRuntime(RuntimeConfig{
		DB:       db,
		Chain:    cfg.Chain,
		Notifier: notifier,
		OnionKey: keychain.NewPubKeyECDH(
			cfg.IdentityKey, cfg.KeyRing,
		),
		Signer:        cfg.Signer,
		FeeEstimator:  cfg.FeeEstimator,
		WitnessBeacon: witnessBeacon,
		SelfNode:      route.NewVertex(cfg.IdentityKey.PubKey),
		Funding: &FundingConfig{
			WalletController: cfg.WalletController,
			KeyRing:          cfg.KeyRing,
			NetParams:        cfg.NetParams,
			IdentityKey:      cfg.IdentityKey,
			ChannelAcceptor:  acceptor,
			RoutingPolicy: models.ForwardingPolicy{
				MinHTLCOut:    1,
				TimeLockDelta: 18,
			},
			NotifyWhenOnline: func(remote [33]byte,
				peerChan chan<- lnpeer.Peer) {

				if peer != nil && remote == peer.PubKey() {
					peerChan <- peer
				}
			},
			WatchNewChannel: func(*chanstate.OpenChannel,
				*btcec.PublicKey) error {

				return nil
			},
			NotifyPendingOpen: func(wirePoint wire.OutPoint,
				_ *chanstate.OpenChannel, _ *btcec.PublicKey) {

				_ = wirePoint
			},
		},
		Onchain: &OnchainConfig{
			ShouldWatchChannel:      cfg.ShouldWatchChannel,
			BeforeCommitmentPublish: cfg.BeforeCommitmentPublish,
			RecordFullyResolved:     cfg.RecordChannelFullyResolved,
		},
	})
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}

	peer, err = NewPeer(PeerConfig{
		RemoteKey: cfg.RemoteNodeKey,
		Transport: cfg.Transport,
		AddChannel: func(channel *lnpeer.NewChannel,
			_ <-chan struct{}) error {

			if err := runtime.WatchChannel(
				channel.OpenChannel,
			); err != nil {
				return err
			}
			linkConfig, err := runtime.NewOnchainLinkConfig(
				peer, channel.OpenChannel.FundingOutpoint,
				cfg.OnChannelFailure,
			)
			if err != nil {
				return err
			}
			if _, err := runtime.AddLink(
				channel.OpenChannel, linkConfig,
			); err != nil {
				return err
			}
			if cfg.OnChannelOpened != nil {
				cfg.OnChannelOpened(channel.OpenChannel)
			}

			return nil
		},
	})
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}

	fundingEndpoint, err := NewNativeFundingEndpoint(
		cfg.Party, runtime.Funding(), cfg.Signer, cfg.BackingKey,
	)
	if err != nil {
		if closeDB {
			_ = db.Close()
		}

		return nil, err
	}

	return &NativeNode{
		cfg: cfg, db: db, closeDB: closeDB, notifier: notifier,
		runtime: runtime, peer: peer,
		fundingEndpoint: fundingEndpoint,
	}, nil
}

// validateNativeNodeConfig rejects incomplete process composition before a
// channel database is opened.
func validateNativeNodeConfig(cfg NativeNodeConfig) error {
	switch {
	case cfg.Party != arkchannel.PartyClient &&
		cfg.Party != arkchannel.PartyHub:
		return fmt.Errorf("channel party is required")

	case cfg.DB == nil && cfg.DataDir == "":
		return fmt.Errorf("channel data directory is required")

	case cfg.Chain == nil:
		return fmt.Errorf("channel chain IO is required")

	case cfg.Notifier == nil:
		return fmt.Errorf("channel notifier is required")

	case cfg.WalletController == nil:
		return fmt.Errorf("channel wallet controller is required")

	case cfg.KeyRing == nil:
		return fmt.Errorf("channel key ring is required")

	case cfg.Signer == nil:
		return fmt.Errorf("channel signer is required")

	case cfg.FeeEstimator == nil:
		return fmt.Errorf("channel fee estimator is required")

	case cfg.NetParams == nil:
		return fmt.Errorf("channel network is required")

	case cfg.IdentityKey.PubKey == nil:
		return fmt.Errorf("channel identity key is required")

	case cfg.BackingKey.PubKey == nil:
		return fmt.Errorf("channel backing key is required")

	case cfg.RemoteNodeKey == nil:
		return fmt.Errorf("remote channel node key is required")

	case cfg.Transport == nil:
		return fmt.Errorf("channel peer transport is required")

	case cfg.Intents == nil:
		return fmt.Errorf("channel intent source is required")

	case cfg.ShouldWatchChannel == nil:
		return fmt.Errorf("on-chain channel admission is required")

	case cfg.BeforeCommitmentPublish == nil:
		return fmt.Errorf("commitment publication barrier is required")

	case cfg.RecordChannelFullyResolved == nil:
		return fmt.Errorf("channel resolution recorder is required")

	default:
		return nil
	}
}

// openNativeChannelDB opens the persistent lnd state owned by one Wavelength
// channel endpoint.
func openNativeChannelDB(dataDir string) (*channeldb.DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create channel data directory: %w", err)
	}
	backend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:     filepath.Clean(dataDir),
		DBFileName: channelDBFileName,
		DBTimeout:  channelDBTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("open channel database backend: %w", err)
	}
	db, err := channeldb.CreateWithBackend(backend)
	if err != nil {
		_ = backend.Close()

		return nil, fmt.Errorf("open channel database: %w", err)
	}

	return db, nil
}

// Start starts the native lnd channel and payment subsystems.
func (n *NativeNode) Start() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return nil
	}
	if n.stopped {
		return fmt.Errorf("native channel node already stopped")
	}
	if err := n.runtime.Start(); err != nil {
		return err
	}
	_, err := n.runtime.RestorePeerLinks(
		n.peer, func(state *chanstate.OpenChannel) (LinkConfig, error) {
			watch, err := n.cfg.ShouldWatchChannel(
				state.FundingOutpoint,
			)
			if err != nil {
				return LinkConfig{}, err
			}
			if !watch {
				return LinkConfig{}, fmt.Errorf("active "+
					"channel %v is not admitted to the "+
					"on-chain lifecycle",
					state.FundingOutpoint)
			}

			return n.runtime.NewOnchainLinkConfig(
				n.peer, state.FundingOutpoint,
				n.cfg.OnChannelFailure,
			)
		},
	)
	if err != nil {
		_ = n.runtime.Stop()

		return fmt.Errorf("restore native channel links: %w", err)
	}
	n.started = true

	return nil
}

// Stop stops native lnd state before closing a node-owned channel database.
func (n *NativeNode) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return nil
	}
	n.stopped = true

	var stopErr error
	if n.started {
		stopErr = n.runtime.Stop()
	}
	if n.closeDB {
		if err := n.db.Close(); stopErr == nil {
			stopErr = err
		}
	}

	return stopErr
}

// Runtime returns the native lnd subsystem composition.
func (n *NativeNode) Runtime() *Runtime {
	return n.runtime
}

// Peer returns the transport-backed logical lnd peer.
func (n *NativeNode) Peer() *Peer {
	return n.peer
}

// FundingEndpoint returns this process's validating funding counterparty.
func (n *NativeNode) FundingEndpoint() *NativeFundingEndpoint {
	return n.fundingEndpoint
}

// FundingActivator exposes synthetic confirmation for the signed backing.
func (n *NativeNode) FundingActivator() arkchannel.VirtualFundingActivator {
	return n.runtime.Funding()
}

// HandoffChannel verifies lnd owns the chain lifecycle before Ark publishes
// one channel's backing transaction.
func (n *NativeNode) HandoffChannel(channelPoint wire.OutPoint) error {
	return n.runtime.HandoffChannel(channelPoint)
}

// ForceCloseChannel asks lnd to close and resolve one handed-off channel.
func (n *NativeNode) ForceCloseChannel(channelPoint wire.OutPoint) (*wire.MsgTx,
	error) {

	return n.runtime.ForceCloseChannel(channelPoint)
}

// WaitForceCloseResult returns the commitment transaction lnd durably
// classified after either endpoint won a force-close publication race.
func (n *NativeNode) WaitForceCloseResult(ctx context.Context,
	channelPoint wire.OutPoint) (chainhash.Hash, error) {

	return n.runtime.WaitForceCloseResult(ctx, channelPoint)
}

// ResumeForceCloseChannel reconciles an already materialized channel with
// lnd's durable commitment-broadcast state after restart.
func (n *NativeNode) ResumeForceCloseChannel(channelPoint wire.OutPoint) error {
	return n.runtime.ResumeForceCloseChannel(channelPoint)
}

// NewNegotiator constructs the funder-side coordinator for this node.
func (n *NativeNode) NewNegotiator(remote FundingCounterparty,
	recovery ChannelRecoveryManager) (*ChannelNegotiator, error) {

	return NewChannelNegotiator(
		n.fundingEndpoint, remote, n.peer, recovery,
	)
}

// PeerMessageHandler returns the authenticated ingress for native BOLT
// messages.
func (n *NativeNode) PeerMessageHandler() PeerEventHandler {
	return n.runtime.PeerMessageHandler(n.peer)
}

// AddInvoice creates a native lnd invoice and returns its preimage and hash.
func (n *NativeNode) AddInvoice(ctx context.Context, amount btcutil.Amount) (
	lntypes.Preimage, lntypes.Hash, error) {

	if amount <= 0 {
		return lntypes.Preimage{}, lntypes.Hash{}, fmt.Errorf(
			"invoice amount must be positive")
	}
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return lntypes.Preimage{}, lntypes.Hash{}, err
	}
	hash, err := n.AddInvoiceWithPreimage(ctx, amount, preimage, false)
	if err != nil {
		return lntypes.Preimage{}, lntypes.Hash{}, err
	}

	return preimage, hash, nil
}

// AddInvoiceWithPreimage adds or validates one deterministic native invoice.
// A retry never changes amount, preimage, or hold semantics.
func (n *NativeNode) AddInvoiceWithPreimage(ctx context.Context,
	amount btcutil.Amount, preimage lntypes.Preimage, hold bool) (
	lntypes.Hash, error) {

	if amount <= 0 {
		return lntypes.Hash{}, fmt.Errorf("invoice amount must be " +
			"positive")
	}
	hash := preimage.Hash()
	invoice := &invoices.Invoice{
		CreationDate: time.Now(),
		HodlInvoice:  hold,
		Terms: invoices.ContractTerm{
			FinalCltvDelta:  18,
			Expiry:          time.Hour,
			PaymentPreimage: &preimage,
			Value: lnwire.NewMSatFromSatoshis(
				amount,
			),
			Features: emptyFeatureVector(),
		},
	}
	_, err := n.runtime.Invoices().AddInvoice(ctx, invoice, hash)
	if errors.Is(err, invoices.ErrDuplicateInvoice) {
		existing, lookupErr := n.runtime.Invoices().LookupInvoice(
			ctx, hash,
		)
		if lookupErr != nil {
			return lntypes.Hash{}, lookupErr
		}
		if err := validateNativeInvoice(
			existing, amount, hold,
		); err != nil {
			return lntypes.Hash{}, err
		}

		return hash, nil
	}
	if err != nil {
		return lntypes.Hash{}, err
	}

	return hash, nil
}

// AddHoldInvoice adds or validates a deterministic hash-only hold invoice.
func (n *NativeNode) AddHoldInvoice(ctx context.Context, hash lntypes.Hash,
	amount btcutil.Amount) error {

	if hash == (lntypes.Hash{}) {
		return fmt.Errorf("payment hash is required")
	}
	if amount <= 0 {
		return fmt.Errorf("invoice amount must be positive")
	}
	_, err := n.runtime.Invoices().AddInvoice(ctx, &invoices.Invoice{
		CreationDate: time.Now(),
		HodlInvoice:  true,
		Terms: invoices.ContractTerm{
			FinalCltvDelta: 18,
			Expiry:         time.Hour,
			Value: lnwire.NewMSatFromSatoshis(
				amount,
			),
			Features: emptyFeatureVector(),
		},
	}, hash)
	if errors.Is(err, invoices.ErrDuplicateInvoice) {
		existing, lookupErr := n.runtime.Invoices().LookupInvoice(
			ctx, hash,
		)
		if lookupErr != nil {
			return lookupErr
		}

		return validateNativeInvoice(existing, amount, true)
	}

	return err
}

// validateNativeInvoice rejects hash reuse with different immutable terms.
func validateNativeInvoice(invoice invoices.Invoice, amount btcutil.Amount,
	hold bool) error {

	if invoice.Terms.Value != lnwire.NewMSatFromSatoshis(amount) {
		return fmt.Errorf("native invoice already has another amount")
	}
	if invoice.HodlInvoice != hold {
		return fmt.Errorf("native invoice hold semantics changed")
	}
	if invoice.State == invoices.ContractCanceled {
		return fmt.Errorf("native invoice is canceled")
	}

	return nil
}

// WaitInvoiceAccepted waits for a hold invoice to own at least one accepted
// HTLC. A settled invoice also satisfies this recovery barrier.
func (n *NativeNode) WaitInvoiceAccepted(ctx context.Context,
	hash lntypes.Hash) error {

	return n.waitInvoiceState(ctx, hash, func(invoice *invoices.Invoice) (
		bool, error) {

		switch invoice.State {
		case invoices.ContractAccepted, invoices.ContractSettled:
			return true, nil

		case invoices.ContractCanceled:
			return false, fmt.Errorf("native invoice was canceled")

		default:
			return false, nil
		}
	})
}

// WaitInvoiceSettled waits until the native invoice registry has accepted a
// valid preimage for this hash.
func (n *NativeNode) WaitInvoiceSettled(ctx context.Context,
	hash lntypes.Hash) error {

	return n.waitInvoiceState(ctx, hash, func(invoice *invoices.Invoice) (
		bool, error) {

		switch invoice.State {
		case invoices.ContractSettled:
			return true, nil

		case invoices.ContractCanceled:
			return false, fmt.Errorf("native invoice was canceled")

		default:
			return false, nil
		}
	})
}

// waitInvoiceState subscribes before waiting so no accepted or settled update
// can be lost between a lookup and subscription.
func (n *NativeNode) waitInvoiceState(ctx context.Context, hash lntypes.Hash,
	terminal func(*invoices.Invoice) (bool, error)) error {

	subscription, err := n.runtime.Invoices().SubscribeSingleInvoice(
		ctx, hash,
	)
	if err != nil {
		return err
	}
	defer subscription.Cancel()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case invoice, ok := <-subscription.Updates:
			if !ok {
				return fmt.Errorf("native invoice " +
					"subscription closed")
			}
			done, err := terminal(invoice)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// SettleHoldInvoice releases a native hold invoice with its matching preimage.
func (n *NativeNode) SettleHoldInvoice(ctx context.Context,
	preimage lntypes.Preimage) error {

	err := n.runtime.Invoices().SettleHodlInvoice(ctx, preimage)
	if errors.Is(err, invoices.ErrInvoiceAlreadySettled) {
		return nil
	}

	return err
}

// CancelInvoice fails a native hold invoice while no preimage is known.
func (n *NativeNode) CancelInvoice(ctx context.Context,
	hash lntypes.Hash) error {

	err := n.runtime.Invoices().CancelInvoice(ctx, hash)
	if errors.Is(err, invoices.ErrInvoiceAlreadyCanceled) {
		return nil
	}

	return err
}

// ChannelBalance returns the latest local and remote balances for an active
// native channel owned by this endpoint.
func (n *NativeNode) ChannelBalance(record arkchannel.Record) (btcutil.Amount,
	btcutil.Amount, error) {

	if record.Snapshot.Phase != arkchannel.PhaseActive ||
		record.Snapshot.Backing == nil {
		return 0, 0, fmt.Errorf("Ark channel is not active")
	}
	terms := record.Snapshot.Terms
	if _, err := n.runtime.GetLink(
		lnwire.NewShortChanIDFromInt(terms.ReservedSCID),
	); err != nil {
		return 0, 0, fmt.Errorf("Ark channel link is inactive: %w", err)
	}
	channel, err := n.db.ChannelStateDB().FetchChannel(
		record.Snapshot.Backing.ChannelPoint,
	)
	if err != nil {
		return 0, 0, err
	}
	if err := channel.Refresh(); err != nil {
		return 0, 0, err
	}
	local, _, err := channel.LatestCommitments()
	if err != nil {
		return 0, 0, err
	}

	return local.LocalBalance.ToSatoshis(),
		local.RemoteBalance.ToSatoshis(), nil
}

// PayInvoiceResult sends or resumes one fixed one-hop payment and returns the
// destination preimage recorded by lnd's control tower.
func (n *NativeNode) PayInvoiceResult(ctx context.Context,
	record arkchannel.Record, hash lntypes.Hash, amount btcutil.Amount) (
	lntypes.Preimage, error) {

	preimage, found, err := n.existingPaymentResult(ctx, hash)
	if err != nil {
		return lntypes.Preimage{}, err
	}
	if found {
		return preimage, nil
	}

	attempt, err := n.sendInvoiceAttempt(ctx, record, hash, amount)
	if err == nil && attempt.Settle != nil {
		return attempt.Settle.Preimage, nil
	}
	if err == nil {
		return lntypes.Preimage{}, fmt.Errorf("native lnd payment " +
			"did not settle")
	}

	// SendToRoute reports an already-known payment on replay. The control
	// tower remains authoritative for whether that attempt settled.
	preimage, found, lookupErr := n.existingPaymentResult(ctx, hash)
	if lookupErr != nil {
		return lntypes.Preimage{}, fmt.Errorf("determine native "+
			"payment state after dispatch: %w", lookupErr)
	}
	if found {
		return preimage, nil
	}

	return lntypes.Preimage{}, &PaymentNotStartedError{Err: err}
}

// existingPaymentResult returns a terminal preimage or waits for an in-flight
// payment that survived process restart.
func (n *NativeNode) existingPaymentResult(ctx context.Context,
	hash lntypes.Hash) (lntypes.Preimage, bool, error) {

	control := n.runtime.Payments().ControlTower()
	payment, err := control.FetchPayment(ctx, hash)
	if errors.Is(err, paymentsdb.ErrPaymentNotInitiated) {
		return lntypes.Preimage{}, false, nil
	}
	if err != nil {
		return lntypes.Preimage{}, false, err
	}
	if payment.Terminated() {
		return terminalPaymentPreimage(payment)
	}
	subscriber, err := control.SubscribePayment(hash)
	if err != nil {
		return lntypes.Preimage{}, false, err
	}
	defer subscriber.Close()

	for {
		select {
		case <-ctx.Done():
			return lntypes.Preimage{}, false, ctx.Err()

		case update, ok := <-subscriber.Updates():
			if !ok {
				return lntypes.Preimage{}, false, fmt.Errorf(
					"native payment subscription " +
						"closed")
			}
			payment, ok := update.(paymentsdb.DBMPPayment)
			if !ok || !payment.Terminated() {
				continue
			}

			return terminalPaymentPreimage(payment)
		}
	}
}

// terminalPaymentPreimage extracts the successful terminal attempt.
func terminalPaymentPreimage(payment paymentsdb.DBMPPayment) (lntypes.Preimage,
	bool, error) {

	attempt, failure := payment.TerminalInfo()
	if failure != nil {
		return lntypes.Preimage{}, false, &TerminalPaymentError{
			Reason: failure.String(),
		}
	}
	if attempt == nil || attempt.Settle == nil {
		return lntypes.Preimage{}, false, fmt.Errorf("native payment " +
			"terminated without preimage")
	}

	return attempt.Settle.Preimage, true, nil
}

// sendInvoiceAttempt constructs and dispatches one private one-hop route.
func (n *NativeNode) sendInvoiceAttempt(ctx context.Context,
	record arkchannel.Record, hash lntypes.Hash, amount btcutil.Amount) (
	*paymentsdb.HTLCAttempt, error) {

	if record.Snapshot.Phase != arkchannel.PhaseActive ||
		record.Snapshot.Backing == nil {
		return nil, fmt.Errorf("Ark channel is not active")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("payment amount must be positive")
	}
	_, height, err := n.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("read payment height: %w", err)
	}
	if height < 0 {
		return nil, fmt.Errorf("invalid payment height %d", height)
	}

	terms := record.Snapshot.Terms
	remoteNode := terms.HubNodeKey
	if n.cfg.Party == arkchannel.PartyHub {
		remoteNode = terms.ClientNodeKey
	}
	lockTime, err := arkChannelPaymentLockTime(
		uint32(height), terms.VTXO,
	)
	if err != nil {
		return nil, err
	}
	msat := lnwire.NewMSatFromSatoshis(amount)
	paymentRoute := &route.Route{
		TotalTimeLock: lockTime,
		TotalAmount:   msat,
		SourcePubKey:  route.NewVertex(n.cfg.IdentityKey.PubKey),
		Hops: []*route.Hop{{
			PubKeyBytes:      route.Vertex(remoteNode),
			ChannelID:        terms.ReservedSCID,
			OutgoingTimeLock: lockTime,
			AmtToForward:     msat,
			LegacyPayload:    true,
		}},
	}

	return n.runtime.Payments().SendToOperator(
		ctx, hash, paymentRoute, nil,
	)
}

// arkChannelPaymentLockTime keeps a private HTLC enforceable while the Ark
// source follows its non-interactive recovery path onto the chain. The funder
// delay contains both the channel materialization delay and its reaction
// window, so the ordinary Lightning margin starts after that entire horizon.
func arkChannelPaymentLockTime(height uint32,
	terms arkchannel.VTXOTerms) (uint32, error) {

	if terms.FunderDelay < terms.ChannelDelay {
		return 0, fmt.Errorf("channel funder delay %d is shorter than "+
			"materialization delay %d", terms.FunderDelay,
			terms.ChannelDelay)
	}
	if terms.FunderDelay > maximumBlockHeight-privatePaymentCLTVDelta {
		return 0, fmt.Errorf("channel payment CLTV delta overflows")
	}
	delta := terms.FunderDelay + privatePaymentCLTVDelta
	if height > maximumBlockHeight-delta {
		return 0, fmt.Errorf("channel payment locktime overflows")
	}

	return height + delta, nil
}

// PayInvoice sends one fixed one-hop payment over an active Ark channel.
func (n *NativeNode) PayInvoice(ctx context.Context, record arkchannel.Record,
	hash lntypes.Hash, amount btcutil.Amount) error {

	_, err := n.PayInvoiceResult(ctx, record, hash, amount)

	return err
}

// InvoiceSettled reports whether the native invoice registry has accepted the
// preimage for one payment hash.
func (n *NativeNode) InvoiceSettled(ctx context.Context, hash lntypes.Hash) (
	bool, error) {

	invoice, err := n.runtime.Invoices().LookupInvoice(ctx, hash)
	if err != nil {
		return false, err
	}

	return invoice.State == invoices.ContractSettled, nil
}
