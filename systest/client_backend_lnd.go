//go:build systest

package systest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	clientlnd "github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// lndBackend implements ClientBackend using an LND node for all wallet
// operations. This wraps the existing LND-based wiring that was
// previously hardcoded in newTestClientInternal.
type lndBackend struct {
	// harness is a reference to the parent E2E harness for creating
	// LND instances and accessing loggers.
	harness *E2EHarness

	// lndInstance is this backend's dedicated LND instance.
	lndInstance *clientharness.LndInstance

	// lndServices provides access to the LND RPC services.
	lndServices *lndclient.LndServices

	// chain is the LND-backed chain monitoring backend.
	chain *chainbackends.LNDBackend

	// boarding is the LND-backed boarding address manager.
	boarding wallet.BoardingBackend

	// clientWallet is the LND-backed signing wallet.
	clientWallet round.ClientWallet
}

// NewLNDBackend creates a new LND-backed client backend. This starts a
// dedicated LND instance for wallet isolation and initializes all
// backend components.
func NewLNDBackend(h *E2EHarness) ClientBackend {
	// Start a dedicated LND instance for this client.
	name := fmt.Sprintf("client-lnd-%d", nextBackendID())
	lndInstance := h.StartClientLND(name)

	return newLNDBackendFromInstance(h, lndInstance)
}

// newLNDBackendFromInstance creates an LND backend from an existing LND
// instance. This is used for restart testing where the same LND node
// must be reused.
func newLNDBackendFromInstance(h *E2EHarness,
	lndInstance *clientharness.LndInstance) *lndBackend {

	lndServices := lndInstance.Client

	// Create ChainBackend using client's LND.
	chain := chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: lndServices,
		}.WithLogger(
			h.SubLogger(chainbackends.LndClientSubsystem),
		),
	)

	// Create BoardingBackend using client's LND.
	boarding := clientlnd.NewBoardingBackend(lndServices.WalletKit)

	// Create ClientWallet (LndWalletController implements
	// input.Signer + DeriveNextKey).
	clientWallet := lndbackend.NewLndWalletController(
		lndServices.WalletKit, lndServices.Signer,
	)

	return &lndBackend{
		harness:      h,
		lndInstance:  lndInstance,
		lndServices:  lndServices,
		chain:        chain,
		boarding:     boarding,
		clientWallet: clientWallet,
	}
}

// ChainBackend returns the LND-backed chain monitoring backend.
func (b *lndBackend) ChainBackend() chainsource.ChainBackend {
	return b.chain
}

// BoardingBackend returns the LND-backed boarding address manager.
func (b *lndBackend) BoardingBackend() wallet.BoardingBackend {
	return b.boarding
}

// ClientWallet returns the LND-backed signing wallet.
func (b *lndBackend) ClientWallet() round.ClientWallet {
	return b.clientWallet
}

// DeriveClientKey derives a stable client identity key from LND.
// It uses a fixed key locator so the identity key is deterministic
// across restarts and clones of the same LND backend.
func (b *lndBackend) DeriveClientKey(ctx context.Context) (
	*keychain.KeyDescriptor, error) {

	return b.lndServices.WalletKit.DeriveKey(
		ctx, &keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	)
}

// GetOnChainBalance returns the confirmed balance from the LND wallet.
func (b *lndBackend) GetOnChainBalance(ctx context.Context) (
	btcutil.Amount, error) {

	balance, err := b.lndServices.Client.WalletBalance(ctx)
	if err != nil {
		return 0, fmt.Errorf("get wallet balance: %w", err)
	}

	return balance.Confirmed, nil
}

// GetNewAddress returns a new taproot address from the LND wallet.
func (b *lndBackend) GetNewAddress(ctx context.Context) (
	btcutil.Address, error) {

	addr, err := b.lndServices.WalletKit.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, fmt.Errorf("get new address: %w", err)
	}

	return addr, nil
}

// Clone creates a new LND backend reusing the same LND instance. This
// is used for restart testing where the same wallet and keys must be
// available.
func (b *lndBackend) Clone() ClientBackend {
	return newLNDBackendFromInstance(b.harness, b.lndInstance)
}

// Stop is a no-op for the LND backend since the LND lifecycle is
// managed by the test harness.
func (b *lndBackend) Stop() {}

// Compile-time check that lndBackend implements ClientBackend.
var _ ClientBackend = (*lndBackend)(nil)
