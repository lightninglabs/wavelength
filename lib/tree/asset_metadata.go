package tree

import (
	"github.com/btcsuite/btcd/btcutil"
)

// NodeMetadata aggregates optional bookkeeping for a node and its subtree.
// The heavy Transfer field has been removed - builders are now reconstructed
// on demand using MakeNodeBuilder.
type NodeMetadata struct {
	// LeafPkScript stores the leaf output script from the LeafDescriptor.
	// This is used by BTCMaterializer to construct the leaf output. For
	// asset trees, the output script is determined by tapd and this field
	// is unused.
	LeafPkScript []byte

	// AssetProof stores the serialized asset proof for this node's output.
	// This is populated after finalization and used as the input proof for
	// child nodes during reconstruction.
	AssetProof []byte

	// Leaf carries the leaf-level metadata when this node is a leaf.
	// Nil for internal (branch) nodes.
	Leaf *LeafAssetMetadata
}

// FundingMode describes who supplied the BTC liquidity for a leaf.
type FundingMode uint8

const (
	// FundingModeUnknown is the zero value until explicitly set.
	FundingModeUnknown FundingMode = iota

	// FundingModeOperatorProvided indicates the operator funded the BTC.
	FundingModeOperatorProvided

	// FundingModeClientGas indicates the client funded their own BTC.
	FundingModeClientGas
)

// String returns a human readable representation of the funding mode.
func (f FundingMode) String() string {
	switch f {
	case FundingModeOperatorProvided:
		return "operator"
	case FundingModeClientGas:
		return "client"
	default:
		return "unknown"
	}
}

// LeafFunding tracks the BTC amount and payer for a leaf.
type LeafFunding struct {
	Mode   FundingMode
	Amount btcutil.Amount
}

// LeafAssetMetadata carries leaf-specific context for asset tree leaves.
//
// The Proof field was removed as input proofs are now derivable from the
// parent node's AssetProof. The Transfer field was removed from NodeMetadata
// as builders are reconstructed on demand using RebuildNodeBuilder.
type LeafAssetMetadata struct {
	// InputProof is the serialized proof for the input being spent when
	// constructing this leaf's transaction. This is only used during
	// initial tree construction and is not persisted in NodeMetadata.Leaf.
	// After construction, the input proof is available from the parent
	// node's Metadata.AssetProof.
	InputProof []byte

	// AssetAmount records the asset value (in asset units) anchored by this
	// leaf.
	AssetAmount uint64

	// Funding attributes the BTC dust that funds this anchor.
	Funding LeafFunding

	// ChangePkScript is where BTC reimbursements should be sent.
	ChangePkScript []byte

	// ExitRebalance records BTC owed on exit. Positive means the operator
	// owes the client; negative means the client repays the operator.
	ExitRebalance btcutil.Amount

	// Labels are free-form annotations for tooling/telemetry.
	Labels map[string]string
}

// AssetMetadata is an alias for LeafAssetMetadata for backward compatibility.
// Deprecated: Use LeafAssetMetadata directly.
type AssetMetadata = LeafAssetMetadata
