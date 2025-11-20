package tree

import (
	"github.com/btcsuite/btcd/btcutil"
)

// AssetLeafMetadata carries leaf-specific context for asset tree leaves.
// This data is stored in the AssetContext during tree construction and
// materialization.
type AssetLeafMetadata struct {
	// InputProof is the serialized proof for the input being spent when
	// constructing this leaf's transaction. This is only used during
	// initial tree construction.
	InputProof []byte

	// AssetAmount records the asset value (in asset units) anchored by this
	// leaf.
	AssetAmount uint64

	// Funding is the BTC amount that funds this anchor. The operator always
	// provides the BTC liquidity.
	Funding btcutil.Amount

	// ChangePkScript is where BTC reimbursements should be sent.
	ChangePkScript []byte
}
