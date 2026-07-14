package walletcore

import (
	"context"
	"fmt"
	"log/slog"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/proofkeys"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// Wallet provides shared btcwallet-backed functionality used by both
// lwwallet and btcwbackend. It wraps a btcwallet.BtcWallet instance
// and provides HD key management, signing (including MuSig2),
// address generation, and balance queries.
//
// Chain-specific wallet implementations embed this struct and add
// their own chain service, chain backend, and boarding backend.
// Embedding Wallet (together with DeriveNextKey) makes the outer
// type satisfy round.ClientWallet.
type Wallet struct {
	// Signer is the input.Signer implementation backed by
	// btcwallet. This supports Schnorr signing, taproot
	// key/script-path signing, and MuSig2 sessions.
	input.Signer

	// BtcWallet is the LND btcwallet instance that provides key
	// management, signing, UTXO tracking, and address generation.
	BtcWallet *btcwallet.BtcWallet

	// KeyRing provides keychain.SecretKeyRing backed by
	// btcwallet's waddrmgr for HD key derivation.
	KeyRing keychain.SecretKeyRing

	// ChainParams identifies the Bitcoin network.
	ChainParams *chaincfg.Params

	// WalletLog is an optional logger for this wallet instance.
	WalletLog fn.Option[btclog.Logger]
}

// Logger returns the configured logger or falls back to extracting
// from context. If no logger is found in either location, returns
// btclog.Disabled.
func (w *Wallet) Logger(ctx context.Context) btclog.Logger {
	return w.WalletLog.UnwrapOr(build.LoggerFromContext(ctx))
}

// DeriveNextKey derives the next key in the specified key family.
// This delegates to the btcwallet-backed keyring.
func (w *Wallet) DeriveNextKey(_ context.Context, family keychain.KeyFamily) (
	*keychain.KeyDescriptor, error) {

	desc, err := w.KeyRing.DeriveNextKey(family)
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	return &desc, nil
}

// DeriveKey derives a specific key identified by the given
// KeyLocator. Unlike DeriveNextKey, this always returns the same key
// for the same locator, making it suitable for stable identity keys.
func (w *Wallet) DeriveKey(_ context.Context, loc keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	desc, err := w.KeyRing.DeriveKey(loc)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}

	return &desc, nil
}

// ProofSigner returns an indexer proof signer bound to keyDesc using the
// wallet's local secret key ring.
func (w *Wallet) ProofSigner(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

	return indexer.NewKeyRingSchnorrSigner(w.KeyRing, keyDesc)
}

// NewAddress generates a new BIP86 taproot receiving address (P2TR
// key-path only) via btcwallet.
func (w *Wallet) NewAddress(ctx context.Context) (btcaddr.Address, error) {
	addr, err := w.BtcWallet.NewAddress(
		lnwallet.TaprootPubkey, false, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	w.Logger(ctx).DebugS(ctx, "Generated new P2TR address",
		slog.String("address", addr.String()),
	)

	return addr, nil
}

var _ proofkeys.Backend = (*Wallet)(nil)

// Balance returns the confirmed and unconfirmed balance across all
// wallet-managed addresses. Confirmed balance requires at least 1
// confirmation.
func (w *Wallet) Balance(ctx context.Context) (btcutil.Amount, btcutil.Amount,
	error) {

	syncedTo := w.BtcWallet.InternalWallet().SyncedTo()
	chainSynced := w.BtcWallet.InternalWallet().ChainSynced()
	w.Logger(ctx).DebugS(ctx, "Checking wallet balance",
		slog.Int("sync_height", int(syncedTo.Height)),
		slog.String("sync_hash", syncedTo.Hash.String()),
		slog.Bool("chain_synced", chainSynced),
	)

	confirmed, err := w.BtcWallet.ConfirmedBalance(1, "")
	if err != nil {
		return 0, 0, fmt.Errorf("get confirmed balance: %w", err)
	}

	// Total includes unconfirmed (0-conf) outputs.
	total, err := w.BtcWallet.ConfirmedBalance(0, "")
	if err != nil {
		return 0, 0, fmt.Errorf("get total balance: %w", err)
	}

	unconfirmed := total - confirmed

	w.Logger(ctx).DebugS(ctx, "Wallet balance result",
		slog.Int64("confirmed_sats", int64(confirmed)),
		slog.Int64("unconfirmed_sats", int64(unconfirmed)),
		slog.Int64("total_sats", int64(total)),
	)

	return confirmed, unconfirmed, nil
}

// InternalWallet returns the underlying btcwallet instance for
// advanced operations not exposed through the Wallet API.
func (w *Wallet) InternalWallet() *btcwallet.BtcWallet {
	return w.BtcWallet
}

// ConfirmedBalance returns the confirmed balance with the specified
// minimum confirmations. This is a convenience wrapper around
// btcwallet's ConfirmedBalance.
func (w *Wallet) ConfirmedBalance(minConfs int32) (btcutil.Amount, error) {
	return w.BtcWallet.ConfirmedBalance(minConfs, "")
}

// ListUnspentWitness returns all unspent witness outputs with
// confirmations in the given range. This delegates to btcwallet's
// ListUnspentWitness which returns P2WKH, P2TR, and nested P2SH
// outputs.
func (w *Wallet) ListUnspentWitness(minConfs, maxConfs int32) ([]*lnwallet.Utxo,
	error) {

	return w.BtcWallet.ListUnspentWitness(
		minConfs, maxConfs, "",
	)
}

// GetKeyRing returns the wallet's secret key ring for key derivation
// and message signing operations that need direct access to
// wallet-owned keys.
func (w *Wallet) GetKeyRing() keychain.SecretKeyRing {
	return w.KeyRing
}
