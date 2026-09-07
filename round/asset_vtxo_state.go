package round

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
)

// setClientVTXOAssetState retains the verified transition with its leaf. A
// missing marker would let a restarted wallet treat carrier sats as Bitcoin.
func setClientVTXOAssetState(cv *ClientVTXO, req types.VTXORequest,
	clientTree *tree.Tree, leaf *tree.Node) error {

	assetContext := clientTree.AssetContext
	if req.AssetRef == "" {
		if assetContext != nil {
			return fmt.Errorf("Bitcoin VTXO request has an asset " +
				"tree")
		}

		return nil
	}
	if assetContext == nil {
		return fmt.Errorf("asset VTXO is missing tree context")
	}
	if err := assetContext.Validate(clientTree.Root); err != nil {
		return fmt.Errorf("asset VTXO tree context: %w", err)
	}
	if assetContext.AssetRef() != req.AssetRef ||
		assetContext.NodeAssetAmount(leaf) != req.AssetAmount {
		return fmt.Errorf("asset VTXO identity does not match request")
	}
	rootBytes := assetContext.LeafAssetRoot(leaf.Input)
	if len(rootBytes) != chainhash.HashSize {
		return fmt.Errorf("asset VTXO is missing leaf commitment root")
	}
	sealedPackage := assetContext.SealedPackage(leaf.Input)
	if len(sealedPackage) == 0 {
		return fmt.Errorf("asset VTXO is missing sealed leaf package")
	}
	var root chainhash.Hash
	copy(root[:], rootBytes)
	cv.TaprootAssetRoot = &root
	cv.TaprootAssetRef = req.AssetRef
	cv.TaprootAssetAmount = req.AssetAmount
	cv.TaprootAssetSealedPackage = bytes.Clone(sealedPackage)

	return nil
}
