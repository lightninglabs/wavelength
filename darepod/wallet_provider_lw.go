package darepod

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/keychain"
)

// lwWalletProvider implements WalletProvider using an in-process
// lightweight wallet. The lwwallet package provides all necessary
// sub-interfaces: ChainBackend (Esplora-based), BoardingBackend,
// Signer (embedded), and DeriveNextKey. This makes the daemon fully
// self-contained without requiring a separate LND process.
type lwWalletProvider struct {
	wallet *lwwallet.Wallet

	chainParams *chaincfg.Params

	// nodePubkey is derived from the wallet seed at startup and
	// cached for repeated access.
	nodePubkey string
}

// newLWWalletProvider creates and starts an lwwallet-backed wallet
// provider. The wallet is fully initialized and synced before
// returning.
func newLWWalletProvider(ctx context.Context,
	cfg *LwWalletConfig,
	chainParams *chaincfg.Params) (*lwWalletProvider, error) {

	w, err := lwwallet.New(lwwallet.Config{
		Seed:         cfg.Seed,
		EsploraURL:   cfg.EsploraURL,
		ChainParams:  chainParams,
		PollInterval: cfg.PollInterval,
		DBDir:        cfg.DBDir,
		Logger:       log,
	})
	if err != nil {
		return nil, fmt.Errorf("create lwwallet: %w", err)
	}

	if err := w.Start(); err != nil {
		return nil, fmt.Errorf("start lwwallet: %w", err)
	}

	// Derive the identity key from the wallet seed. We use the
	// node identity key family (6) index 0 as the stable client
	// identity, matching LND's identity key derivation path.
	keyDesc, err := w.DeriveNextKey(
		ctx, keychain.KeyFamilyNodeKey,
	)
	if err != nil {
		w.Stop()

		return nil, fmt.Errorf(
			"derive identity key: %w", err,
		)
	}

	pubkeyHex := hex.EncodeToString(
		keyDesc.PubKey.SerializeCompressed(),
	)

	return &lwWalletProvider{
		wallet:      w,
		chainParams: chainParams,
		nodePubkey:  pubkeyHex,
	}, nil
}

// ChainBackend returns the Esplora-based chain backend for actor
// system registrations.
func (p *lwWalletProvider) ChainBackend() chainsource.ChainBackend {
	return p.wallet.ChainBackend()
}

// BoardingBackend returns the lwwallet boarding backend for taproot
// script import and UTXO listing.
func (p *lwWalletProvider) BoardingBackend() wallet.BoardingBackend {
	return p.wallet.BoardingBackend()
}

// ClientWallet returns the lwwallet as a round.ClientWallet. The
// lwwallet embeds input.Signer and provides DeriveNextKey, directly
// satisfying the ClientWallet interface.
func (p *lwWalletProvider) ClientWallet() round.ClientWallet {
	return p.wallet
}

// ChainParams returns the Bitcoin network parameters configured for
// this wallet.
func (p *lwWalletProvider) ChainParams() *chaincfg.Params {
	return p.chainParams
}

// NodePubkey returns the hex-encoded identity public key derived
// from the wallet seed.
func (p *lwWalletProvider) NodePubkey() string {
	return p.nodePubkey
}

// OnChainBalance returns the confirmed and unconfirmed on-chain
// balance from the lightweight wallet.
func (p *lwWalletProvider) OnChainBalance(
	ctx context.Context) (btcutil.Amount, btcutil.Amount, error) {

	return p.wallet.Balance(ctx)
}

// NewAddress generates a new P2TR on-chain receiving address via
// the lightweight wallet.
func (p *lwWalletProvider) NewAddress(
	ctx context.Context) (btcutil.Address, error) {

	return p.wallet.NewAddress(ctx)
}

// Close stops the lightweight wallet and releases all resources.
func (p *lwWalletProvider) Close() error {
	p.wallet.Stop()

	return nil
}

// Compile-time check that lwWalletProvider satisfies WalletProvider.
var _ WalletProvider = (*lwWalletProvider)(nil)
