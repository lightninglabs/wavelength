//go:build systest

package systest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/chainsource"
	clientindexer "github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// lwBackend implements ClientBackend using the lightweight in-process
// wallet backed by btcwallet. It provides all wallet operations (key
// derivation, signing, chain monitoring, boarding) without requiring
// an external LND node.
type lwBackend struct {
	// harness is a reference to the parent E2E harness.
	harness *E2EHarness

	// seed is the wallet seed, stored for Clone() to support
	// restart testing.
	seed [32]byte

	// wallet is the btcwallet-backed lightweight wallet. It
	// directly satisfies round.ClientWallet (input.Signer +
	// DeriveNextKey) via its embedded signer and keyring.
	wallet *lwwallet.Wallet

	// identityKey is the cached client identity key derived once
	// at backend creation. This ensures the key is stable across
	// calls and across Clone() instances (same seed produces
	// the same first key in KeyFamilyNodeKey).
	identityKey *keychain.KeyDescriptor

	// stopOnce ensures Stop() is safe to call multiple times.
	stopOnce sync.Once
}

// NewLWBackend creates a new lightweight wallet backend. It generates a
// random seed, creates the wallet using the harness's Esplora URL, and
// starts the wallet (btcwallet + chain backends).
func NewLWBackend(h *E2EHarness) ClientBackend {
	var seed [32]byte
	_, err := rand.Read(seed[:])
	require.NoError(h.t, err, "failed to generate wallet seed")

	return newLWBackendFromSeed(h, seed, 0)
}

// newLWBackendFromSeed creates a lightweight wallet backend from an
// existing seed. The recoveryWindow parameter controls how many
// addresses btcwallet scans ahead to discover previously used keys.
// Use 0 for fresh wallets and a positive value for restart/clone
// scenarios.
func newLWBackendFromSeed(h *E2EHarness,
	seed [32]byte, recoveryWindow uint32) *lwBackend {

	esploraURL := h.Harness.EsploraURL

	// Create the wallet DB directory inside the test artifacts
	// dir so it is preserved alongside other test outputs for
	// debugging.
	dbDir := filepath.Join(
		h.Harness.BaseDir(),
		fmt.Sprintf("lwwallet-%d", nextBackendID()),
	)
	require.NoError(
		h.t, os.MkdirAll(dbDir, 0o755),
		"failed to create wallet db dir",
	)

	lw, err := lwwallet.New(lwwallet.Config{
		Seed:           seed,
		EsploraURL:     esploraURL,
		ChainParams:    &chaincfg.RegressionNetParams,
		PollInterval:   time.Second,
		RecoveryWindow: recoveryWindow,
		DBDir:          dbDir,
		Log:            fn.Some(h.SubLogger(lwwallet.Subsystem)),
	})
	require.NoError(h.t, err, "failed to create lightweight wallet")

	// Start the wallet: this initializes btcwallet (which starts
	// the Esplora chain service and begins wallet sync) and the
	// chainsource ChainBackend (used by actors).
	err = lw.Start()
	require.NoError(h.t, err, "failed to start lightweight wallet")

	// Derive and cache the identity key once. DeriveNextKey is
	// stateful (increments index), so we call it exactly once
	// at creation and return the cached value from
	// DeriveClientKey. The same seed always produces the same
	// first key for KeyFamilyNodeKey.
	identityKey, err := lw.DeriveNextKey(
		h.t.Context(), keychain.KeyFamilyNodeKey,
	)
	require.NoError(h.t, err, "failed to derive identity key")

	return &lwBackend{
		harness:     h,
		seed:        seed,
		wallet:      lw,
		identityKey: identityKey,
	}
}

// ChainBackend returns the Esplora-backed chain monitoring backend.
func (b *lwBackend) ChainBackend() chainsource.ChainBackend {
	return b.wallet.ChainBackend()
}

// BoardingBackend returns the btcwallet-backed boarding address
// manager.
func (b *lwBackend) BoardingBackend() wallet.BoardingBackend {
	return b.wallet.BoardingBackend()
}

// ClientWallet returns the btcwallet-backed signing wallet that
// implements input.Signer and DeriveNextKey.
func (b *lwBackend) ClientWallet() round.ClientWallet {
	return b.wallet
}

// DeriveClientKey returns the cached client identity key that was
// derived once at backend creation. The same seed always produces the
// same key, ensuring stability across calls and Clone() instances.
func (b *lwBackend) DeriveClientKey(_ context.Context) (
	*keychain.KeyDescriptor, error) {

	return b.identityKey, nil
}

// IndexerSigner returns the signer that proves control over the provided
// receive key.
func (b *lwBackend) IndexerSigner(
	keyDesc keychain.KeyDescriptor) clientindexer.SchnorrSigner {

	return clientindexer.NewKeyRingSchnorrSigner(
		b.wallet.KeyRing(), keyDesc,
	)
}

// GetOnChainBalance returns the confirmed on-chain balance across all
// wallet-owned addresses.
func (b *lwBackend) GetOnChainBalance(ctx context.Context) (
	btcutil.Amount, error) {

	confirmed, _, err := b.wallet.Balance(ctx)
	if err != nil {
		return 0, fmt.Errorf("get wallet balance: %w", err)
	}

	return confirmed, nil
}

// GetNewAddress returns a new BIP86 taproot address from the
// lightweight wallet.
func (b *lwBackend) GetNewAddress(ctx context.Context) (
	btcutil.Address, error) {

	return b.wallet.NewAddress(ctx)
}

// Clone creates a new lightweight wallet backend from the same seed.
// BtcWallet's RecoveryWindow discovers previously used keys during
// the initial sync, so no manual state restoration is needed. The
// boarding store re-imports addresses on wallet actor start via
// ImportTaprootScript.
//
// Clone is side-effect free: the caller must call Stop() on the old
// backend separately when it is no longer needed.
func (b *lwBackend) Clone() ClientBackend {
	// Create a new wallet with recovery enabled so btcwallet
	// discovers previously derived keys during sync.
	return newLWBackendFromSeed(b.harness, b.seed, 200)
}

// Stop shuts down the wallet, chain service, and chain backend. The
// database directory lives in the test artifacts dir and is cleaned
// up by the harness. It is safe to call multiple times.
func (b *lwBackend) Stop() {
	b.stopOnce.Do(func() {
		b.wallet.Stop()
	})
}

// Compile-time check that lwBackend implements ClientBackend.
var _ ClientBackend = (*lwBackend)(nil)
