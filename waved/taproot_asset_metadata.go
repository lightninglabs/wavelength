package waved

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/vtxo"
)

// indexerTaprootAssetMetadata decodes the optional SDK-neutral asset metadata
// carried by an indexer VTXO. Root-only rows remain valid for compatibility;
// any new identity/amount pair must be complete.
func indexerTaprootAssetMetadata(indexed *arkrpc.VTXO) (*chainhash.Hash, string,
	uint64, error) {

	if indexed == nil {
		return nil, "", 0, fmt.Errorf("indexer vtxo must be provided")
	}

	rootRaw := indexed.GetTaprootAssetRoot()
	assetRef := indexed.GetTaprootAssetRef()
	assetAmount := indexed.GetTaprootAssetAmount()
	if len(rootRaw) == 0 {
		if assetRef != "" || assetAmount != 0 {
			return nil, "", 0, fmt.Errorf("indexer Taproot Asset " +
				"metadata has no commitment root")
		}

		return nil, "", 0, nil
	}

	root, err := chainhash.NewHash(rootRaw)
	if err != nil {
		return nil, "", 0, fmt.Errorf("parse indexer Taproot Asset "+
			"root: %w", err)
	}
	if assetRef == "" && assetAmount == 0 {
		return root, "", 0, nil
	}
	if assetRef == "" || assetAmount == 0 {
		return nil, "", 0, fmt.Errorf("indexer Taproot Asset ref and " +
			"amount must both be provided")
	}
	if len(assetRef) > vtxo.MaxTaprootAssetRefBytes {
		return nil, "", 0, fmt.Errorf("indexer Taproot Asset ref "+
			"exceeds %d bytes", vtxo.MaxTaprootAssetRefBytes)
	}

	return root, assetRef, assetAmount, nil
}
