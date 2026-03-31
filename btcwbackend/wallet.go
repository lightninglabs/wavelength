package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// walletPassphrase is the default passphrase for the wallet DB.
var walletPassphrase = []byte("btcwbackend")

// hintCacheDBName is the filename for the height hint cache database.
const hintCacheDBName = "heighthint.db"

// Wallet is a lightweight in-process Bitcoin wallet backed by LND's
// btcwallet implementation and a neutrino (BIP 157/158) chain
// backend. It provides full on-chain wallet capabilities (receive,
// balance, key derivation, signing, MuSig2) plus Ark protocol
// participation (boarding address management, chain monitoring).
//
// The wallet wraps a btcwallet.BtcWallet instance which handles key
// management via waddrmgr, UTXO tracking, and transaction signing.
// Chain data comes from neutrino's compact block filters via the P2P
// network.
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

	// neutrinoSvc manages the neutrino chain service lifecycle.
	neutrinoSvc *NeutrinoService

	// chainBackend implements chainsource.ChainBackend for the
	// actor system (confirmation/spend/block registrations).
	chainBackend *ChainBackend

	// boardingBackend wraps btcwallet to provide the
	// wallet.BoardingBackend interface for Ark boarding.
	boardingBackend *BoardingBackendAdapter

	// keyRing provides keychain.SecretKeyRing backed by
	// btcwallet's waddrmgr for HD key derivation.
	keyRing keychain.SecretKeyRing

	// chainParams identifies the Bitcoin network.
	chainParams *chaincfg.Params

	// walletLog is an optional logger for this wallet instance.
	walletLog fn.Option[btclog.Logger]
}

// New creates a new neutrino-backed wallet from the given
// configuration. The caller must provide a DBDir for btcwallet's
// bbolt database and is responsible for managing the directory's
// lifecycle.
func New(cfg Config) (*Wallet, error) {
	walletLog := cfg.Log.UnwrapOr(btclog.Disabled)

	neutrinoDataDir := cfg.neutrinoDataDir()

	// Create and start the neutrino chain service.
	neutrinoSvc, err := NewNeutrinoService(
		neutrinoDataDir, cfg.ChainParams,
		cfg.ConnectPeers, cfg.AddPeers,
		cfg.PersistFilters, walletLog,
	)
	if err != nil {
		return nil, fmt.Errorf("create neutrino service: %w", err)
	}

	if err := neutrinoSvc.Start(); err != nil {
		return nil, fmt.Errorf("start neutrino service: %w", err)
	}

	// Create the btcwallet chain client backed by neutrino.
	chainClient := neutrinoSvc.ChainClient()

	coinType := coinTypeForNet(cfg.ChainParams)
	blockCache := neutrinoSvc.BlockCache()

	btcw, err := btcwallet.New(btcwallet.Config{
		PrivatePass:    walletPassphrase,
		PublicPass:     walletPassphrase,
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
		_ = neutrinoSvc.Stop()

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
		neutrinoSvc, cfg.FeeURL,
		cfg.feeMinTimeout(), cfg.feeMaxTimeout(),
		hintDBPath, walletLog,
	)
	if err != nil {
		_ = btcw.Stop()
		_ = neutrinoSvc.Stop()

		return nil, fmt.Errorf("create chain backend: %w", err)
	}

	// Create the boarding backend adapter.
	boardingBackend := NewBoardingBackendAdapter(
		btcw, neutrinoSvc.ChainService(),
		blockCache, cfg.ChainParams, coinType, walletLog,
	)

	walletLog.InfoS(context.Background(),
		"Neutrino-backed wallet created",
		slog.String("db_dir", cfg.DBDir),
		slog.String("neutrino_dir", neutrinoDataDir),
		slog.Uint64("coin_type", uint64(coinType)))

	return &Wallet{
		Signer:          btcw,
		btcWallet:       btcw,
		neutrinoSvc:     neutrinoSvc,
		chainBackend:    chainBackend,
		boardingBackend: boardingBackend,
		keyRing:         keyRing,
		chainParams:     cfg.ChainParams,
		walletLog:       cfg.Log,
	}, nil
}

// logger returns the configured logger or falls back to extracting
// from context.
func (w *Wallet) logger(ctx context.Context) btclog.Logger {
	return w.walletLog.UnwrapOr(build.LoggerFromContext(ctx))
}

// Start initializes the wallet by starting btcwallet (which
// internally starts the chain client and syncs the wallet) and the
// chainsource ChainBackend.
func (w *Wallet) Start() error {
	ctx := context.Background()

	// btcWallet.Start() unlocks the wallet, creates key scopes,
	// starts the chain client, and begins wallet synchronization.
	if err := w.btcWallet.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}

	// Start the chain backend. This is idempotent (sync.Once) so it
	// is safe even if the daemon's startBtcwallet also calls Start().
	if err := w.chainBackend.Start(); err != nil {
		return fmt.Errorf("start chain backend: %w", err)
	}

	w.logger(ctx).InfoS(ctx, "Neutrino-backed wallet started")

	return nil
}

// Stop shuts down the wallet, neutrino service, and chain backend.
func (w *Wallet) Stop() {
	ctx := context.Background()

	w.logger(ctx).InfoS(ctx, "Stopping neutrino-backed wallet")

	// Note: chainBackend is NOT stopped here — the daemon's
	// server.go defer owns that lifecycle. The ChainBackend.Stop()
	// is idempotent (sync.Once) so it is safe even if called from
	// both Wallet and server.
	//
	// Stop order: btcwallet (depends on neutrino chain client) must
	// stop before neutrino service.
	_ = w.btcWallet.Stop()
	_ = w.neutrinoSvc.Stop()

	w.logger(ctx).InfoS(ctx, "Neutrino-backed wallet stopped")
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
	return w.keyRing
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

// DeriveKey derives a specific key identified by the given
// KeyLocator. Unlike DeriveNextKey, this always returns the same key
// for the same locator, making it suitable for stable identity keys.
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
// wallet-managed addresses.
func (w *Wallet) Balance(
	ctx context.Context) (btcutil.Amount, btcutil.Amount, error) {

	syncedTo := w.btcWallet.InternalWallet().SyncedTo()
	chainSynced := w.btcWallet.InternalWallet().ChainSynced()
	w.logger(ctx).DebugS(ctx, "Checking wallet balance",
		slog.Int("sync_height", int(syncedTo.Height)),
		slog.String("sync_hash", syncedTo.Hash.String()),
		slog.Bool("chain_synced", chainSynced))

	confirmed, err := w.btcWallet.ConfirmedBalance(1, "")
	if err != nil {
		return 0, 0, fmt.Errorf(
			"get confirmed balance: %w", err,
		)
	}

	total, err := w.btcWallet.ConfirmedBalance(0, "")
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
// minimum confirmations.
func (w *Wallet) ConfirmedBalance(
	minConfs int32) (btcutil.Amount, error) {

	return w.btcWallet.ConfirmedBalance(minConfs, "")
}

// ListUnspentWitness returns all unspent witness outputs with
// confirmations in the given range.
func (w *Wallet) ListUnspentWitness(minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	return w.btcWallet.ListUnspentWitness(
		minConfs, maxConfs, "",
	)
}
