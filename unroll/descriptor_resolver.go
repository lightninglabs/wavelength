package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/vtxo"
)

// DescriptorLineageResolver gathers lineage material from the current
// single-lineage VTXO descriptor and OOR artifact stores.
type DescriptorLineageResolver struct {
	// VTXOStore provides VTXO descriptor lookups.
	VTXOStore vtxo.VTXOStore

	// ArtifactStore resolves OOR unroll packages for chained VTXOs.
	ArtifactStore packageResolver
}

// ResolveLineage gathers normalized lineage material for one target from the
// descriptor tree path and, when ChainDepth > 0, from locally persisted OOR
// artifacts.
func (r *DescriptorLineageResolver) ResolveLineage(ctx context.Context,
	target wire.OutPoint) (*LineageMaterial, error) {

	if r.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store must be provided")
	}

	desc, err := r.VTXOStore.GetVTXO(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("%w: %w",
			ErrUnrollTargetNotFound, err)
	}

	if err := validateProofDescriptor(desc); err != nil {
		return nil, err
	}

	mat := &LineageMaterial{
		TargetOutpoint: target,
		CSVDelay:       desc.RelativeExpiry,
	}

	if desc.TreePath != nil {
		mat.TreePaths = append(mat.TreePaths, desc.TreePath)
	}

	if desc.ChainDepth > 0 {
		if r.ArtifactStore == nil {
			return nil, fmt.Errorf("%w: missing local OOR "+
				"artifact resolver",
				ErrUnrollProofUnavailable)
		}

		extraNodes, err := r.resolveOORArtifacts(ctx, target, mat)
		if err != nil {
			return nil, err
		}

		mat.ExtraNodes = extraNodes
	}

	return mat, nil
}

// resolveOORArtifacts loads OOR unroll packages and converts them into extra
// recovery nodes.
func (r *DescriptorLineageResolver) resolveOORArtifacts(
	ctx context.Context, target wire.OutPoint,
	mat *LineageMaterial) ([]*recovery.Node, error) {

	resolved, err := r.ArtifactStore.ResolveUnrollPackages(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve unroll packages: %w",
			ErrUnrollProofUnavailable, err)
	}

	treeTxids := make(map[chainhash.Hash]struct{})
	for _, tp := range mat.TreePaths {
		if tp == nil || tp.Root == nil {
			continue
		}

		for treeNode := range tp.Root.NodesIter() {
			tx, err := proofTxFromTreeNode(treeNode)
			if err != nil {
				continue
			}

			treeTxids[tx.TxHash()] = struct{}{}
		}
	}

	var trulyUnresolved []wire.OutPoint
	for _, op := range resolved.UnresolvedCheckpointInputs {
		if _, ok := treeTxids[op.Hash]; !ok {
			trulyUnresolved = append(trulyUnresolved, op)
		}
	}

	if len(trulyUnresolved) > 0 {
		return nil, fmt.Errorf("%w: unresolved checkpoint "+
			"inputs for %v: %v",
			ErrUnrollProofUnavailable, target, trulyUnresolved)
	}

	var extraNodes []*recovery.Node
	seen := make(map[chainhash.Hash]struct{})

	for i := range resolved.Packages {
		pkg := resolved.Packages[i]
		if pkg == nil {
			return nil, fmt.Errorf("%w: package %d missing",
				ErrUnrollProofInvalid, i)
		}

		for j := range pkg.FinalCheckpointPSBTs {
			tx, err := extractFinalizedTx(
				pkg.FinalCheckpointPSBTs[j],
			)
			if err != nil {
				return nil, err
			}

			txid := tx.TxHash()
			if _, ok := seen[txid]; ok {
				continue
			}

			seen[txid] = struct{}{}
			extraNodes = append(extraNodes, &recovery.Node{
				Kind: recovery.NodeKindCheckpoint,
				Tx:   tx,
			})
		}

		tx, err := extractFinalizedTx(pkg.ArkPSBT)
		if err != nil {
			return nil, err
		}

		txid := tx.TxHash()
		if _, ok := seen[txid]; ok {
			continue
		}

		seen[txid] = struct{}{}
		extraNodes = append(extraNodes, &recovery.Node{
			Kind: recovery.NodeKindArk,
			Tx:   tx,
		})
	}

	return extraNodes, nil
}
