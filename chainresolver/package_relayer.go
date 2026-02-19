package chainresolver

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/chain"
)

// BitcoindPackageRelayer implements PackageRelayer using a direct
// connection to bitcoind's submitpackage RPC. This requires a bitcoind
// backend with package relay support (Bitcoin Core 28+).
type BitcoindPackageRelayer struct {
	client *chain.BitcoindRPCClient
}

// NewBitcoindPackageRelayer creates a new BitcoindPackageRelayer
// wrapping the given bitcoind RPC client.
func NewBitcoindPackageRelayer(
	client *chain.BitcoindRPCClient) *BitcoindPackageRelayer {

	return &BitcoindPackageRelayer{client: client}
}

// SubmitPackage submits a parent+child transaction package via
// bitcoind's submitpackage RPC. It validates that the package was
// accepted by checking the package-level result message.
func (r *BitcoindPackageRelayer) SubmitPackage(ctx context.Context,
	parents []*wire.MsgTx, child *wire.MsgTx) error {

	result, err := r.client.SubmitPackage(parents, child, nil)
	if err != nil {
		return fmt.Errorf("submitpackage RPC: %w", err)
	}

	if result.PackageMsg != "success" {
		return fmt.Errorf("package rejected: %s",
			result.PackageMsg)
	}

	return nil
}

// Compile-time check that BitcoindPackageRelayer implements
// PackageRelayer.
var _ PackageRelayer = (*BitcoindPackageRelayer)(nil)
