package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// NativeNodeConfig contains the process-owned dependencies for one modular
// lnd channel endpoint.
type NativeNodeConfig struct {
	DB *channeldb.DB

	// OwnsDB transfers cleanup responsibility to the node, including
	// constructor failure. Otherwise the caller retains ownership.
	OwnsDB bool

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

	OnChannelOpened          func(*chanstate.OpenChannel)
	OnChannelFailure         LinkFailureHandler
	OnChannelRestoreFailure  func(wire.OutPoint, error)
	ChannelLifecycle         contractcourt.AuxChannelLifecycle
	ShouldDisableChannelAdds func(wire.OutPoint) (bool, error)
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

	mu         sync.Mutex
	started    bool
	stopped    bool
	restoreErr error
}

// NewNativeNode composes one endpoint without starting its lnd goroutines.
func NewNativeNode(cfg NativeNodeConfig) (*NativeNode, error) {
	if err := validateNativeNodeConfig(cfg); err != nil {
		if cfg.OwnsDB && cfg.DB != nil {
			_ = cfg.DB.Close()
		}

		return nil, err
	}

	db := cfg.DB
	closeDB := cfg.OwnsDB

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
			ChannelLifecycle: cfg.ChannelLifecycle,
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

			// funding.Manager calls AddNewChannel only after the
			// fresh channel_ready exchange. Reestablishment is
			// reserved for links reconstructed from disk; enabling
			// it here can race a redundant channel_reestablish
			// against this initial link installation.
			linkConfig.SyncStates = false
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

	case cfg.DB == nil:
		return fmt.Errorf("channel database is required")

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

	case cfg.ChannelLifecycle == nil:
		return fmt.Errorf("auxiliary channel lifecycle is required")

	default:
		return nil
	}
}

// Start starts the native lnd channel and payment subsystems.
func (n *NativeNode) Start() error {
	n.mu.Lock()
	if n.started {
		n.mu.Unlock()

		return nil
	}
	if n.stopped {
		n.mu.Unlock()

		return fmt.Errorf("native channel node already stopped")
	}
	if err := n.runtime.Start(); err != nil {
		n.mu.Unlock()

		return err
	}
	_, err := n.runtime.RestorePeerLinks(n.peer, n.restoredLinkConfig)
	if err != nil {
		var restoreErr *PeerLinkRestoreError
		if !errors.As(err, &restoreErr) {
			_ = n.runtime.Stop()
			n.mu.Unlock()

			return fmt.Errorf("restore native channel links: %w",
				err)
		}
		n.restoreErr = restoreErr
	}
	n.started = true
	restoreErr := n.restoreErr
	reportFailure := n.cfg.OnChannelRestoreFailure
	n.mu.Unlock()

	if reportFailure != nil && restoreErr != nil {
		var failures *PeerLinkRestoreError
		if errors.As(restoreErr, &failures) {
			for _, failure := range failures.Failures {
				reportFailure(failure.ChannelPoint, failure.Err)
			}
		}
	}

	return nil
}

// restoredLinkConfig applies durable Ark admission and cooperative-close state
// whenever lnd reconstructs one persisted channel link.
func (n *NativeNode) restoredLinkConfig(state *chanstate.OpenChannel) (
	LinkConfig, error) {

	owner, err := n.cfg.ChannelLifecycle.ChainWatchOwner(
		context.Background(), state,
	)
	if err != nil {
		return LinkConfig{}, err
	}
	if owner != contractcourt.ChainWatchOwnerLnd {
		return LinkConfig{}, fmt.Errorf("active channel %v is not "+
			"owned by lnd's on-chain lifecycle (owner=%v)",
			state.FundingOutpoint, owner)
	}
	if err := n.runtime.WatchChannel(state); err != nil {
		return LinkConfig{}, err
	}

	linkConfig, err := n.runtime.NewOnchainLinkConfig(
		n.peer, state.FundingOutpoint, n.cfg.OnChannelFailure,
	)
	if err != nil {
		return LinkConfig{}, err
	}
	if n.cfg.ShouldDisableChannelAdds != nil {
		disabled, err := n.cfg.ShouldDisableChannelAdds(
			state.FundingOutpoint,
		)
		if err != nil {
			return LinkConfig{}, err
		}
		linkConfig.AddsDisabled = disabled
	}

	return linkConfig, nil
}

// RestoreError returns the per-channel failures retained during startup.
func (n *NativeNode) RestoreError() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.restoreErr
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

// RestoreBacking registers one durable virtual funding transaction before the
// native funding manager restores pending channels from lnd's database.
func (n *NativeNode) RestoreBacking(terms arkchannel.Terms,
	backing arkchannel.Backing) error {

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return fmt.Errorf("cannot restore backing after native node " +
			"start")
	}
	if n.stopped {
		return fmt.Errorf("native channel node already stopped")
	}

	return n.runtime.Funding().RestoreBacking(terms, backing)
}

// HandoffChannel verifies lnd owns the chain lifecycle before Ark publishes
// one channel's backing transaction.
func (n *NativeNode) HandoffChannel(channelPoint wire.OutPoint) error {
	return n.runtime.HandoffChannel(channelPoint)
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
	return func(ctx context.Context, message lnwire.Message) error {
		reestablish, ok := message.(*lnwire.ChannelReestablish)
		if ok {
			return n.runtime.HandleChannelReestablish(
				reestablish, n.peer, n.restoredLinkConfig,
			)
		}

		return n.runtime.HandlePeerMessage(ctx, message, n.peer)
	}
}
