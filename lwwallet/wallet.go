package lwwallet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightningnetwork/lnd/blockcache"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// defaultBlockCacheSize is the number of blocks to cache in memory.
// This prevents redundant block fetches during wallet sync.
const defaultBlockCacheSize uint64 = 20

// walletPassphrase is the default passphrase for the wallet DB.
var walletPassphrase = []byte("lwwallet")

// Wallet is a lightweight in-process Bitcoin wallet backed by LND's
// btcwallet implementation and an Esplora chain backend. It provides
// full on-chain wallet capabilities (receive, balance, key derivation,
// signing, MuSig2) plus Ark protocol participation (boarding address
// management, chain monitoring).
//
// The wallet wraps a btcwallet.BtcWallet instance which handles key
// management via waddrmgr, UTXO tracking, and transaction signing.
// Chain data is fetched from Esplora via the EsploraChainService
// (implementing btcwallet's chain.Interface) and the existing
// ChainBackend (implementing chainsource.ChainBackend for actors).
//
// The wallet exposes sub-interfaces via accessor methods:
//   - BoardingBackend() returns the wallet.BoardingBackend adapter
//   - Signer() returns the input.Signer implementation
//   - ChainBackend() returns the chainsource.ChainBackend for actors
//   - KeyRing() returns the keychain.SecretKeyRing for key operations
type Wallet struct {
	// Signer is the input.Signer implementation backed by
	// btcwallet. This supports Schnorr signing, taproot
	// key/script-path signing, and MuSig2 sessions. Embedding
	// this (together with DeriveNextKey) means Wallet directly
	// satisfies round.ClientWallet.
	input.Signer

	// btcWallet is the LND btcwallet instance that provides key
	// management, signing, UTXO tracking, and address generation.
	btcWallet *btcwallet.BtcWallet

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

	// keyRing provides keychain.SecretKeyRing backed by btcwallet's
	// waddrmgr for HD key derivation.
	keyRing keychain.SecretKeyRing

	// chainParams identifies the Bitcoin network.
	chainParams *chaincfg.Params

	// walletLog is an optional logger for this wallet instance. When set,
	// it takes precedence over the context-based logger from
	// build.LoggerFromContext. When None, the wallet falls back to the
	// context logger (or btclog.Disabled if none is found).
	walletLog fn.Option[btclog.Logger]
}

// New creates a new lightweight wallet from the given configuration.
// The caller must provide a DBDir for btcwallet's bbolt database and
// is responsible for managing the directory's lifecycle (creation
// before calling New, cleanup after Stop if desired).
func New(cfg Config) (*Wallet, error) {
	// Unwrap the optional logger, falling back to the package-level
	// logger which is set via the central logging registry.
	walletLog := cfg.Log.UnwrapOr(log)

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

	coinType := coinTypeForNet(cfg.ChainParams)
	blockCache := blockcache.NewBlockCache(defaultBlockCacheSize)

	btcw, err := btcwallet.New(btcwallet.Config{
		PrivatePass:    walletPassphrase,
		PublicPass:     walletPassphrase,
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
		Signer:          btcw,
		btcWallet:       btcw,
		chainSvc:        chainSvc,
		esplora:         esplora,
		chainBackend:    chainBackend,
		boardingBackend: boardingBackend,
		keyRing:         keyRing,
		chainParams:     cfg.ChainParams,
		walletLog:       cfg.Log,
	}, nil
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (w *Wallet) logger(ctx context.Context) btclog.Logger {
	return w.walletLog.UnwrapOr(build.LoggerFromContext(ctx))
}

// Start initializes the wallet by starting btcwallet (which
// internally starts the EsploraChainService and syncs the wallet)
// and the chainsource ChainBackend.
func (w *Wallet) Start() error {
	ctx := context.Background()

	// btcWallet.Start() unlocks the wallet, creates key scopes,
	// starts the chain service, and begins wallet synchronization.
	if err := w.btcWallet.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}

	// Start the chainsource ChainBackend used by the actor system.
	// This is separate from the chain service used by btcwallet.
	if err := w.chainBackend.Start(); err != nil {
		return fmt.Errorf("start chain backend: %w", err)
	}

	w.logger(ctx).InfoS(ctx, "Lightweight wallet started")

	return nil
}

// Stop shuts down the wallet, chain service, and chain backend. We
// wait for the chain service goroutine to fully exit before
// returning to avoid racing with btcwallet's internal writes.
func (w *Wallet) Stop() {
	ctx := context.Background()

	w.logger(ctx).InfoS(ctx, "Stopping lightweight wallet")

	_ = w.btcWallet.Stop()
	w.chainSvc.WaitForShutdown()
	_ = w.chainBackend.Stop()

	w.logger(ctx).InfoS(ctx, "Lightweight wallet stopped")
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

// DeriveNextKey derives the next key in the specified key family.
// This delegates to the btcwallet-backed keyring.
func (w *Wallet) DeriveNextKey(_ context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	desc, err := w.keyRing.DeriveNextKey(family)
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	return &desc, nil
}

// DeriveKey derives a specific key identified by the given KeyLocator.
// Unlike DeriveNextKey, this always returns the same key for the same
// locator, making it suitable for stable identity keys.
func (w *Wallet) DeriveKey(_ context.Context,
	loc keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	desc, err := w.keyRing.DeriveKey(loc)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}

	return &desc, nil
}

// NewAddress generates a new BIP86 taproot receiving address (P2TR
// key-path only) via btcwallet.
func (w *Wallet) NewAddress(
	ctx context.Context) (btcutil.Address, error) {

	addr, err := w.btcWallet.NewAddress(
		lnwallet.TaprootPubkey, false,
		lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	w.logger(ctx).DebugS(ctx, "Generated new P2TR address",
		slog.String("address", addr.String()))

	return addr, nil
}

// Balance returns the confirmed and unconfirmed balance across all
// wallet-managed addresses. Confirmed balance requires at least 1
// confirmation.
func (w *Wallet) Balance(
	ctx context.Context) (btcutil.Amount, btcutil.Amount, error) {

	// Log sync state for debugging. The sync height determines
	// whether confirmed transactions are counted.
	syncedTo := w.btcWallet.InternalWallet().SyncedTo()
	chainSynced := w.btcWallet.InternalWallet().ChainSynced()
	w.logger(ctx).DebugS(ctx, "Checking wallet balance",
		slog.Int("sync_height", int(syncedTo.Height)),
		slog.String("sync_hash", syncedTo.Hash.String()),
		slog.Bool("chain_synced", chainSynced))

	confirmed, err := w.btcWallet.ConfirmedBalance(1, "")
	if err != nil {
		return 0, 0, fmt.Errorf("get confirmed balance: %w", err)
	}

	// Total includes unconfirmed (0-conf) outputs.
	total, err := w.btcWallet.ConfirmedBalance(
		0, "",
	)
	if err != nil {
		return 0, 0, fmt.Errorf("get total balance: %w", err)
	}

	unconfirmed := total - confirmed

	w.logger(ctx).DebugS(ctx, "Wallet balance result",
		slog.Int64("confirmed_sats", int64(confirmed)),
		slog.Int64("unconfirmed_sats", int64(unconfirmed)),
		slog.Int64("total_sats", int64(total)))

	return confirmed, unconfirmed, nil
}

// InternalWallet returns the underlying btcwallet instance for
// advanced operations not exposed through the Wallet API.
func (w *Wallet) InternalWallet() *btcwallet.BtcWallet {
	return w.btcWallet
}

// ConfirmedBalance returns the confirmed balance with the specified
// minimum confirmations. This is a convenience wrapper around
// btcwallet's ConfirmedBalance.
func (w *Wallet) ConfirmedBalance(
	minConfs int32) (btcutil.Amount, error) {

	return w.btcWallet.ConfirmedBalance(minConfs, "")
}

// ListUnspentWitness returns all unspent witness outputs with
// confirmations in the given range. This delegates to btcwallet's
// ListUnspentWitness which returns P2WKH, P2TR, and nested P2SH
// outputs.
func (w *Wallet) ListUnspentWitness(minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	return w.btcWallet.ListUnspentWitness(
		minConfs, maxConfs, "",
	)
}
