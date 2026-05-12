package lndbackend

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/proof"
)

// NewLndHeaderVerifier returns a proof.HeaderVerifier that validates block
// headers against LND's chain backend. It verifies that the given block
// header matches the block at the claimed height on the best chain.
func NewLndHeaderVerifier(
	chainKit lndclient.ChainKitClient) proof.HeaderVerifier {

	return func(header wire.BlockHeader, height uint32) error {
		ctx := context.Background()

		// Get the canonical block hash at the claimed height.
		blockHash, err := chainKit.GetBlockHash(
			ctx, int64(height),
		)
		if err != nil {
			return fmt.Errorf("get block hash at height %d: %w",
				height, err)
		}

		// Compute the hash of the provided header and compare.
		headerHash := header.BlockHash()
		if headerHash != blockHash {
			return fmt.Errorf("block header hash %s does not "+
				"match chain hash %s at height %d", headerHash,
				blockHash, height)
		}

		return nil
	}
}
