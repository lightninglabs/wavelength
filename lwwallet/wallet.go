package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/walletcore"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// Wallet is a lightweight in-process Bitcoin wallet backed by LND's
// btcwallet implementation and an Esplora chain backend. It provides
// full on-chain wallet capabilities (receive, balance, key derivation,
// signing, MuSig2) plus Ark protocol participation (boarding address
// management, chain monitoring).
//
// The wallet embeds walletcore.Wallet for shared btcwallet operations
// and adds Esplora-specific chain service, chain backend, and
// boarding backend.
//
// The wallet exposes sub-interfaces via accessor methods:
//   - BoardingBackend() returns the wallet.BoardingBackend adapter
//   - Signer() returns the input.Signer implementation
//   - ChainBackend() returns the chainsource.ChainBackend for actors
//   - KeyRing() returns the keychain.SecretKeyRing for key operations
type Wallet struct {
	// Wallet provides shared btcwallet-backed operations.
	walletcore.Wallet

	// chainSvc implements btcwallet's chain.Interface, feeding
	// block notifications to btcwallet for wallet sync.
	chainSvc *EsploraChainService

	// esplora is the Esplora REST API client shared by chain
	// service and chain backend.
	esplora *EsploraClient

	// chainBackend implements chainsource.ChainBackend for the
	// actor system (confirmation/spend/block registrations).
	chainBackend *ChainBackend

	// boardingBackend wraps btcwallet to provide the
	// wallet.BoardingBackend interface for Ark boarding.
	boardingBackend *BoardingBackendAdapter
}

// New creates a new lightweight wallet from the given configuration.
// The caller must provide a DBDir for btcwallet's bbolt database and
// is responsible for managing the directory's lifecycle (creation
// before calling New, cleanup after Stop if desired).
func New(cfg Config) (*Wallet, error) {
	// Constructors run before a contextual logger is guaranteed,
	// so default to a disabled logger when one was not explicitly
	// provided.
	walletLog := cfg.Log.UnwrapOr(btclog.Disabled)

	esplora := NewEsploraClient(cfg.EsploraURL, walletLog)

	// The EsploraChainService implements btcwallet's chain.Interface
	// and feeds block notifications to btcwallet for wallet sync.
	chainSvc := NewEsploraChainService(
		esplora, cfg.PollInterval, walletLog,
	)

	// The ChainBackend implements chainsource.ChainBackend for the
	// actor system (confirmation/spend/block registrations).
	chainBackend := NewChainBackend(
		esplora, cfg.PollInterval, walletLog,
	)

	coinType := walletcore.CoinTypeForNet(cfg.ChainParams)
	blockCache := blockcache.NewBlockCache(
		walletcore.DefaultBlockCacheSize,
	)

	btcw, err := btcwallet.New(btcwallet.Config{
		PrivatePass:    walletcore.WalletPassphrase,
		PublicPass:     walletcore.WalletPassphrase,
		HdSeed:         cfg.Seed[:],
		ChainSource:    chainSvc,
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

	// Create the keyring from btcwallet's internal wallet. This
	// provides HD key derivation using the same m/1017'/coinType'
	// scope as LND.
	keyRing := keychain.NewBtcWalletKeyRing(
		btcw.InternalWallet(), coinType,
	)

	boardingBackend := NewBoardingBackendAdapter(
		btcw, esplora, cfg.ChainParams, coinType, walletLog,
	)

	walletLog.InfoS(context.Background(), "Lightweight wallet created",
		slog.String("db_dir", cfg.DBDir),
		slog.Uint64("coin_type", uint64(coinType)))

	return &Wallet{
		Wallet: walletcore.Wallet{
			Signer:      btcw,
			BtcWallet:   btcw,
			KeyRing:     keyRing,
			ChainParams: cfg.ChainParams,
			WalletLog:   cfg.Log,
		},
		chainSvc:        chainSvc,
		esplora:         esplora,
		chainBackend:    chainBackend,
		boardingBackend: boardingBackend,
	}, nil
}

// Start initializes the wallet by starting btcwallet (which
// internally starts the EsploraChainService and syncs the wallet)
// and the chainsource ChainBackend.
func (w *Wallet) Start() error {
	ctx := context.Background()

	// btcWallet.Start() unlocks the wallet, creates key scopes,
	// starts the chain service, and begins wallet synchronization.
	if err := w.BtcWallet.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}

	// Start the chainsource ChainBackend used by the actor system.
	// This is separate from the chain service used by btcwallet.
	if err := w.chainBackend.Start(); err != nil {
		return fmt.Errorf("start chain backend: %w", err)
	}

	w.Logger(ctx).InfoS(ctx, "Lightweight wallet started")

	return nil
}

// Stop shuts down the wallet, chain service, and chain backend. We
// wait for the chain service goroutine to fully exit before
// returning to avoid racing with btcwallet's internal writes.
func (w *Wallet) Stop() {
	ctx := context.Background()

	w.Logger(ctx).InfoS(ctx, "Stopping lightweight wallet")

	_ = w.BtcWallet.Stop()
	w.chainSvc.WaitForShutdown()
	_ = w.chainBackend.Stop()

	w.Logger(ctx).InfoS(ctx, "Lightweight wallet stopped")
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

// KeyRing returns the wallet's secret key ring for key derivation and
// message signing operations that need direct access to wallet-owned
// keys.
func (w *Wallet) KeyRing() keychain.SecretKeyRing {
	return w.Wallet.KeyRing
}
