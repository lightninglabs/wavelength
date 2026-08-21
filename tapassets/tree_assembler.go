package tapassets

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// AssetTreeRequest describes one asset-aware VTXO tree to build beneath a
// batch output.
type AssetTreeRequest struct {
	// Leaves are the tree's VTXO leaves; every leaf must carry a
	// non-zero asset amount.
	Leaves []tree.LeafDescriptor

	// OperatorKey participates in every node's cosigner set.
	OperatorKey *btcec.PublicKey

	// Radix is the maximum number of children per branch node.
	Radix int

	// BatchOutpoint is the batch output the root node spends.
	BatchOutpoint wire.OutPoint

	// BatchOutput is the batch output itself. Its value must equal the
	// sum of the leaf amounts: node transactions are zero fee.
	BatchOutput *wire.TxOut

	// AssetAmount is the total asset amount carried by the batch
	// output; the leaf amounts must sum to it exactly.
	AssetAmount uint64
}

// BuildAssetTree builds and materializes one asset-aware VTXO tree: pass 1
// shapes the tree and folds subtree asset totals, pass 2 commits one asset
// transition per node through tap-sdk. The returned tree and context must
// stay paired: the context carries the per-node signing tweaks and sealed
// packages the tree's transactions depend on.
func BuildAssetTree(ctx context.Context, cfg TreeMaterializerConfig,
	req AssetTreeRequest) (*tree.Tree, *tree.AssetTreeContext, error) {

	materializer, err := NewTreeMaterializer(cfg)
	if err != nil {
		return nil, nil, err
	}

	return buildAssetTree(ctx, materializer, req)
}

// buildAssetTree runs both passes against an already-constructed
// materializer.
func buildAssetTree(ctx context.Context, materializer *TreeMaterializer,
	req AssetTreeRequest) (*tree.Tree, *tree.AssetTreeContext, error) {

	if req.BatchOutput == nil {
		return nil, nil, fmt.Errorf("batch output is required")
	}
	if req.BatchOutput.Value <= 0 {
		return nil, nil, fmt.Errorf("batch output value must be " +
			"positive")
	}
	if !bytes.Equal(
		req.BatchOutput.PkScript, materializer.cfg.Root.BatchPkScript,
	) {
		return nil, nil, fmt.Errorf("batch output script does not " +
			"match the root asset source")
	}
	if req.BatchOutpoint == (wire.OutPoint{}) {
		return nil, nil, fmt.Errorf("batch outpoint is required")
	}
	if req.OperatorKey == nil {
		return nil, nil, fmt.Errorf("operator key is required")
	}
	if req.Radix < 2 {
		return nil, nil, fmt.Errorf("radix must be at least 2, got %d",
			req.Radix)
	}

	var (
		btcTotal   btcutil.Amount
		assetTotal uint64
		owners     = make(map[string]struct{}, len(req.Leaves))
	)
	for idx := range req.Leaves {
		leaf := &req.Leaves[idx]
		if leaf.Amount <= 0 {
			return nil, nil, fmt.Errorf("leaf %d carrier value "+
				"must be positive", idx)
		}
		if leaf.AssetAmount == 0 {
			return nil, nil, fmt.Errorf("leaf %d carries no "+
				"asset amount", idx)
		}
		if leaf.CoSignerKey == nil {
			return nil, nil, fmt.Errorf("leaf %d cosigner is "+
				"required", idx)
		}
		owner := string(leaf.CoSignerKey.SerializeCompressed())
		if _, ok := owners[owner]; ok {
			return nil, nil, fmt.Errorf("leaf %d repeats a "+
				"cosigner", idx)
		}
		owners[owner] = struct{}{}

		if leaf.Amount > btcutil.Amount(math.MaxInt64)-btcTotal {
			return nil, nil, fmt.Errorf("leaf carrier value " +
				"overflow")
		}
		if leaf.AssetAmount > math.MaxUint64-assetTotal {
			return nil, nil, fmt.Errorf("leaf asset amount " +
				"overflow")
		}
		btcTotal += leaf.Amount
		assetTotal += leaf.AssetAmount
	}
	if btcTotal != btcutil.Amount(req.BatchOutput.Value) {
		return nil, nil, fmt.Errorf("leaf amounts %d do not consume "+
			"the batch output value %d", btcTotal,
			req.BatchOutput.Value)
	}
	if assetTotal != req.AssetAmount {
		return nil, nil, fmt.Errorf("leaf asset amounts %d do not "+
			"consume the batch asset amount %d", assetTotal,
			req.AssetAmount)
	}

	structure, err := tree.BuildStructure(req.Leaves,
		tree.StructureConfig{
			OperatorKey: req.OperatorKey,
			Radix:       req.Radix,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	if structure == nil || structure.AssetContext == nil {
		return nil, nil, fmt.Errorf("asset tree structure has no " +
			"asset context")
	}
	if err := validateTreeProofCapacity(
		materializer.cfg.Root, structure.Root.Depth(),
	); err != nil {
		return nil, nil, err
	}

	// The materializer reads subtree totals from the structure's
	// context; rebind it so callers cannot desynchronize the two.
	materializer.cfg.AssetContext = structure.AssetContext
	structure.AssetContext.SetAssetRef(materializer.cfg.AssetRef.String())

	err = tree.Materialize(
		ctx, structure.Root, tree.MaterializeParams{
			Input: req.BatchOutpoint,
		},
		materializer,
	)
	if err != nil {
		return nil, nil, err
	}

	sweepRoot := materializer.cfg.SweepLeaf.TapHash()
	built := &tree.Tree{
		Root:               structure.Root,
		BatchOutpoint:      req.BatchOutpoint,
		BatchOutput:        req.BatchOutput,
		SweepTapscriptRoot: sweepRoot[:],
		AssetContext:       structure.AssetContext,
	}
	if err := built.Verify(); err != nil {
		return nil, nil, fmt.Errorf("materialized tree is "+
			"inconsistent: %w", err)
	}

	return built, structure.AssetContext, nil
}
