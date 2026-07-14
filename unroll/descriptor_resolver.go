package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/vtxo"
)

// DescriptorLineageResolver assembles the raw transaction graph a VTXO
// needs to surface on chain, strictly from locally persisted state (the
// VTXO store and the OOR artifact store).
//
// "Lineage" here has two parts:
//
//  1. The round-birth ancestry: the tree of transactions the operator
//     signed at round creation, ending in the leaf that carries our
//     VTXO. This lives in the descriptor's TreePath.
//
//  2. Any OOR chain stacked on top: for VTXOs the client received via
//     out-of-round transfers, there is a sequence of checkpoint and
//     ark transactions that hop the funds from one VTXO to the next.
//     These live in the OOR artifact store.
//
// The resolver normalizes both sources into a single LineageMaterial
// bundle that [BuildProofFromMaterial] can stitch into a proof graph.
type DescriptorLineageResolver struct {
	// VTXOStore provides VTXO descriptor lookups.
	VTXOStore vtxo.VTXOStore

	// ArtifactStore resolves OOR unroll packages for chained VTXOs.
	ArtifactStore packageResolver
}

// ResolveLineage produces the lineage material bundle for one target.
//
// The algorithm has three stages:
//
//  1. Load-and-validate the VTXO descriptor. validateProofDescriptor
//     encodes the "hard local start contract" — the descriptor must be
//     non-terminal and have every field we need (TreePath, round id,
//     commitment txid, created height, batch expiry). If any of these
//     are missing we fail fast because no amount of retrying will
//     produce data the local store never had.
//
//  2. Seed the lineage bundle with the CSV delay and the descriptor's
//     round-birth tree path. Every VTXO has at least this ancestry.
//
//  3. If ChainDepth > 0 the VTXO was received via one or more OOR
//     hops, so we delegate to resolveOORArtifacts to walk the artifact
//     store. ChainDepth == 0 means the VTXO came straight from a
//     round, no artifacts needed.
//
// The returned LineageMaterial is caller-owned but the transactions
// inside are not deep-copied; BuildProofFromMaterial treats them as
// read-only.
func (r *DescriptorLineageResolver) ResolveLineage(ctx context.Context,
	target wire.OutPoint) (*LineageMaterial, error) {

	desc, err := r.loadDescriptor(ctx, target)
	if err != nil {
		return nil, err
	}

	if err := validateProofDescriptor(desc); err != nil {
		return nil, err
	}

	return r.resolveValidatedLineage(ctx, target, desc)
}

// ResolveLineageHistorical resolves the lineage of target with the same
// shape-level descriptor validation as ResolveLineage but without
// rejecting terminal-status descriptors (Spent / Forfeited / Failed).
//
// TEST-HARNESS ONLY. Production code must use ResolveLineage so the
// terminal-status guard remains in force; this entry point exists for
// fraud-response itests that need to walk the historical recovery DAG
// of a VTXO that has already been spent or forfeited. See
// LocalProofAssembler.EnsureProofForHarness for the assembler-level
// counterpart.
func (r *DescriptorLineageResolver) ResolveLineageHistorical(
	ctx context.Context, target wire.OutPoint) (*LineageMaterial, error) {

	desc, err := r.loadDescriptor(ctx, target)
	if err != nil {
		return nil, err
	}

	if err := validateProofDescriptorShape(desc); err != nil {
		return nil, err
	}

	return r.resolveValidatedLineage(ctx, target, desc)
}

// loadDescriptor fetches the VTXO descriptor for target and wraps the
// store's "not found" error in the typed ErrUnrollTargetNotFound
// sentinel so callers can distinguish "we do not know about this VTXO"
// from actual storage errors.
func (r *DescriptorLineageResolver) loadDescriptor(ctx context.Context,
	target wire.OutPoint) (*vtxo.Descriptor, error) {

	if r.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store must be provided")
	}

	desc, err := r.VTXOStore.GetVTXO(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnrollTargetNotFound, err)
	}

	return desc, nil
}

// resolveValidatedLineage assembles the LineageMaterial bundle for a
// descriptor that has already passed shape validation. Both
// ResolveLineage (production) and ResolveLineageHistorical (harness)
// share this body: descriptor shape is the same in both paths, and
// only the upstream status-arm decision differs.
func (r *DescriptorLineageResolver) resolveValidatedLineage(ctx context.Context,
	target wire.OutPoint, desc *vtxo.Descriptor) (*LineageMaterial, error) {

	// Seed with the round-birth tree(s) and the descriptor's CSV
	// delay. CSVDelay is the per-VTXO relative expiry the planner
	// later uses to decide when the timeout path is ready.
	mat := &LineageMaterial{
		TargetOutpoint: target,
		CSVDelay:       desc.RelativeExpiry,
	}

	// validateProofDescriptorShape upstream gates every fragment for
	// non-nil TreePath, non-empty tree, and non-zero CommitmentTxID,
	// so we can append every fragment unconditionally here. TreeDepth
	// is deliberately not part of that gate (it is expiry-timing
	// metadata, not proof material).
	for _, a := range desc.Ancestry {
		mat.TreePaths = append(mat.TreePaths, a.TreePath)
	}

	// If the VTXO has any OOR hops, walk the artifact store to
	// collect the checkpoint + ark transactions that stitch the
	// chain together. ChainDepth is the authoritative count of hops,
	// bumped each time the VTXO was received OOR.
	if desc.ChainDepth > 0 {
		if r.ArtifactStore == nil {
			return nil, fmt.Errorf("%w: missing local OOR "+
				"artifact resolver", ErrUnrollProofUnavailable)
		}

		extraNodes, err := r.resolveOORArtifacts(ctx, target, mat)
		if err != nil {
			return nil, err
		}

		mat.ExtraNodes = extraNodes
	}

	return mat, nil
}

// resolveOORArtifacts walks the OOR artifact store for one target and
// normalizes the stored packages into recovery.Node entries.
//
// An OOR package is the bundle of transactions that hopped a VTXO from
// one party to another outside of a round. For recovery purposes we
// care about two sub-transactions in each package:
//
//   - The final checkpoint PSBT(s): one or more bridging transactions
//     that chain from the previous VTXO's outpoint into the next.
//   - The ark PSBT: the transaction that produces the current VTXO
//     output.
//
// The algorithm:
//
//  1. Ask the artifact store to resolve every package whose output
//     eventually leads to this target. The store returns packages plus
//     a list of "unresolved checkpoint inputs" — parents that the
//     store could not locate.
//
//  2. An unresolved input is only fatal if it is not also in the
//     round-birth tree. The tree path supplies the ultimate roots of
//     the lineage, so a checkpoint whose parent is a tree node is
//     resolvable even though the artifact store does not carry it.
//     This cross-check prevents false "incomplete lineage" failures
//     when the first OOR hop consumes a tree leaf directly.
//
//  3. For each resolved package, extract the underlying wire.MsgTx
//     from every checkpoint PSBT and from the ark PSBT, de-duplicate
//     by txid, and tag each with its recovery Kind
//     (NodeKindCheckpoint vs NodeKindArk) so the proof graph knows
//     which role each transaction plays.
//
// Returning fewer nodes than expected is handled upstream by
// validateInputCompleteness — that pass walks the target's inputs and
// fails loudly if any parent is still missing after this function
// returns.
func (r *DescriptorLineageResolver) resolveOORArtifacts(ctx context.Context,
	target wire.OutPoint, mat *LineageMaterial) ([]*recovery.Node, error) {

	resolved, err := r.ArtifactStore.ResolveUnrollPackages(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve unroll packages: %w",
			ErrUnrollProofUnavailable, err)
	}

	// Build an index of every txid already present in the round-birth
	// tree. An OOR package whose earliest parent is a tree node is
	// fully resolvable — the tree path supplies the parent — so we
	// can strike those parents off the artifact store's
	// "unresolved" list.
	treeTxids := make(map[chainhash.Hash]struct{})
	for _, tp := range mat.TreePaths {
		if tp == nil || tp.Root == nil {
			continue
		}

		for treeNode := range tp.Root.NodesIter() {
			tx, err := proofTxFromTreeNode(treeNode)
			if err != nil {
				// Skip degenerate tree nodes rather than fail
				// the whole lineage; BuildProofFromMaterial
				// will reject any downstream conflict anyway.
				continue
			}

			treeTxids[tx.TxHash()] = struct{}{}
		}
	}

	// Any checkpoint input the artifact store marked unresolved but
	// that we find in the tree path is fine. Everything else is a
	// genuine gap and means we cannot assemble a full proof from
	// local state.
	var trulyUnresolved []wire.OutPoint
	for _, op := range resolved.UnresolvedCheckpointInputs {
		if _, ok := treeTxids[op.Hash]; !ok {
			trulyUnresolved = append(trulyUnresolved, op)
		}
	}

	if len(trulyUnresolved) > 0 {
		return nil, fmt.Errorf("%w: unresolved checkpoint inputs for "+
			"%v: %v", ErrUnrollProofUnavailable, target,
			trulyUnresolved)
	}

	// Stitch every package into extraNodes. `seen` collapses
	// duplicates across packages so a shared transaction appears once
	// in the proof (e.g. a checkpoint that bridges two OOR hops).
	var extraNodes []*recovery.Node
	seen := make(map[chainhash.Hash]struct{})

	for i := range resolved.Packages {
		pkg := resolved.Packages[i]
		if pkg == nil {
			return nil, fmt.Errorf("%w: package %d missing",
				ErrUnrollProofInvalid, i)
		}

		// A single OOR package can carry multiple checkpoint PSBTs
		// (e.g. one per input branch when the package spends from a
		// multi-input source).
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

		// Every package also carries exactly one ark-tx that
		// produces the VTXO output at the end of that hop.
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
