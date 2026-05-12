//go:build systest

package systest

import (
	"context"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/chainsource"
	clientindexer "github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/keychain"
)

// backendCounter generates unique IDs for backend instances to ensure
// unique LND container names and other resources.
var backendCounter atomic.Int64

// nextBackendID returns the next unique backend ID.
func nextBackendID() int64 {
	return backendCounter.Add(1)
}

// ClientBackend abstracts the wallet backend used by a test client.
// Two implementations exist: LND-backed (existing behavior) and
// lwwallet-backed (lightweight in-process wallet). This allows the same
// E2E test suite to run against both backends to verify parity.
type ClientBackend interface {
	// ChainBackend returns the chain monitoring backend used for
	// block, confirmation, and spend notifications.
	ChainBackend() chainsource.ChainBackend

	// BoardingBackend returns the boarding address manager for
	// importing taproot scripts and listing UTXOs.
	BoardingBackend() wallet.BoardingBackend

	// ClientWallet returns the round signing wallet that implements
	// input.Signer and DeriveNextKey for VTXO signing.
	ClientWallet() round.ClientWallet

	// DeriveClientKey derives the client identity key used to
	// identify this client in the Ark protocol. The returned
	// key must be stable across calls and across Clone()
	// instances of the same backend.
	DeriveClientKey(ctx context.Context) (*keychain.KeyDescriptor, error)

	// IndexerSigner returns a Schnorr signer bound to the provided wallet
	// key. Receive-script registration and proof-gated indexer queries must
	// use the same key that controls the target receive script.
	IndexerSigner(
		keyDesc keychain.KeyDescriptor) clientindexer.SchnorrSigner

	// GetOnChainBalance returns the confirmed on-chain balance
	// across all wallet-owned addresses.
	GetOnChainBalance(ctx context.Context) (btcutil.Amount, error)

	// GetNewAddress returns a new on-chain receiving address for
	// receiving funds (e.g., leave outputs).
	GetNewAddress(ctx context.Context) (btcutil.Address, error)

	// Clone creates a new backend instance that reuses the same
	// identity (same seed or same LND instance). This is used
	// for restart testing where the wallet state must be
	// preserved. Clone is side-effect free; the caller must
	// call Stop() on the old backend separately.
	Clone() ClientBackend

	// Stop performs any cleanup needed by the backend (e.g.,
	// stopping the chain polling loop).
	Stop()
}
