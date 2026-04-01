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
	"github.com/lightninglabs/darepo-client/btcwbackend"
	"github.com/lightninglabs/darepo-client/chainsource"
	clientindexer "github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo-client/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// btcwBackend implements ClientBackend using the neutrino-backed
// btcwallet. It connects directly to the harness's bitcoind P2P
// port for header and compact block filter sync, providing a fully
// self-contained SPV wallet without requiring Esplora or LND.
type btcwBackend struct {
	// harness is a reference to the parent E2E harness.
	harness *E2EHarness

	// seed is the wallet seed, stored for Clone() to support
	// restart testing.
	seed [32]byte

	// wallet is the neutrino-backed btcwallet. It embeds
	// walletcore.Wallet which satisfies round.ClientWallet
	// (input.Signer + DeriveNextKey).
	wallet *btcwbackend.Wallet

	// identityKey is the cached client identity key derived once
	// at backend creation. This ensures the key is stable across
	// calls and across Clone() instances (same seed produces the
	// same first key in KeyFamilyNodeKey).
	identityKey *keychain.KeyDescriptor

	// stopOnce ensures Stop() is safe to call multiple times.
	stopOnce sync.Once
}

// NewBtcwBackend creates a new neutrino-backed btcwallet backend.
// It generates a random seed, creates the wallet connected to the
// harness's bitcoind P2P port, and starts the wallet.
func NewBtcwBackend(h *E2EHarness) ClientBackend {
	var seed [32]byte
	_, err := rand.Read(seed[:])
	require.NoError(h.t, err, "failed to generate wallet seed")

	return newBtcwBackendFromSeed(h, seed, 0)
}

// newBtcwBackendFromSeed creates a neutrino-backed btcwallet
// backend from an existing seed. The recoveryWindow parameter
// controls how many addresses btcwallet scans ahead to discover
// previously used keys. Use 0 for fresh wallets and a positive
// value for restart/clone scenarios.
func newBtcwBackendFromSeed(h *E2EHarness,
	seed [32]byte, recoveryWindow uint32) *btcwBackend {

	p2pAddr := h.Harness.BitcoindP2P

	// Create the wallet DB directory inside the test artifacts
	// dir so it is preserved alongside other test outputs for
	// debugging.
	dbDir := filepath.Join(
		h.Harness.BaseDir(),
		fmt.Sprintf("btcwallet-%d", nextBackendID()),
	)
	require.NoError(
		h.t, os.MkdirAll(dbDir, 0o755),
		"failed to create wallet db dir",
	)

	w, err := btcwbackend.New(btcwbackend.Config{
		Config: walletcore.Config{
			Seed:           seed,
			ChainParams:    &chaincfg.RegressionNetParams,
			RecoveryWindow: recoveryWindow,
			DBDir:          dbDir,
			Log: fn.Some(
				h.SubLogger(btcwbackend.Subsystem),
			),
		},
		ConnectPeers: []string{p2pAddr},
		FeeURL:       h.Harness.EsploraURL + "/api/v1/fees/recommended",
	})
	require.NoError(h.t, err, "failed to create btcwallet")

	// Start the wallet: this initializes neutrino (syncs headers
	// and compact block filters), starts btcwallet, and starts
	// the chainsource ChainBackend.
	err = w.Start()
	require.NoError(h.t, err, "failed to start btcwallet")

	// Wait for neutrino to sync headers and compact block filters
	// with the regtest bitcoind. Without this, the chain backend
	// reports height 0 and confirmation notifications never fire.
	require.Eventually(h.t, func() bool {
		height, _, bestErr := w.ChainBackend().BestBlock(
			h.t.Context(),
		)

		return bestErr == nil && height > 0
	}, 2*time.Minute, 500*time.Millisecond,
		"neutrino did not sync with bitcoind",
	)

	// Derive and cache the identity key once. DeriveNextKey is
	// stateful (increments index), so we call it exactly once
	// at creation and return the cached value from
	// DeriveClientKey. The same seed always produces the same
	// first key for KeyFamilyNodeKey.
	identityKey, err := w.DeriveNextKey(
		h.t.Context(), keychain.KeyFamilyNodeKey,
	)
	require.NoError(h.t, err, "failed to derive identity key")

	return &btcwBackend{
		harness:     h,
		seed:        seed,
		wallet:      w,
		identityKey: identityKey,
	}
}

// ChainBackend returns the neutrino-backed chain monitoring
// backend.
func (b *btcwBackend) ChainBackend() chainsource.ChainBackend {
	return b.wallet.ChainBackend()
}

// BoardingBackend returns the neutrino-backed boarding address
// manager.
func (b *btcwBackend) BoardingBackend() wallet.BoardingBackend {
	return b.wallet.BoardingBackend()
}

// ClientWallet returns the btcwallet-backed signing wallet that
// implements input.Signer and DeriveNextKey.
func (b *btcwBackend) ClientWallet() round.ClientWallet {
	return &b.wallet.Wallet
}

// DeriveClientKey returns the cached client identity key that was
// derived once at backend creation. The same seed always produces
// the same key, ensuring stability across calls and Clone()
// instances.
func (b *btcwBackend) DeriveClientKey(_ context.Context) (
	*keychain.KeyDescriptor, error) {

	return b.identityKey, nil
}

// IndexerSigner returns the signer that proves control over the
// provided receive key.
func (b *btcwBackend) IndexerSigner(
	keyDesc keychain.KeyDescriptor) clientindexer.SchnorrSigner {

	return clientindexer.NewKeyRingSchnorrSigner(
		b.wallet.KeyRing(), keyDesc,
	)
}

// GetOnChainBalance returns the confirmed on-chain balance across
// all wallet-owned addresses.
func (b *btcwBackend) GetOnChainBalance(ctx context.Context) (
	btcutil.Amount, error) {

	confirmed, _, err := b.wallet.Balance(ctx)
	if err != nil {
		return 0, fmt.Errorf("get wallet balance: %w", err)
	}

	return confirmed, nil
}

// GetNewAddress returns a new BIP86 taproot address from the
// btcwallet.
func (b *btcwBackend) GetNewAddress(ctx context.Context) (
	btcutil.Address, error) {

	return b.wallet.NewAddress(ctx)
}

// Clone creates a new btcwallet backend from the same seed.
// BtcWallet's RecoveryWindow discovers previously used keys during
// the initial sync, so no manual state restoration is needed. The
// boarding store re-imports addresses on wallet actor start via
// ImportTaprootScript.
//
// Clone is side-effect free: the caller must call Stop() on the old
// backend separately when it is no longer needed.
func (b *btcwBackend) Clone() ClientBackend {
	// Create a new wallet with recovery enabled so btcwallet
	// discovers previously derived keys during sync.
	return newBtcwBackendFromSeed(b.harness, b.seed, 200)
}

// Stop shuts down the wallet, neutrino service, and chain backend.
// The database directory lives in the test artifacts dir and is
// cleaned up by the harness. It is safe to call multiple times.
func (b *btcwBackend) Stop() {
	b.stopOnce.Do(func() {
		b.wallet.Stop()
	})
}

// Compile-time check that btcwBackend implements ClientBackend.
var _ ClientBackend = (*btcwBackend)(nil)
