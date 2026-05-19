//go:build !js || !wasm

package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// hintCacheDBName is the filename for the height hint cache database.
const hintCacheDBName = "heighthint.db"

// Wallet is a lightweight in-process Bitcoin wallet backed by LND's
// btcwallet implementation and a neutrino (BIP 157/158) chain
// backend. It embeds walletcore.Wallet for shared btcwallet
// operations and adds neutrino-specific chain service, chain backend,
// and boarding backend.
//
// The wallet exposes sub-interfaces via accessor methods:
//   - BoardingBackend() returns the wallet.BoardingBackend adapter
//   - Signer() returns the input.Signer implementation
//   - ChainBackend() returns the chainsource.ChainBackend for actors
//   - KeyRing() returns the keychain.SecretKeyRing for key operations
type Wallet struct {
	// Wallet provides shared btcwallet-backed operations.
	walletcore.Wallet

	// neutrinoSvc manages the neutrino chain service lifecycle.
	neutrinoSvc *NeutrinoService

	// chainBackend implements chainsource.ChainBackend for the
	// actor system (confirmation/spend/block registrations).
	chainBackend *ChainBackend

	// boardingBackend wraps btcwallet to provide the
	// wallet.BoardingBackend interface for Ark boarding.
	boardingBackend *BoardingBackendAdapter

	// ownsNeutrino is true when the wallet created and owns the
	// neutrino service lifecycle (created via New). When false
	// (created via NewWithNeutrino), the caller manages neutrino
	// shutdown.
	ownsNeutrino bool
}

// New creates a new neutrino-backed wallet from the given
// configuration. The wallet creates and owns its own neutrino
// service, which is stopped when the wallet is stopped.
func New(cfg Config) (*Wallet, error) {
	walletLog := cfg.Log.UnwrapOr(btclog.Disabled)
	neutrinoDataDir := cfg.neutrinoDataDir()
	var neutrinoOpts []NeutrinoServiceOption
	if cfg.DisableGlobalLoggers {
		neutrinoOpts = append(
			neutrinoOpts, WithoutGlobalDependencyLoggers(),
		)
	}

	// Create and start the neutrino chain service.
	neutrinoSvc, err := NewNeutrinoService(
		neutrinoDataDir, cfg.ChainParams, cfg.ConnectPeers,
		cfg.AddPeers, cfg.PersistFilters, walletLog, neutrinoOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("create neutrino service: %w", err)
	}

	if err := neutrinoSvc.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("start neutrino service: %w", err)
	}

	w, err := NewWithNeutrino(cfg, neutrinoSvc)
	if err != nil {
		_ = neutrinoSvc.Stop()

		return nil, err
	}

	// Mark that this wallet owns the neutrino service lifecycle.
	w.ownsNeutrino = true

	return w, nil
}

// NewWithNeutrino creates a new neutrino-backed wallet using a
// pre-started NeutrinoService. The caller retains ownership of the
// neutrino service lifecycle — the wallet will NOT stop it on
// Wallet.Stop(). This allows the daemon to start neutrino early
// (for P2P connection and header sync) independently of wallet
// unlock timing.
func NewWithNeutrino(cfg Config,
	neutrinoSvc *NeutrinoService) (*Wallet, error) {

	walletLog := cfg.Log.UnwrapOr(btclog.Disabled)
	neutrinoDataDir := cfg.neutrinoDataDir()

	// Create the btcwallet chain client backed by neutrino.
	chainClient := neutrinoSvc.ChainClient()

	coinType := walletcore.CoinTypeForNet(cfg.ChainParams)
	blockCache := neutrinoSvc.BlockCache()

	btcw, err := btcwallet.New(btcwallet.Config{
		PrivatePass:    walletcore.WalletPassphrase,
		PublicPass:     walletcore.WalletPassphrase,
		HdSeed:         cfg.Seed[:],
		ChainSource:    chainClient,
		NetParams:      cfg.ChainParams,
		CoinType:       coinType,
		RecoveryWindow: cfg.RecoveryWindow,
		LoaderOptions: []btcwallet.LoaderOption{
			btcwallet.LoaderWithLocalWalletDB(
				cfg.DBDir, false, 60*time.Second,
			),
		},
	}, blockCache)
	if err != nil {
		return nil, fmt.Errorf("create btcwallet: %w", err)
	}

	// Create the keyring from btcwallet's internal wallet.
	keyRing := keychain.NewBtcWalletKeyRing(
		btcw.InternalWallet(), coinType,
	)

	// Create the chain backend with neutrino notifier and fee
	// estimation.
	hintDBPath := filepath.Join(neutrinoDataDir, hintCacheDBName)
	chainBackend, err := NewChainBackend(
		neutrinoSvc, cfg.FeeURL, cfg.feeMinTimeout(),
		cfg.feeMaxTimeout(), hintDBPath, walletLog,
	)
	if err != nil {
		_ = btcw.Stop()

		return nil, fmt.Errorf("create chain backend: %w", err)
	}
	chainBackend.SetPackageSubmitter(cfg.PackageSubmitter)

	// Create the boarding backend adapter.
	boardingBackend := NewBoardingBackendAdapter(
		btcw, neutrinoSvc.ChainService(), blockCache, cfg.ChainParams,
		coinType, walletLog,
	)

	walletLog.InfoS(context.Background(), "Neutrino-backed wallet created",
		slog.String("db_dir", cfg.DBDir),
		slog.String("neutrino_dir", neutrinoDataDir),
		slog.Uint64("coin_type", uint64(coinType)),
	)

	return &Wallet{
		Wallet: walletcore.Wallet{
			Signer:      btcw,
			BtcWallet:   btcw,
			KeyRing:     keyRing,
			ChainParams: cfg.ChainParams,
			WalletLog:   cfg.Log,
		},
		neutrinoSvc:     neutrinoSvc,
		chainBackend:    chainBackend,
		boardingBackend: boardingBackend,
	}, nil
}

// Start initializes the wallet by starting btcwallet (which
// internally starts the chain client and syncs the wallet) and the
// chainsource ChainBackend.
func (w *Wallet) Start() error {
	ctx := context.Background()

	// btcWallet.Start() unlocks the wallet, creates key scopes,
	// starts the chain client, and begins wallet synchronization.
	if err := w.BtcWallet.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}

	// Start the chain backend. This is idempotent (sync.Once) so
	// it is safe even if the daemon's startBtcwallet also calls
	// Start().
	if err := w.chainBackend.Start(); err != nil {
		return fmt.Errorf("start chain backend: %w", err)
	}

	w.Logger(ctx).InfoS(ctx, "Neutrino-backed wallet started")

	return nil
}

// Stop shuts down the wallet and, if the wallet owns the neutrino
// service (created via New), stops it too. When created via
// NewWithNeutrino, the caller manages the neutrino lifecycle.
func (w *Wallet) Stop() {
	ctx := context.Background()

	w.Logger(ctx).InfoS(ctx, "Stopping neutrino-backed wallet")

	// Note: chainBackend is NOT stopped here — the daemon's
	// server.go defer owns that lifecycle. The ChainBackend.Stop()
	// is idempotent (sync.Once) so it is safe even if called from
	// both Wallet and server.
	//
	// Stop order: btcwallet (depends on neutrino chain client)
	// must stop before neutrino service.
	_ = w.BtcWallet.Stop()
	if err := w.BtcWallet.InternalWallet().Database().Close(); err != nil {
		w.Logger(ctx).WarnS(ctx, "Failed to close btcwallet DB", err)
	}

	if w.ownsNeutrino {
		_ = w.neutrinoSvc.Stop()
	}

	w.Logger(ctx).InfoS(ctx, "Neutrino-backed wallet stopped")
}

// BoardingBackend returns the wallet.BoardingBackend adapter that
// wraps btcwallet for Ark boarding address management.
func (w *Wallet) BoardingBackend() *BoardingBackendAdapter {
	return w.boardingBackend
}

// ChainBackend returns the chainsource.ChainBackend used by the
// actor system for confirmation, spend, and block registrations.
func (w *Wallet) ChainBackend() *ChainBackend {
	return w.chainBackend
}

// KeyRing returns the wallet's secret key ring for key derivation
// and message signing operations.
func (w *Wallet) KeyRing() keychain.SecretKeyRing {
	return w.Wallet.KeyRing
}

// IsSynced returns whether the underlying btcwallet has fully synced
// to the current best block. This includes completion of any recovery
// scan. Callers should poll this before marking the wallet ready to
// ensure the chain notification pipeline is fully operational.
func (w *Wallet) IsSynced() (bool, int64, error) {
	return w.BtcWallet.IsSynced()
}
