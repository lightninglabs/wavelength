package darepod

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
)

// WalletProvider abstracts the wallet backend used by the daemon. Two
// implementations exist: LND-backed (default, production) and lwwallet-backed
// (lightweight in-process wallet for testing and SDK embedding). This allows
// the daemon's startup sequence to remain wallet-agnostic while still
// providing chain monitoring, boarding, signing, key derivation, and on-chain
// balance capabilities.
//
// The interface mirrors the coupling points between the daemon and LND:
//   - ChainBackend: block/confirmation/spend notifications
//   - BoardingBackend: taproot script import and UTXO listing
//   - ClientWallet: MuSig2 signing with key derivation
//   - ChainParams: network identification
//   - NodePubkey: client identity for indexer registration
//   - OnChainBalance: confirmed/unconfirmed wallet balance
//   - NewAddress: on-chain receiving address generation
type WalletProvider interface {
	// ChainBackend returns the chain monitoring backend used for
	// block, confirmation, and spend notifications by the actor
	// system.
	ChainBackend() chainsource.ChainBackend

	// BoardingBackend returns the boarding address manager for
	// importing taproot scripts and listing UTXOs.
	BoardingBackend() wallet.BoardingBackend

	// ClientWallet returns the round signing wallet that
	// implements input.Signer and DeriveNextKey for VTXO
	// signing during round participation.
	ClientWallet() round.ClientWallet

	// ChainParams returns the Bitcoin network parameters for
	// address encoding and script construction.
	ChainParams() *chaincfg.Params

	// NodePubkey returns the hex-encoded public key identifying
	// this client. For LND, this is the node's identity key; for
	// lwwallet, it is derived from the seed.
	NodePubkey() string

	// OnChainBalance returns the confirmed and unconfirmed
	// on-chain balance across all wallet-managed addresses.
	OnChainBalance(ctx context.Context) (
		confirmed, unconfirmed btcutil.Amount, err error,
	)

	// NewAddress generates a new on-chain receiving address for
	// funding the wallet (e.g., boarding deposits).
	NewAddress(ctx context.Context) (btcutil.Address, error)

	// Close releases all resources held by the wallet provider.
	Close() error
}
