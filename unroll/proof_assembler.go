package unroll

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/vtxo"
)

var (
	// ErrUnrollTargetNotFound indicates the requested local target does not
	// exist or cannot be used for unilateral exit.
	ErrUnrollTargetNotFound = errors.New("unroll target not found")

	// ErrUnrollProofUnavailable indicates local data was insufficient
	// to build a unilateral-exit proof.
	ErrUnrollProofUnavailable = errors.New("unroll proof unavailable")

	// ErrUnrollProofInvalid indicates a locally assembled or decoded
	// proof was invalid.
	ErrUnrollProofInvalid = errors.New("unroll proof invalid")
)

type packageResolver interface {
	ResolveUnrollPackages(ctx context.Context,
		outpoint wire.OutPoint) (*db.OORUnrollPackages, error)
}

// LocalProofAssembler builds unilateral-exit proofs from strictly local state.
type LocalProofAssembler struct {
	// Resolver gathers normalized lineage material for a target. When nil,
	// EnsureProof falls back to DescriptorLineageResolver using
	// VTXOStore and
	// ArtifactStore.
	Resolver LineageResolver

	// VTXOStore provides VTXO descriptor lookups for proof assembly.
	VTXOStore vtxo.VTXOStore

	// ArtifactStore resolves OOR unroll packages for chained VTXOs.
	ArtifactStore packageResolver
}

// EnsureProof builds an immutable [recovery.Proof] for the target
// entirely from local authoritative state (the VTXO descriptor store
// and the OOR artifact store).
//
// The strict-locality contract is deliberate: if the operator is
// cooperating we would never be unrolling in the first place, so the
// unroll flow cannot depend on any operator RPC. Everything it needs
// must have been persisted when the VTXO was received (round commit,
// tree path) or when it was chained through OOR (checkpoint artifacts).
//
// The assembler itself is stateless; heavy lifting happens inside the
// configured [LineageResolver] (typically [DescriptorLineageResolver])
// which walks the VTXO store plus any OOR chain back to its round roots
// and normalizes the resulting transactions into a [LineageMaterial]
// bundle. [BuildProofFromMaterial] then stitches that bundle into a
// proof graph.
func (a *LocalProofAssembler) EnsureProof(ctx context.Context,
	target wire.OutPoint) (*recovery.Proof, error) {

	if a == nil {
		return nil, fmt.Errorf("proof assembler must be provided")
	}

	resolver := a.resolver()
	mat, err := resolver.ResolveLineage(ctx, target)
	if err != nil {
		return nil, err
	}

	return BuildProofFromMaterial(mat)
}

// EnsureProofForHarness is identical to EnsureProof except that it
// resolves the lineage of a target whose VTXO has already transitioned
// to a terminal status (Spent / Forfeited / Failed). EnsureProof
// rejects terminal targets because no production unroll job can
// usefully start from one — the VTXO no longer exists to be swept.
//
// This entry point exists so test harnesses can walk the historical
// recovery DAG of a terminal VTXO and force-broadcast individual
// lineage transactions to provoke server-side fraud-response paths
// (e.g. a previous owner attempting to unilaterally unroll a VTXO
// they have already forfeited).
//
// PRODUCTION CODE MUST NOT CALL THIS METHOD. Every other descriptor
// shape invariant (ancestry present, tree paths well-formed, commitment
// txid set, etc.) is still enforced — only the terminal-status arm of
// validateProofDescriptor is skipped.
func (a *LocalProofAssembler) EnsureProofForHarness(ctx context.Context,
	target wire.OutPoint) (*recovery.Proof, error) {

	if a == nil {
		return nil, fmt.Errorf("proof assembler must be provided")
	}

	resolver, ok := a.resolver().(historicalLineageResolver)
	if !ok {
		return nil, fmt.Errorf("configured lineage resolver does not " +
			"support historical (terminal-tolerant) walks")
	}

	mat, err := resolver.ResolveLineageHistorical(ctx, target)
	if err != nil {
		return nil, err
	}

	return BuildProofFromMaterial(mat)
}

// historicalLineageResolver is the optional capability a LineageResolver
// can implement to support terminal-tolerant lineage walks for test
// harnesses. Production code paths use ResolveLineage on the base
// LineageResolver interface and never reach this surface.
type historicalLineageResolver interface {
	// ResolveLineageHistorical resolves the lineage of target with the
	// same shape-level validation ResolveLineage applies, but without
	// rejecting terminal-status descriptors.
	ResolveLineageHistorical(ctx context.Context,
		target wire.OutPoint) (*LineageMaterial, error)
}

// resolver returns the configured LineageResolver or creates a fallback
// DescriptorLineageResolver from the legacy fields.
func (a *LocalProofAssembler) resolver() LineageResolver {
	if a.Resolver != nil {
		return a.Resolver
	}

	return &DescriptorLineageResolver{
		VTXOStore:     a.VTXOStore,
		ArtifactStore: a.ArtifactStore,
	}
}

// BuildProofFromMaterial stitches a [LineageMaterial] bundle into an
// immutable [recovery.Proof].
//
// The build is three passes:
//
//  1. Walk every TreePath's node graph in pre-order, turning each
//     tree.Node into a recovery.Node of kind NodeKindTree. Duplicate
//     txids across tree paths are tolerated (the bundle may contain
//     overlapping ancestry) but conflicting duplicates — same txid,
//     different raw bytes — are rejected so we cannot ship a proof
//     that is internally ambiguous about which transaction is signed.
//
//  2. Append any ExtraNodes (e.g. OOR hop transactions pulled from
//     the artifact store), subject to the same conflict check.
//
//  3. Call recovery.NewProof, which builds the actual DAG and checks
//     the target's outpoint is reachable. Then validateInputCompleteness
//     walks the target transaction's inputs to make sure every parent
//     either resides in the proof graph or is a known external input
//     (batch outpoint) — this catches the lineage-resolver returning
//     a proof that skipped some branch.
//
// The returned proof is immutable. No caller in the unroll flow ever
// mutates the returned graph; the planner and FSM use it as read-only
// reference data throughout the actor's lifetime.
func BuildProofFromMaterial(mat *LineageMaterial) (*recovery.Proof, error) {
	if err := mat.Validate(); err != nil {
		return nil, err
	}

	nodes := make([]*recovery.Node, 0)
	seen := make(map[chainhash.Hash]*recovery.Node)

	for i, tp := range mat.TreePaths {
		if err := addTreePathNodes(&nodes, seen, tp); err != nil {
			return nil, fmt.Errorf("tree path %d: %w", i, err)
		}
	}

	for _, extra := range mat.ExtraNodes {
		if err := addProofNode(&nodes, seen, extra); err != nil {
			return nil, err
		}
	}

	proof, err := recovery.NewProof(
		mat.TargetOutpoint, mat.CSVDelay, nodes...,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnrollProofInvalid, err)
	}

	if err := validateInputCompleteness(proof, mat); err != nil {
		return nil, err
	}

	return proof, nil
}

// validateInputCompleteness checks that every input of the target
// transaction has a known parent. A valid parent is either:
//
//   - A node inside the proof graph (standard ancestor), or
//   - A known external batch outpoint (the root of a tree path — the
//     parent of a tree is a round commitment, which is broadcast by
//     the operator and does not live in the client's proof set).
//
// If an input spends from anything else, the proof is incomplete — the
// lineage resolver missed a branch. We fail loudly rather than ship a
// proof that would make the FSM sit in AwaitingMaterialization forever
// waiting on a transaction it cannot produce.
func validateInputCompleteness(proof *recovery.Proof,
	mat *LineageMaterial) error {

	targetNode, err := proof.TargetNode()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnrollProofInvalid, err)
	}

	isRoot := false
	for _, rootTxid := range proof.RootTxids() {
		if rootTxid == proof.TargetOutpoint().Hash {
			isRoot = true
			break
		}
	}

	if isRoot {
		return nil
	}

	knownExternal := make(map[chainhash.Hash]struct{})
	for _, tp := range mat.TreePaths {
		if tp == nil {
			continue
		}

		knownExternal[tp.BatchOutpoint.Hash] = struct{}{}
	}

	for _, txIn := range targetNode.Tx.TxIn {
		parentHash := txIn.PreviousOutPoint.Hash
		if _, inProof := proof.Node(parentHash); inProof {
			continue
		}

		if _, ext := knownExternal[parentHash]; ext {
			continue
		}

		return fmt.Errorf("%w: target %s has input spending from %s "+
			"which is neither in the proof nor a known external "+
			"input (incomplete lineage branch)",
			ErrUnrollProofUnavailable, proof.TargetOutpoint().Hash,
			parentHash)
	}

	return nil
}

// validateProofDescriptor enforces the hard local start contract for one
// unilateral-exit target descriptor: shape invariants plus a non-terminal
// status check. Production callers (ResolveLineage) use this; the
// harness-only ResolveLineageHistorical path skips the status arm by
// calling validateProofDescriptorShape directly.
func validateProofDescriptor(desc *vtxo.Descriptor) error {
	if err := validateProofDescriptorShape(desc); err != nil {
		return err
	}

	if err := validateProofDescriptorActive(desc); err != nil {
		return err
	}

	return nil
}

// validateProofDescriptorActive checks only that the descriptor is not in
// a terminal status (Spent / Forfeited / Failed). A terminal target
// cannot drive a production unroll job because the VTXO no longer exists
// to be swept; the test-harness historical walker bypasses this arm.
func validateProofDescriptorActive(desc *vtxo.Descriptor) error {
	switch desc.Status {
	case vtxo.VTXOStatusSpent,
		vtxo.VTXOStatusForfeited,
		vtxo.VTXOStatusFailed:
		return fmt.Errorf("%w: target %v is terminal (%s)",
			ErrUnrollTargetNotFound, desc.Outpoint, desc.Status)

	default:
		return nil
	}
}

// validateProofDescriptorShape checks every non-status invariant the
// proof builder needs from a descriptor: the descriptor itself,
// ancestry presence and per-fragment shape, round-context fields, and
// non-negative chain depth. Both the production and harness lineage
// paths must enforce these — a missing tree path or zero commitment
// txid would otherwise surface deep inside addTreePathNodes as a
// confusing "tree path missing root".
func validateProofDescriptorShape(desc *vtxo.Descriptor) error {
	switch {
	case desc == nil:
		return fmt.Errorf("%w: descriptor missing",
			ErrUnrollTargetNotFound)

	case len(desc.Ancestry) == 0:
		return fmt.Errorf("%w: descriptor missing ancestry",
			ErrUnrollProofUnavailable)

	case desc.CommitmentTxID == (chainhash.Hash{}):
		return fmt.Errorf("%w: descriptor missing commitment txid",
			ErrUnrollProofUnavailable)

	case desc.RoundID == "":
		return fmt.Errorf("%w: descriptor missing round id",
			ErrUnrollProofUnavailable)

	case desc.CreatedHeight == 0:
		return fmt.Errorf("%w: descriptor missing created height",
			ErrUnrollProofUnavailable)

	case desc.BatchExpiry == 0:
		return fmt.Errorf("%w: descriptor missing batch expiry",
			ErrUnrollProofUnavailable)

	case desc.ChainDepth < 0:
		return fmt.Errorf("%w: invalid chain depth %d",
			ErrUnrollProofInvalid, desc.ChainDepth)
	}

	// Per-fragment well-formedness. The slice-length check above catches
	// the empty-ancestry case; this loop catches the harder one where the
	// slice has entries but a fragment is structurally unusable. Without
	// these checks a nil or zero-rooted TreePath would slip past
	// validateProofDescriptor and surface deep inside addTreePathNodes
	// with a confusing "tree path missing root" — at which point the
	// FSM has already advanced into AwaitingMaterialization with a
	// proof set it can never assemble. Fail fast at the boundary.
	for i, frag := range desc.Ancestry {
		switch {
		case frag.TreePath == nil:
			return fmt.Errorf("%w: ancestry fragment %d missing "+
				"tree path", ErrUnrollProofUnavailable, i)

		case frag.TreePath.Root == nil:
			return fmt.Errorf("%w: ancestry fragment %d has "+
				"empty tree", ErrUnrollProofUnavailable, i)

		case frag.CommitmentTxID == (chainhash.Hash{}):
			return fmt.Errorf("%w: ancestry fragment %d missing "+
				"commitment txid", ErrUnrollProofUnavailable, i)

		case frag.TreeDepth == 0:
			return fmt.Errorf("%w: ancestry fragment %d has zero "+
				"tree depth", ErrUnrollProofUnavailable, i)
		}
	}

	return nil
}

// addTreePathNodes appends the round-birth ancestry from one descriptor tree
// path into the in-progress proof node set.
func addTreePathNodes(nodes *[]*recovery.Node,
	seen map[chainhash.Hash]*recovery.Node, treePath *tree.Tree) error {

	if treePath == nil || treePath.Root == nil {
		return fmt.Errorf("%w: tree path missing root",
			ErrUnrollProofUnavailable)
	}

	for treeNode := range treePath.Root.NodesIter() {
		tx, err := proofTxFromTreeNode(treeNode)
		if err != nil {
			return err
		}

		node := &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   tx,
		}

		if err := addProofNode(nodes, seen, node); err != nil {
			return err
		}
	}

	return nil
}

// addProofNode appends one proof node while rejecting conflicting duplicate
// txids.
func addProofNode(nodes *[]*recovery.Node,
	seen map[chainhash.Hash]*recovery.Node, node *recovery.Node) error {

	txid, err := node.TXID()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnrollProofInvalid, err)
	}

	existing, ok := seen[txid]
	if ok {
		equal, err := sameNode(existing, node)
		if err != nil {
			return err
		}

		if !equal {
			return fmt.Errorf("%w: conflicting proof node %s",
				ErrUnrollProofInvalid, txid)
		}

		return nil
	}

	seen[txid] = node
	*nodes = append(*nodes, node)

	return nil
}

// sameNode reports whether two proof nodes represent the same transaction and
// role.
func sameNode(a, b *recovery.Node) (bool, error) {
	switch {
	case a == nil || b == nil:
		return false, fmt.Errorf("%w: proof node cannot be nil",
			ErrUnrollProofInvalid)

	case a.Kind != b.Kind:
		return false, nil
	}

	var aBuf bytes.Buffer
	if err := a.Tx.Serialize(&aBuf); err != nil {
		return false, err
	}

	var bBuf bytes.Buffer
	if err := b.Tx.Serialize(&bBuf); err != nil {
		return false, err
	}

	return bytes.Equal(aBuf.Bytes(), bBuf.Bytes()), nil
}

// extractFinalizedTx prefers the fully finalized transaction from a persisted
// PSBT, but falls back to the unsigned transaction for synthetic test packets.
func extractFinalizedTx(pkt *psbt.Packet) (*wire.MsgTx, error) {
	if pkt == nil {
		return nil, fmt.Errorf("%w: psbt must be provided",
			ErrUnrollProofInvalid)
	}

	raw, err := psbtutil.Serialize(pkt)
	if err != nil {
		return nil, fmt.Errorf("%w: serialize psbt: %w",
			ErrUnrollProofInvalid, err)
	}

	cloned, err := psbtutil.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: parse psbt: %w",
			ErrUnrollProofInvalid, err)
	}

	tx, extractErr := psbt.Extract(cloned)
	if extractErr == nil {
		return tx, nil
	}

	err = psbt.MaybeFinalizeAll(cloned)
	if err == nil {
		tx, extractErr = psbt.Extract(cloned)
		if extractErr == nil {
			return tx, nil
		}
	}

	for i := range cloned.Inputs {
		if len(cloned.Inputs[i].FinalScriptWitness) > 0 {
			continue
		}

		if err := psbt.Finalize(cloned, i); err == nil {
			continue
		}

		err := finalizeTaprootScriptSpend(
			&cloned.Inputs[i],
		)
		if err != nil {
			return nil, fmt.Errorf("%w: finalize taproot script "+
				"spend input %d: %v", ErrUnrollProofInvalid, i,
				err)
		}
	}

	tx, extractErr = psbt.Extract(cloned)
	if extractErr == nil {
		return tx, nil
	}

	return nil, fmt.Errorf("%w: psbt not fully finalized (last extract "+
		"error: %v)", ErrUnrollProofInvalid, extractErr)
}

// finalizeTaprootScriptSpend constructs FinalScriptWitness from PSBT taproot
// script-spend signature fields.
func finalizeTaprootScriptSpend(in *psbt.PInput) error {
	if len(in.TaprootScriptSpendSig) == 0 {
		return fmt.Errorf("no taproot script spend signatures")
	}

	if len(in.TaprootLeafScript) == 0 {
		return fmt.Errorf("no taproot leaf scripts")
	}

	leaf := in.TaprootLeafScript[0]
	var witnessItems [][]byte
	for _, sig := range in.TaprootScriptSpendSig {
		witnessItems = append(witnessItems, sig.Signature)
	}

	witnessItems = append(witnessItems, leaf.Script)
	witnessItems = append(witnessItems, leaf.ControlBlock)

	witness := wire.TxWitness(witnessItems)
	var buf bytes.Buffer
	if err := psbt.WriteTxWitness(&buf, witness); err != nil {
		return fmt.Errorf("encode witness: %w", err)
	}
	in.FinalScriptWitness = buf.Bytes()

	return nil
}

// proofTxFromTreeNode prefers the signed tree transaction when available, but
// falls back to the unsigned form for synthetic test trees.
func proofTxFromTreeNode(node *tree.Node) (*wire.MsgTx, error) {
	if node == nil {
		return nil, fmt.Errorf("%w: tree node missing",
			ErrUnrollProofInvalid)
	}

	tx, err := node.ToSignedTx()
	if err == nil {
		return tx, nil
	}

	tx, err = node.ToTx()
	if err != nil {
		return nil, fmt.Errorf("%w: tree node tx: %w",
			ErrUnrollProofInvalid, err)
	}

	return tx, nil
}
