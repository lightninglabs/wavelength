package vtxo

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// MaxAncestryPaths bounds the per-VTXO ancestry slice the indexer is
// allowed to return. Real cross-round multi-input OOR VTXOs see at most
// a handful of contributing commitments; the cap exists so a misbehaving
// or compromised indexer cannot force unbounded allocation here before
// the per-entry validation runs.
const MaxAncestryPaths = 64

// AncestryFromRPC converts a slice of arkrpc.AncestryPath into the typed
// vtxo.Ancestry shape used by descriptors and incoming-receive
// pipelines. Returns an error when the slice is empty (a VTXO without
// ancestry would persist as unexitable, so version-skew producers that
// still send the retired tree_path/tree_depth scalars must fail closed
// here rather than silently materialize a stranded descriptor) or when
// the slice exceeds MaxAncestryPaths.
//
// Lives in the vtxo package so both the OOR receive path (which routes
// through the durable QueryIncomingMetadataRequest outbox) and the
// in-round receive path (which materializes synchronously inside
// IncomingVTXOHandler from a thin IncomingVTXOEvent push) share one
// validator and one conversion. Drift between the two would silently
// produce non-exitable descriptors on one path but not the other —
// exactly the symptom of bug-3 in the working BUGS_FOUND.md.
func AncestryFromRPC(paths []*arkrpc.AncestryPath) ([]Ancestry, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("indexer vtxo missing ancestry paths")
	}

	if len(paths) > MaxAncestryPaths {
		return nil, fmt.Errorf("indexer vtxo ancestry exceeds cap: "+
			"got %d, max %d", len(paths), MaxAncestryPaths)
	}

	out := make([]Ancestry, 0, len(paths))
	for i, p := range paths {
		if p == nil {
			continue
		}

		treePath, err := arkrpc.AncestryPathToTree(p)
		if err != nil {
			return nil, fmt.Errorf("path[%d] tree: %w", i, err)
		}

		commitmentTxID, err := arkrpc.AncestryCommitmentTxID(p)
		if err != nil {
			return nil, fmt.Errorf("path[%d] commitment: %w", i,
				err)
		}

		// Validate the indexer-supplied tree_depth against the
		// reconstructed path before it can be persisted. A zero or
		// truncated claim would otherwise survive the rest of the
		// receive-side checks and only fail at unilateral-exit time
		// (zero) or under-report the worst-case CSV window
		// (truncated), which is a fund-availability surface for
		// OOR-received VTXOs.
		err = arkrpc.ValidateAncestryPathDepth(
			p.GetTreeDepth(), treePath,
		)
		if err != nil {
			return nil, fmt.Errorf("path[%d] depth: %w", i, err)
		}

		out = append(out, Ancestry{
			TreePath:         treePath,
			CommitmentTxID:   commitmentTxID,
			InputIndices:     slices.Clone(p.GetInputIndices()),
			TreeDepth:        p.GetTreeDepth(),
			CommitmentHeight: p.GetCommitmentHeight(),
		})
	}

	return out, nil
}

// VerifyReceivedVTXOBinding cryptographically binds a received in-round VTXO to
// the commitment(s) the client will watch for reorg safety. It PRESUMES the
// caller has already authenticated each tree's batch output against the real
// commitment transaction (batchcanon.EvidenceFromAncestryPaths: the commitment
// tx self-authenticates via tx.TxHash()==commitmentTxID, and its output at the
// batch index is matched byte-for-byte). That makes each tree's BatchOutput the
// real PayToTaproot(round key) — the on-chain anchor this proof leans on.
//
// Without this, the indexer's ancestry is trusted for shape only, so a
// malicious indexer could name a decoy commitment (real, deeply-buried, never
// reorging) and have the client watch it while the VTXO's true lineage silently
// leaves the chain. Running tree signature verification ALONE does not close
// that: signatures verify against a per-node aggregate key recomputed from the
// tree's own (attacker-supplied) cosigners, so an attacker signs a fabricated
// tree with their own keys and passes. The load-bearing check here is (3): each
// node's aggregate key must equal the taproot key of the output it spends. At
// the root that output is the authenticated commitment output, so the root key
// is pinned to the real round key (unforgeable); taproot's SIGHASH_DEFAULT
// commits to each tx's outputs, so that pin cascades down to every leaf.
//
// For each ancestry tree it proves:
//  1. structure — root spends BatchOutpoint, children reference parent txids
//     (tree.Verify);
//  2. FinalKey — recomputed per node from its cosigners + the sweep tapscript
//     root (reconstruction drops it);
//  3. key-to-output binding — PayToTaproot(node key) == pkScript of the output
//     the node spends, anchored at the root by the authenticated batch output;
//  4. signatures — every node validly signed (tree.VerifySigned).
//
// It then requires that some leaf of an authenticated tree pays the received
// VTXO script, so the proven lineage actually terminates at this coin. Genuine
// trees satisfy (3) by construction (each parent output IS PayToTaproot of its
// child's aggregate), so this never false-rejects a real receive; and it is
// key-identity-agnostic (never demands a specific key), so it does not depend on
// the operator identity key (which is NOT the tree's per-round signing key).
func VerifyReceivedVTXOBinding(ancestry []Ancestry, vtxoScript []byte) error {
	if len(vtxoScript) == 0 {
		return fmt.Errorf("missing pkScript for received-VTXO binding")
	}

	paysReceivedVTXO := false
	for i := range ancestry {
		tp := ancestry[i].TreePath
		if tp == nil || tp.Root == nil {
			return fmt.Errorf("ancestry path %d has no tree", i)
		}

		if err := verifyAuthenticatedTree(tp); err != nil {
			return fmt.Errorf("ancestry path %d: %w", i, err)
		}

		for _, leaf := range tp.Root.GetLeafNodes() {
			if len(leaf.Outputs) > 0 && bytes.Equal(
				leaf.Outputs[0].PkScript, vtxoScript,
			) {
				paysReceivedVTXO = true
			}
		}
	}

	if !paysReceivedVTXO {
		return fmt.Errorf("no authenticated ancestry tree pays the " +
			"received VTXO script")
	}

	return nil
}

// VerifyOORAncestryLineage cryptographically binds an OOR-received VTXO's
// commitment lineage to the reorg watches the client is about to arm. Like the
// in-round binding it proves each ancestry tree is a genuine, operator-signed
// descent from its authenticated commitment output (verifyAuthenticatedTree),
// closing the same decoy-commitment gap. It differs in the anchor and the
// terminator:
//
//   - Anchor: OOR incoming metadata can be a durable replay, so rather than
//     trust the reconstructed tree's own batch output it re-anchors on the
//     caller-validated BatchEvidence — the tree's batch outpoint and output
//     script must equal the authenticated evidence for that commitment before
//     the key-to-output cascade is meaningful.
//   - Terminator: an OOR output is an ark-tx output, not a commitment-tree
//     leaf, so it does NOT require a leaf to pay the received VTXO. Instead it
//     binds the coin's real inputs to the watched lineage: the received coin's
//     ark tx spends, via its checkpoints, the committed base VTXOs, and each
//     checkpoint's prevout (a coinInput) must be produced by an authenticated
//     ancestry leaf. This closes the genuine-but-unrelated (decoy) commitment
//     variant: a real tree from an unrelated round has no leaf producing the
//     coin's actual inputs. It is applied only when chainDepth == 1 (a single
//     OOR hop, so the checkpoint prevouts ARE the committed leaves); for
//     chainDepth > 1 the coin's direct inputs are prior OOR VTXOs whose
//     connection to these commitments runs through intermediate hops the
//     recipient does not hold, so the strict bind is not locally verifiable and
//     only the authenticated-tree check (forged-tree variant) applies there.
func VerifyOORAncestryLineage(ancestry []Ancestry,
	evidence []batchcanon.BatchEvidence, chainDepth int,
	coinInputs []wire.OutPoint) error {

	byCommitment := make(
		map[chainhash.Hash]batchcanon.BatchEvidence, len(evidence),
	)
	for _, e := range evidence {
		byCommitment[e.BatchTxID] = e
	}

	// The set of committed base-VTXO txids the authenticated trees produce.
	leafTxids := make(map[chainhash.Hash]struct{})

	for i := range ancestry {
		tp := ancestry[i].TreePath
		if tp == nil || tp.Root == nil || tp.BatchOutput == nil {
			return fmt.Errorf("ancestry path %d has no tree", i)
		}

		e, ok := byCommitment[tp.BatchOutpoint.Hash]
		if !ok {
			return fmt.Errorf("ancestry path %d: no authenticated "+
				"evidence for commitment %s", i,
				tp.BatchOutpoint.Hash)
		}
		if tp.BatchOutpoint.Index != e.BatchOutputIndex ||
			!bytes.Equal(
				tp.BatchOutput.PkScript, e.ConfirmationPkScript,
			) {

			return fmt.Errorf("ancestry path %d: tree batch output "+
				"does not match the authenticated commitment "+
				"output", i)
		}

		if err := verifyAuthenticatedTree(tp); err != nil {
			return fmt.Errorf("ancestry path %d: %w", i, err)
		}

		for _, leaf := range tp.Root.GetLeafNodes() {
			txid, err := leaf.TXID()
			if err != nil {
				return fmt.Errorf("ancestry path %d: leaf "+
					"txid: %w", i, err)
			}
			leafTxids[txid] = struct{}{}
		}
	}

	// chainDepth is intentionally not consulted: it is indexer-supplied, and
	// trusting it would let an attacker skip the bind by claiming a deeper
	// chain. The bind is unconditional.
	_ = chainDepth

	return bindCoinInputsToLeaves(leafTxids, coinInputs)
}

// bindCoinInputsToLeaves enforces the terminator: every coin input (checkpoint
// prevout) must be produced by an authenticated ancestry leaf, so a
// genuine-but-unrelated commitment tree cannot be watched in place of the coin's
// real lineage. The bind is UNCONDITIONAL and never keys off the indexer-
// supplied chain depth: trusting that value would let an attacker claim a deeper
// chain to skip the check on a single-hop coin. coinInputs come from the
// co-signed checkpoint PSBTs (the coin's real spent outpoints), so a decoy
// ancestry — whose leaves are not the coin's actual inputs — is rejected.
func bindCoinInputsToLeaves(leafTxids map[chainhash.Hash]struct{},
	coinInputs []wire.OutPoint) error {

	if len(coinInputs) == 0 {
		return fmt.Errorf("OOR receive has no coin inputs to bind " +
			"against ancestry")
	}
	for _, in := range coinInputs {
		if _, ok := leafTxids[in.Hash]; !ok {
			return fmt.Errorf("coin input %s is not produced by any "+
				"authenticated ancestry leaf", in)
		}
	}

	return nil
}

// verifyAuthenticatedTree proves a tree whose BatchOutput is already
// authenticated is genuine: valid structure, every node's aggregate key equals
// the taproot key of the output it spends, and every node validly signed.
func verifyAuthenticatedTree(tp *tree.Tree) error {
	// (1) Structural integrity: root spends BatchOutpoint, and each child's
	// input references its parent's real txid:index. This makes the prevout
	// fetched below genuinely the parent's output.
	if err := tp.Verify(); err != nil {
		return fmt.Errorf("structure: %w", err)
	}

	// The prevout fetcher maps each node's input to the output it spends: the
	// authenticated batch output for the root, and the parent's output for
	// every child.
	fetcher, err := tp.Root.PrevOutputFetcher(tp.BatchOutput)
	if err != nil {
		return fmt.Errorf("prevouts: %w", err)
	}

	// (2)+(3): recompute each node's aggregate key and require it to equal the
	// taproot key of the output it spends. The root is pinned to the
	// authenticated commitment output; the cascade pins the rest.
	if err := tp.Root.ForEach(func(n *tree.Node) error {
		finalKey, err := tree.ComputeFinalKey(
			n.CoSigners, tp.SweepTapscriptRoot,
		)
		if err != nil {
			return fmt.Errorf("recompute final key: %w", err)
		}
		n.FinalKey = finalKey

		wantScript, err := txscript.PayToTaprootScript(finalKey)
		if err != nil {
			return fmt.Errorf("derive taproot script: %w", err)
		}
		prevOut := fetcher.FetchPrevOutput(n.Input)
		if prevOut == nil ||
			!bytes.Equal(wantScript, prevOut.PkScript) {

			return fmt.Errorf("node aggregate key does not match " +
				"the output it spends")
		}

		return nil
	}); err != nil {
		return err
	}

	// (4) Every node's signature must verify. Given (3), an attacker cannot
	// forge these without the real round's keys.
	if err := tp.VerifySigned(); err != nil {
		return fmt.Errorf("signatures: %w", err)
	}

	return nil
}
