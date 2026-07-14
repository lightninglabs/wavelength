package lwwallet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
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

	// tipPoller is the single Esplora tip-detection goroutine
	// shared by chainSvc (btcwallet) and chainBackend (actors).
	// Centralizing the poll cadence avoids the prior arrangement
	// in which each consumer ran an independent ticker against
	// the same Esplora endpoint.
	tipPoller *TipPoller

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

// ErrWalletNotFound is returned when opening an existing wallet was
// requested (nil Config.Seed) but no wallet database exists yet.
var ErrWalletNotFound = errors.New("no wallet database found")

// ErrWalletExists is returned when creating a new wallet was requested
// (non-nil Config.Seed) but a wallet database already exists.
var ErrWalletExists = errors.New("wallet database already exists")

// WalletExists reports whether a wallet database has already been
// created for the given configuration. Only ChainParams, RecoveryWindow
// and DBDir are consulted. Callers use this to decide between the
// create (seed) and open (password-only) paths before constructing the
// wallet.
func WalletExists(cfg Config) (bool, error) {
	return walletExists(cfg)
}

// checkWalletInvariants validates the seed, password, and database
// state agree on whether the wallet is being created or opened.
func checkWalletInvariants(cfg Config) error {
	if len(cfg.WalletPassword) == 0 {
		return fmt.Errorf("wallet password is required")
	}

	if cfg.Seed != nil && len(cfg.Seed) != walletcore.SeedLen {
		return fmt.Errorf("seed must be %d bytes, got %d",
			walletcore.SeedLen, len(cfg.Seed))
	}

	exists, err := walletExists(cfg)
	if err != nil {
		return fmt.Errorf("probe wallet database: %w", err)
	}

	switch {
	case cfg.Seed == nil && !exists:
		return fmt.Errorf("%w in %q: create the wallet with a "+
			"seed first", ErrWalletNotFound, cfg.DBDir)

	case cfg.Seed != nil && exists:
		return fmt.Errorf("%w in %q: open it without a seed instead",
			ErrWalletExists, cfg.DBDir)
	}

	return nil
}

// New creates a new lightweight wallet from the given configuration.
// The caller must provide a DBDir for btcwallet's wallet database. Native
// builds use that path for btcwallet's bbolt database, while browser builds
// derive a stable OPFS SQLite database name from it.
func New(cfg Config) (*Wallet, error) {
	// Constructors run before a contextual logger is guaranteed,
	// so default to a disabled logger when one was not explicitly
	// provided.
	walletLog := cfg.Log.UnwrapOr(btclog.Disabled)

	// Enforce the seed/existing-database invariant before any
	// subsystem is constructed. btcwallet silently generates a
	// random seed when asked to create a wallet without one, and
	// silently ignores a supplied seed when a wallet database
	// already exists. Both failure modes are unacceptable for a
	// funds-bearing wallet, so fail loudly instead.
	if err := checkWalletInvariants(cfg); err != nil {
		return nil, err
	}

	esplora := NewEsploraClient(cfg.EsploraURL, walletLog)

	// A single TipPoller owns Esplora tip detection for the whole
	// wallet. Both chainSvc and chainBackend subscribe to its
	// event stream, so each new block yields exactly one
	// GetTipHeight + GetBlockHashByHeight + GetBlockHeader round
	// trip rather than two parallel sets.
	tipPoller := NewTipPoller(esplora, cfg.PollInterval, walletLog)

	// The EsploraChainService implements btcwallet's chain.Interface
	// and feeds block notifications to btcwallet for wallet sync.
	chainSvc := NewEsploraChainService(esplora, tipPoller, walletLog)

	// The ChainBackend implements chainsource.ChainBackend for the
	// actor system (confirmation/spend/block registrations). It
	// shares the wallet's TipPoller, so its Start does not spin up
	// a second poll goroutine.
	chainBackend, err := NewChainBackendWithPoller(
		esplora, tipPoller, walletLog,
	)
	if err != nil {
		return nil, fmt.Errorf("create chain backend: %w", err)
	}

	coinType := walletcore.CoinTypeForNet(cfg.ChainParams)
	blockCache := blockcache.NewBlockCache(
		walletcore.DefaultBlockCacheSize,
	)

	loaderOptions, loaderCleanup, err := newWalletLoaderOptions(cfg)
	if err != nil {
		return nil, fmt.Errorf("create wallet loader options: %w", err)
	}

	btcw, err := btcwallet.New(btcwallet.Config{
		PrivatePass:    cfg.WalletPassword,
		PublicPass:     walletcore.PublicWalletPassphrase,
		HdSeed:         cfg.Seed,
		Birthday:       cfg.Birthday,
		ChainSource:    chainSvc,
		NetParams:      cfg.ChainParams,
		CoinType:       coinType,
		RecoveryWindow: cfg.RecoveryWindow,
		LoaderOptions:  loaderOptions,
	}, blockCache)
	if err != nil {
		// On failure the wallet never adopted the loader's
		// database handle, so release it here (a no-op natively,
		// an OPFS handle close in browser builds).
		loaderCleanup()

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
		tipPoller:       tipPoller,
		chainSvc:        chainSvc,
		esplora:         esplora,
		chainBackend:    chainBackend,
		boardingBackend: boardingBackend,
	}, nil
}

// Start initializes the wallet. The startup order is load-bearing:
// the TipPoller must be running before either chainSvc or
// chainBackend subscribes, since both rely on TipPoller.BestBlock()
// to seed their initial tip without issuing fresh Esplora calls.
// btcwallet's Start in turn drives chainSvc.Start through its
// chain.Interface contract, so chainSvc inherits the live tip the
// poller already established.
func (w *Wallet) Start() error {
	ctx := context.Background()

	// Each successful sub-system Start arms a rollback closure;
	// on the happy path we clear the slice at the end and the
	// deferred unwind is a no-op. On any error return below the
	// already-started subsystems are torn down in reverse order
	// so a bad passphrase / locked DB / unreachable Esplora does
	// not leak a polling goroutine for the lifetime of the
	// process.
	var rollback []func()
	defer func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}()

	// New opened the wallet database, so a failed start must close
	// it again or a retried unlock deadlocks on the database's
	// exclusive lock (bbolt flock natively, EXCLUSIVE OPFS locking
	// in browser builds). This matters in particular for a wrong
	// wallet passphrase, which surfaces from BtcWallet.Start below.
	// Appended first so the reverse-order unwind runs it last, after
	// the subsystems armed below have been rolled back.
	rollback = append(rollback, func() {
		err := w.BtcWallet.InternalWallet().Database().Close()
		if err != nil {
			w.Logger(ctx).WarnS(ctx, "Failed to close btcwallet DB",
				err,
			)
		}
	})

	// Spin up the shared tip poller before any consumer subscribes.
	if err := w.tipPoller.Start(); err != nil {
		return fmt.Errorf("start tip poller: %w", err)
	}
	rollback = append(rollback, w.tipPoller.Stop)

	// btcWallet.Start() unlocks the wallet, creates key scopes,
	// starts the chain service (which subscribes to the tip
	// poller), and begins wallet synchronization.
	if err := w.BtcWallet.Start(); err != nil {
		return fmt.Errorf("start btcwallet: %w", err)
	}
	rollback = append(rollback, func() { _ = w.BtcWallet.Stop() })

	// Start the chainsource ChainBackend used by the actor system.
	// It also subscribes to the wallet's TipPoller; it does not
	// own that poller, so calling Start here only spawns the
	// event-handler goroutine.
	if err := w.chainBackend.Start(); err != nil {
		return fmt.Errorf("start chain backend: %w", err)
	}

	w.Logger(ctx).InfoS(ctx, "Lightweight wallet started")

	// All subsystems started cleanly — clear the rollback slice
	// so the deferred unwind is a no-op.
	rollback = nil

	return nil
}

// Stop shuts down the wallet, chain service, chain backend, and
// shared tip poller. The teardown order mirrors Start in reverse:
// btcwallet first (so it stops draining notifications), then the
// chain backend (which unsubscribes from the poller), and finally
// the tip poller itself once nobody else can be observing it.
func (w *Wallet) Stop() {
	ctx := context.Background()

	w.Logger(ctx).InfoS(ctx, "Stopping lightweight wallet")

	_ = w.BtcWallet.Stop()
	if err := w.BtcWallet.InternalWallet().Database().Close(); err != nil {
		w.Logger(ctx).WarnS(ctx, "Failed to close btcwallet DB", err)
	}

	// Explicitly Stop the chain service before waiting on its
	// goroutine. btcwallet.Stop will transitively call
	// chainClient.Stop today, but relying on that is brittle —
	// any future fast-shutdown path that bypasses
	// btcwallet.Stop would leave handleTipEvents blocked on its
	// quit channel and deadlock WaitForShutdown. The Stop is
	// idempotent (sync.Once), so the duplicate-close path is
	// safe even if btcwallet's Stop already fired it.
	w.chainSvc.Stop()
	w.chainSvc.WaitForShutdown()
	_ = w.chainBackend.Stop()
	w.tipPoller.Stop()

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

// FinalizePsbtDirect signs and finalizes a PSBT using btcwallet.
// This is the lwwallet equivalent of LND's WalletKit.FinalizePsbt.
func (w *Wallet) FinalizePsbtDirect(packet *psbt.Packet) error {
	return w.BtcWallet.FinalizePsbt(
		packet, lnwallet.DefaultAccountName,
	)
}

// WaitForSync blocks until btcwallet's sync height reaches the
// current Esplora chain tip. This closes the race between the
// chain backend actor (which may notify about confirmations
// immediately) and btcwallet's asynchronous block processing
// pipeline fed by the EsploraChainService. Without this,
// ListUnspentWitness can return stale results when called right
// after a confirmation event because the two pipelines poll
// Esplora independently.
//
// The target tip is read from the shared TipPoller's cached
// snapshot rather than via a fresh GetTipHeight HTTP call: the
// poller is the only Esplora tip-detection authority in the wallet,
// and reading its cache lets WaitForSync run at the wallet's
// internal poll cadence rather than firing one extra HTTP request
// per call (which on a hot ListUnspentWitness path could compound
// against an already rate-limited Esplora endpoint).
func (w *Wallet) WaitForSync(ctx context.Context) error {
	tipHeight, _, _ := w.tipPoller.BestBlock()

	for {
		syncedTo := w.BtcWallet.InternalWallet().SyncedTo()
		if syncedTo.Height >= tipHeight {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-time.After(50 * time.Millisecond):
		}
	}
}
