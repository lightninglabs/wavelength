package waved

import (
	"context"
	"fmt"
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

// ArkChannelRecoveryRuntimeConfig contains the per-client durable store and
// process-owned chain dependencies needed by the common unroller.
type ArkChannelRecoveryRuntimeConfig struct {
	Store        *db.Store
	ChannelStore *db.ArkChannelStoreDB
	ChainBackend chainsource.ChainBackend
	Wallet       ArkChannelUnrollWallet
	Log          btclog.Logger
}

// ArkChannelRecoveryRuntime owns the hub endpoint's unroll actors, package
// archive, source watches, and channel materializer.
type ArkChannelRecoveryRuntime struct {
	actorSystem  *actor.ActorSystem
	registry     *unroll.UnrollRegistryActor
	recovery     *arkChannelRecoveryArchive
	materializer *unrollbridge.Controller
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
	recovery, err := newArkChannelRecoveryArchive(
		vtxoStore, oorStore, cfg.ChainBackend, log,
	)
	if err != nil {
		return nil, err
	}
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
		recovery.Stop()

		return nil, fmt.Errorf("restore Ark channel unrolls: %w", err)
	}
	materializer, err := unrollbridge.NewController(
		registry.Ref(), recovery,
	)
	if err != nil {
		registry.Stop()
		recovery.Stop()

		return nil, err
	}
	cleanup = false

	return &ArkChannelRecoveryRuntime{
		actorSystem: actorSystem, registry: registry,
		recovery: recovery, materializer: materializer,
	}, nil
}

// Recovery returns the package archive and source watcher.
func (r *ArkChannelRecoveryRuntime) Recovery() ArkChannelRecoveryController {
	return r.recovery
}

// Materializer returns the common-unroller channel adapter.
func (r *ArkChannelRecoveryRuntime) Materializer() *unrollbridge.Controller {
	return r.materializer
}

// Stop releases watches and actors without stopping externally owned chain or
// wallet backends.
func (r *ArkChannelRecoveryRuntime) Stop() error {
	if r == nil {
		return nil
	}
	if r.recovery != nil {
		r.recovery.Stop()
	}
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
