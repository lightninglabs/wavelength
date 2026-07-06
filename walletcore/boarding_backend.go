package walletcore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

// BoardingBackendBase provides shared btcwallet-backed boarding
// functionality used by both lwwallet and btcwbackend boarding
// adapters. It handles key derivation, taproot script import, and
// imported address tracking. Chain-specific adapters embed this
// struct and implement ListUnspent, GetTransaction, and GetBlock
// using their respective chain data sources.
type BoardingBackendBase struct {
	// BtcWallet is the btcwallet instance for script imports.
	BtcWallet *btcwallet.BtcWallet

	// Log is the structured logger for this boarding backend.
	Log btclog.Logger

	// KeyRing is the cached key ring for HD key derivation.
	KeyRing keychain.KeyRing

	// ChainKeyScope is the key scope used for script imports.
	// This matches LND's m/1017'/coinType' derivation.
	ChainKeyScope waddrmgr.KeyScope

	// Mu protects ImportedAddrs.
	Mu sync.Mutex

	// ImportedAddrs tracks addresses imported via
	// ImportTaprootScript. Chain-specific ListUnspent
	// implementations use this to filter results to only boarding
	// UTXOs.
	//
	// This map is in-memory and is repopulated on daemon restart
	// by the wallet actor, which re-imports all persisted boarding
	// addresses from the database during startup (see
	// wallet.Ark.handleStartupRecovery).
	ImportedAddrs map[string]btcaddr.Address
}

// NewBoardingBackendBase creates a new base boarding backend wrapping
// the given btcwallet instance.
func NewBoardingBackendBase(btcw *btcwallet.BtcWallet, coinType uint32,
	logger btclog.Logger) BoardingBackendBase {

	keyRing := keychain.NewBtcWalletKeyRing(
		btcw.InternalWallet(), coinType,
	)

	return BoardingBackendBase{
		BtcWallet: btcw,
		Log:       logger,
		KeyRing:   keyRing,
		ChainKeyScope: waddrmgr.KeyScope{
			Purpose: keychain.BIP0043Purpose,
			Coin:    coinType,
		},
		ImportedAddrs: make(map[string]btcaddr.Address),
	}
}

// DeriveNextKey derives the next key in the specified key family.
// This delegates to btcwallet's keyring which uses the waddrmgr for
// HD key derivation following the m/1017'/coinType'/family'/0/index
// path.
func (b *BoardingBackendBase) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	desc, err := b.KeyRing.DeriveNextKey(family)
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	b.Log.DebugS(ctx, "Derived next key",
		slog.Int("family", int(family)),
		slog.Int("index", int(desc.Index)),
	)

	return &desc, nil
}

// ImportTaprootScript imports a taproot script into btcwallet and
// tracks the resulting address for UTXO filtering. After import,
// btcwallet will track UTXOs paying to this address via whatever
// chain source is configured (Esplora notifications or neutrino
// compact block filter matching).
//
// The script is imported under BIP86 (default taproot) scope rather
// than the custom chain key scope used for key derivation. This
// matches lnd's ImportTaprootScript RPC behavior and is required
// because btcwallet's block processing pipeline skips credit
// tracking for addresses in non-default scopes
// (chainntfns.go:IsDefaultScope check).
func (b *BoardingBackendBase) ImportTaprootScript(ctx context.Context,
	script *waddrmgr.Tapscript) (btcaddr.Address, error) {

	managedAddr, err := b.BtcWallet.ImportTaprootScript(
		waddrmgr.KeyScopeBIP0086, script,
	)
	if err != nil {
		if waddrmgr.IsError(err, waddrmgr.ErrDuplicateAddress) {
			addr, addrErr := b.addressForTaprootScript(script)
			if addrErr != nil {
				return nil, fmt.Errorf("derive duplicate "+
					"taproot script address: %w", addrErr)
			}

			numAddrs := b.trackImportedAddress(addr)
			b.Log.DebugS(ctx, "Tracked duplicate taproot script",
				slog.String("address", addr.String()),
				slog.Int("tracked_addrs", numAddrs),
			)

			return addr, nil
		}

		return nil, fmt.Errorf("import taproot script: %w", err)
	}

	addr := managedAddr.Address()

	numAddrs := b.trackImportedAddress(addr)

	b.Log.DebugS(ctx, "Imported taproot script via btcwallet",
		slog.String("address", addr.String()),
		slog.Int("tracked_addrs", numAddrs),
	)

	return addr, nil
}

// trackImportedAddress records an imported address so ListUnspent
// implementations can filter results to only return boarding UTXOs.
// Returns the number of currently tracked addresses after the insert,
// which lets callers log the count without a separate locked read or
// a full map copy via SnapshotAddrs.
func (b *BoardingBackendBase) trackImportedAddress(addr btcaddr.Address) int {
	b.Mu.Lock()
	b.ImportedAddrs[addr.String()] = addr
	numAddrs := len(b.ImportedAddrs)
	b.Mu.Unlock()

	return numAddrs
}

// addressForTaprootScript derives the P2TR address that btcwallet stores for
// a tapscript import. This lets restart recovery repopulate the in-memory
// address filter when btcwallet reports that a persisted import already exists.
func (b *BoardingBackendBase) addressForTaprootScript(
	script *waddrmgr.Tapscript) (btcaddr.Address, error) {

	taprootKey, err := script.TaprootKey()
	if err != nil {
		return nil, fmt.Errorf("calculate taproot key: %w", err)
	}

	return btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey),
		b.BtcWallet.InternalWallet().ChainParams(),
	)
}

// SnapshotAddrs returns a snapshot of the currently imported
// addresses under the lock. This is a convenience for ListUnspent
// implementations that need to iterate over addresses without
// holding the lock for the duration of chain queries.
func (b *BoardingBackendBase) SnapshotAddrs() map[string]btcaddr.Address {
	b.Mu.Lock()
	addrs := make(map[string]btcaddr.Address, len(b.ImportedAddrs))
	for k, v := range b.ImportedAddrs {
		addrs[k] = v
	}
	b.Mu.Unlock()

	return addrs
}
