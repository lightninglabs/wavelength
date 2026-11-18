package waved

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lndbackend"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ArkChannelUnrollWallet is the fee-input and sweep wallet surface shared by
// txconfirm and the ordinary VTXO unroller.
type ArkChannelUnrollWallet interface {
	txconfirm.Wallet
	unroll.SweepWallet
}

// NewLNDArkChannelUnrollWallet adapts swapd's existing LND wallet for channel
// source recovery and CPFP fee management.
func NewLNDArkChannelUnrollWallet(signer lndclient.SignerClient,
	walletKit lndclient.WalletKitClient,
	chainKit lndclient.ChainKitClient) ArkChannelUnrollWallet {

	return &lndUnrollWallet{
		ClientWallet: lndbackend.NewClientWallet(signer, walletKit),
		boardingBackend: lndbackend.NewBoardingBackend(
			walletKit, chainKit,
		),
	}
}

// ArkChannelRecoveryRuntimeConfig contains the process-wide durable store and
// process-owned chain dependencies needed by the common unroller.
type ArkChannelRecoveryRuntimeConfig struct {
	Store        *db.Store
	ChannelStore *db.ArkChannelStoreDB
	ChainBackend chainsource.ChainBackend
	Wallet       ArkChannelUnrollWallet
	Log          btclog.Logger
}

// ArkChannelRecoveryRuntime owns the operator's unroll actors and expiry feed.
// Endpoint-specific watches deliver events only to their own channel service.
type ArkChannelRecoveryRuntime struct {
	actorSystem *actor.ActorSystem
	registry    *unroll.UnrollRegistryActor
	cfg         ArkChannelRecoveryRuntimeConfig
	tipMu       sync.Mutex
	height      int32
	nextTip     chan struct{}
	stopTip     context.CancelFunc
	tipDone     chan struct{}
}

// NewArkChannelRecoveryRuntime composes the same unroller used by waved while
// leaving ownership of the database, chain backend, and wallet with swapd.
//
//nolint:contextcheck // recovery actors own lifecycle until runtime Stop
func NewArkChannelRecoveryRuntime(ctx context.Context,
	cfg ArkChannelRecoveryRuntimeConfig) (*ArkChannelRecoveryRuntime,
	error) {

	switch {
	case cfg.Store == nil || cfg.ChannelStore == nil:
		return nil, fmt.Errorf("Ark channel recovery store is required")

	case cfg.ChainBackend == nil:
		return nil, fmt.Errorf("Ark channel recovery chain backend " +
			"is required")

	case cfg.Wallet == nil:
		return nil, fmt.Errorf("Ark channel recovery wallet is " +
			"required")
	}
	log := cfg.Log
	if log == nil {
		log = btclog.Disabled
	}
	actorSystem := actor.NewActorSystemWithConfig(actor.SystemConfig{
		MailboxCapacity: 100, Log: fn.Some(log),
	})
	cleanup := true
	defer func() {
		if cleanup {
			shutdownCtx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			_ = actorSystem.Shutdown(shutdownCtx)
		}
	}()
	chainActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: cfg.ChainBackend, System: actorSystem,
			FinalityDepth: chainsource.DefaultFinalityDepth,
		},
	)
	chainSource := actor.RegisterWithSystem(
		actorSystem, "ark-channel-chain-source",
		chainsource.ChainSourceKey, chainActor,
	)
	deliveryStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		cfg.Store.DB(), cfg.Store.Backend(), clock.NewDefaultClock(),
		log,
	)
	if err != nil {
		return nil, err
	}
	txConfirm := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource: chainSource, Wallet: cfg.Wallet,
		Log: fn.Some(log),
	})
	txConfirmRef := actor.RegisterWithSystem(
		actorSystem, txconfirm.ServiceKeyName,
		txconfirm.NewServiceKey(), txConfirm,
	)
	txConfirm.SetSelfRef(txConfirmRef)
	vtxoStore := cfg.Store.NewVTXOStore(clock.NewDefaultClock())
	oorStore := cfg.Store.NewOORArtifactStore(clock.NewDefaultClock())
	registry := unroll.NewUnrollRegistryActor(unroll.RegistryConfig{
		Store: &unroll.DBRegistryStore{
			UEStore: cfg.Store.NewUnilateralExitStore(
				clock.NewDefaultClock(),
			),
		},
		DeliveryStore: deliveryStore,
		ProofAssembler: &unroll.LocalProofAssembler{
			VTXOStore: vtxoStore, ArtifactStore: oorStore,
		},
		VTXOStore: vtxoStore, TxConfirmRef: txConfirmRef,
		ChainSource: chainSource, Wallet: cfg.Wallet,
		LedgerSink: fn.None[ledger.Sink](),
		Log:        fn.Some(log),
		ExitSpendPolicyResolver: unroll.PolicyResolvers{
			unrollbridge.Resolver{Channels: cfg.ChannelStore},
		},
	})
	if err := registry.RestoreNonTerminal(ctx); err != nil {
		registry.Stop()

		return nil, fmt.Errorf("restore Ark channel unrolls: %w", err)
	}
	cleanup = false

	tipCtx, stopTip := context.WithCancel(ctx)
	runtime := &ArkChannelRecoveryRuntime{
		actorSystem: actorSystem, registry: registry,
		cfg: cfg, height: -1, nextTip: make(chan struct{}),
		stopTip: stopTip, tipDone: make(chan struct{}),
	}
	go func() {
		defer close(runtime.tipDone)
		watchSourceExpiryTip(
			tipCtx, cfg.ChainBackend, clock.NewDefaultClock(), log,
			runtime.publishTip,
		)
	}()

	return runtime, nil
}

// publishTip wakes all endpoint guards without blocking on a slow peer.
func (r *ArkChannelRecoveryRuntime) publishTip(height int32) {
	r.tipMu.Lock()
	defer r.tipMu.Unlock()
	r.height = height
	close(r.nextTip)
	r.nextTip = make(chan struct{})
}

// SourceExpiryTip captures the tip and subsequent wakeup atomically, so
// simultaneous registration and a threshold crossing cannot lose the block.
func (r *ArkChannelRecoveryRuntime) SourceExpiryTip() (int32, <-chan struct{}) {
	r.tipMu.Lock()
	defer r.tipMu.Unlock()

	return r.height, r.nextTip
}

// NewEndpoint binds a peer's source observations to its own FSM. The actors,
// chain source, unroll jobs, and durable evidence belong to the hub process.
func (r *ArkChannelRecoveryRuntime) NewEndpoint() (ArkChannelRecoveryController,
	*unrollbridge.Controller, error) {

	clk := clock.NewDefaultClock()
	recovery, err := newArkChannelRecoveryArchive(
		r.cfg.Store.NewVTXOStore(clk),
		r.cfg.Store.NewOORArtifactStore(clk), r.cfg.ChainBackend,
		r.cfg.Log,
	)
	if err != nil {
		return nil, nil, err
	}
	recovery.importOnly = true
	materializer, err := unrollbridge.NewController(
		r.registry.Ref(), recovery,
	)
	if err != nil {
		recovery.Stop()

		return nil, nil, err
	}

	return recovery, materializer, nil
}

// Stop releases watches and actors without stopping externally owned chain or
// wallet backends.
func (r *ArkChannelRecoveryRuntime) Stop() error {
	if r == nil {
		return nil
	}
	r.stopTip()
	<-r.tipDone
	if r.registry != nil {
		r.registry.Stop()
	}
	if r.actorSystem == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return r.actorSystem.Shutdown(ctx)
}
