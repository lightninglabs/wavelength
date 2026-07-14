package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/stretchr/testify/require"
)

// fakeProofAssembler returns a pre-built proof when
// EnsureProofForHarness is called for the configured target. Used by
// GetVTXOLineageTx tests to inject deterministic recovery DAGs
// without standing up the full VTXOStore + ArtifactStore wiring.
type fakeProofAssembler struct {
	target wire.OutPoint
	proof  *recovery.Proof
	err    error
}

// EnsureProofForHarness satisfies the daemon's harnessProofAssembler
// capability — the only entry point the lineage accessor uses.
func (f *fakeProofAssembler) EnsureProofForHarness(_ context.Context,
	target wire.OutPoint) (*recovery.Proof, error) {

	if f.err != nil {
		return nil, f.err
	}
	if target != f.target {
		return nil, errUnknownTarget
	}

	return f.proof, nil
}

// errUnknownTarget is returned by fakeProofAssembler when the caller
// asks for a target that wasn't configured.
var errUnknownTarget = &assemblerError{msg: "unknown target"}

type assemblerError struct{ msg string }

func (e *assemblerError) Error() string { return e.msg }

// linkedTx builds a minimal MsgTx with one input per parent and one
// output at index 0. Pass an off-graph outpoint (e.g. a synthetic
// "batch" hash) to represent an external input — the recovery proof
// builder ignores inputs whose parent txid is not part of the proof.
func linkedTx(parents ...wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(3)
	for _, p := range parents {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: p})
	}
	tx.AddTxOut(&wire.TxOut{Value: 1_000})

	return tx
}

// outpointOf returns the (txid, 0) outpoint for tx.
func outpointOf(t *testing.T, tx *wire.MsgTx) wire.OutPoint {
	t.Helper()

	h := tx.TxHash()

	return wire.OutPoint{Hash: h, Index: 0}
}

// lineageVisit captures one entry observed during a recursive walk
// of GetVTXOLineageTx. The walker records the queried outpoint, the
// kind of node returned, the txid of the returned tx (zero when the
// walker hit OnChainRoot), and whether the entry was an on-chain
// terminator.
type lineageVisit struct {
	Query       wire.OutPoint
	TxHash      chainhash.Hash
	Kind        recovery.NodeKind
	OnChainRoot bool
}

// walkLineage performs a depth-first recursive walk of the recovery
// lineage rooted at vtxo by repeatedly calling GetVTXOLineageTx —
// exactly as a fraud-itest harness will. Visits are appended in DFS
// order: each parent of a returned tx is fully walked (left to right)
// before its sibling.
func walkLineage(t *testing.T, s *Server, vtxo wire.OutPoint) []lineageVisit {
	t.Helper()

	var visits []lineageVisit

	var visit func(query wire.OutPoint)
	visit = func(query wire.OutPoint) {
		entry, err := s.GetVTXOLineageTx(
			context.Background(), vtxo, query,
		)
		require.NoError(t, err)
		require.Equal(t, query, entry.Outpoint)

		v := lineageVisit{
			Query:       query,
			OnChainRoot: entry.OnChainRoot,
		}
		if entry.OnChainRoot {
			require.Nil(t, entry.Tx)
			require.Empty(t, entry.ParentOutpoints)
			visits = append(visits, v)

			return
		}

		require.NotNil(t, entry.Tx)
		v.TxHash = entry.Tx.TxHash()
		v.Kind = entry.Kind
		visits = append(visits, v)

		for _, p := range entry.ParentOutpoints {
			visit(p)
		}
	}
	visit(vtxo)

	return visits
}

// TestGetVTXOLineageTxRoundBornVTXO walks the recovery lineage of a
// round-born VTXO. The DAG is a pure tree-node chain from the leaf
// that creates the VTXO output up to the on-chain batch root: a real
// Ark round has no checkpoint or ark-tx layer for VTXOs minted by
// the round itself.
//
//	[batchTx] (on-chain, off-graph)
//	    │
//	    ▼
//	treeBranch (Tree)
//	    │
//	    ▼
//	treeLeaf  (Tree, creates the VTXO)
//	    │
//	    ▼
//	  (VTXO)
//
// The walker should visit: vtxoOut → treeBranchOut → batchOut(root)
// and terminate with OnChainRoot=true on the batch outpoint.
func TestGetVTXOLineageTxRoundBornVTXO(t *testing.T) {
	t.Parallel()

	// Off-graph batch outpoint — its txid does not appear in the
	// proof, so the walker terminates here with OnChainRoot.
	batchOut := wire.OutPoint{Hash: chainhash.Hash{0xba, 0x01}}

	branchTx := linkedTx(batchOut)
	branchOut := outpointOf(t, branchTx)

	leafTx := linkedTx(branchOut)
	vtxoOut := outpointOf(t, leafTx)

	proof, err := recovery.NewProof(
		vtxoOut, 10, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   branchTx,
		}, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   leafTx,
		},
	)
	require.NoError(t, err)

	s := &Server{
		proofAssembler: &fakeProofAssembler{
			target: vtxoOut, proof: proof,
		},
	}

	require.Equal(t, []lineageVisit{
		{
			Query:  vtxoOut,
			TxHash: leafTx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:  branchOut,
			TxHash: branchTx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:       batchOut,
			OnChainRoot: true,
		},
	}, walkLineage(t, s, vtxoOut))
}

// TestGetVTXOLineageTxOORBornSingleBatch walks the recovery lineage
// of an OOR-born VTXO whose single parent input traces back to one
// batch. This is the canonical OOR shape:
//
//	[batchTx] (on-chain, off-graph)
//	    │
//	    ▼
//	treeLeaf   (Tree, creates the sender's VTXO)
//	    │
//	    ▼
//	checkpoint (Checkpoint, spends the sender's VTXO)
//	    │
//	    ▼
//	arkTx      (Ark, spends checkpoint, creates the new VTXO)
//	    │
//	    ▼
//	  (VTXO)
//
// The walker should produce one Ark, one Checkpoint, one Tree, then
// the on-chain terminator.
func TestGetVTXOLineageTxOORBornSingleBatch(t *testing.T) {
	t.Parallel()

	batchOut := wire.OutPoint{Hash: chainhash.Hash{0xba, 0x02}}

	leafTx := linkedTx(batchOut)
	leafOut := outpointOf(t, leafTx)

	checkpointTx := linkedTx(leafOut)
	checkpointOut := outpointOf(t, checkpointTx)

	arkTx := linkedTx(checkpointOut)
	vtxoOut := outpointOf(t, arkTx)

	proof, err := recovery.NewProof(
		vtxoOut, 10,
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: leafTx},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint, Tx: checkpointTx,
		},
		&recovery.Node{Kind: recovery.NodeKindArk, Tx: arkTx},
	)
	require.NoError(t, err)

	s := &Server{
		proofAssembler: &fakeProofAssembler{
			target: vtxoOut, proof: proof,
		},
	}

	require.Equal(t, []lineageVisit{
		{
			Query:  vtxoOut,
			TxHash: arkTx.TxHash(),
			Kind:   recovery.NodeKindArk,
		},
		{
			Query:  checkpointOut,
			TxHash: checkpointTx.TxHash(),
			Kind:   recovery.NodeKindCheckpoint,
		},
		{
			Query:  leafOut,
			TxHash: leafTx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:       batchOut,
			OnChainRoot: true,
		},
	}, walkLineage(t, s, vtxoOut))
}

// TestGetVTXOLineageTxOORMultiInputMixedDepths walks the recovery
// lineage of a multi-input OOR VTXO whose two parent inputs trace
// back to different batches at different depths. The "left" parent
// is a round-born VTXO sitting two tree-tx layers below its batch;
// the "right" parent is itself an OOR-born VTXO, so its lineage
// includes an extra ark-tx + checkpoint pair before reaching the
// tree leaf and batch.
//
//	[batchA] (on-chain)            [batchB] (on-chain)
//	    │                              │
//	    ▼                              ▼
//	treeBranchA (Tree)             treeLeafB  (Tree, creates an
//	    │                                      intermediate VTXO)
//	    ▼                              │
//	treeLeafA   (Tree, creates a       ▼
//	             round-born parent  checkpointB1 (Checkpoint)
//	             VTXO)                 │
//	    │                              ▼
//	    ▼                          arkTxB1     (Ark, creates an
//	checkpointA (Checkpoint)                    OOR-born parent VTXO)
//	    │                              │
//	    │                              ▼
//	    │                          checkpointB2 (Checkpoint)
//	    │                              │
//	    └──────────────┬───────────────┘
//	                   ▼
//	                 arkTx          (Ark, multi-input;
//	                                 creates the final VTXO)
//	                   │
//	                   ▼
//	                 (VTXO)
//
// The walk forks at arkTx into two branches of unequal depth, each
// terminating at its own off-graph batch outpoint. Branch A (left)
// is depth 4; branch B (right) is depth 5. DFS visits branch A
// fully before branch B because that is the input order on arkTx.
func TestGetVTXOLineageTxOORMultiInputMixedDepths(t *testing.T) {
	t.Parallel()

	// Distinct off-graph batches — both must surface as OnChainRoot
	// terminators in the walk, proving the harness can fan out from
	// a multi-input ark tx.
	batchAOut := wire.OutPoint{Hash: chainhash.Hash{0xba, 0xa1}}
	batchBOut := wire.OutPoint{Hash: chainhash.Hash{0xba, 0xb2}}

	// Branch A: round-born parent VTXO. Two tree-tx layers from the
	// batch down to the leaf, then one checkpoint.
	branchATx := linkedTx(batchAOut)
	branchAOut := outpointOf(t, branchATx)

	leafATx := linkedTx(branchAOut)
	leafAOut := outpointOf(t, leafATx)

	checkpointATx := linkedTx(leafAOut)
	checkpointAOut := outpointOf(t, checkpointATx)

	// Branch B: OOR-born parent VTXO. A single tree leaf creates an
	// intermediate VTXO, an upstream OOR (checkpointB1 + arkTxB1)
	// re-spends it, and checkpointB2 anchors that OOR-born parent
	// for the final ark tx.
	leafBTx := linkedTx(batchBOut)
	leafBOut := outpointOf(t, leafBTx)

	checkpointB1Tx := linkedTx(leafBOut)
	checkpointB1Out := outpointOf(t, checkpointB1Tx)

	arkB1Tx := linkedTx(checkpointB1Out)
	arkB1Out := outpointOf(t, arkB1Tx)

	checkpointB2Tx := linkedTx(arkB1Out)
	checkpointB2Out := outpointOf(t, checkpointB2Tx)

	// Final ark tx: two inputs, A first then B. Output index 0 is
	// the recipient's VTXO.
	arkTx := linkedTx(checkpointAOut, checkpointB2Out)
	vtxoOut := outpointOf(t, arkTx)

	proof, err := recovery.NewProof(
		vtxoOut, 10,
		// Branch A nodes.
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: branchATx},
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: leafATx},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint, Tx: checkpointATx,
		},
		// Branch B nodes.
		&recovery.Node{Kind: recovery.NodeKindTree, Tx: leafBTx},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint, Tx: checkpointB1Tx,
		},
		&recovery.Node{Kind: recovery.NodeKindArk, Tx: arkB1Tx},
		&recovery.Node{
			Kind: recovery.NodeKindCheckpoint, Tx: checkpointB2Tx,
		},
		// Joining node.
		&recovery.Node{Kind: recovery.NodeKindArk, Tx: arkTx},
	)
	require.NoError(t, err)

	s := &Server{
		proofAssembler: &fakeProofAssembler{
			target: vtxoOut, proof: proof,
		},
	}

	require.Equal(t, []lineageVisit{
		// Fork.
		{
			Query:  vtxoOut,
			TxHash: arkTx.TxHash(),
			Kind:   recovery.NodeKindArk,
		},
		// Branch A: ckpt → leaf → branch → batchA.
		{
			Query:  checkpointAOut,
			TxHash: checkpointATx.TxHash(),
			Kind:   recovery.NodeKindCheckpoint,
		},
		{
			Query:  leafAOut,
			TxHash: leafATx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:  branchAOut,
			TxHash: branchATx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:       batchAOut,
			OnChainRoot: true,
		},
		// Branch B: ckpt2 → arkB1 → ckpt1 → leafB → batchB.
		{
			Query:  checkpointB2Out,
			TxHash: checkpointB2Tx.TxHash(),
			Kind:   recovery.NodeKindCheckpoint,
		},
		{
			Query:  arkB1Out,
			TxHash: arkB1Tx.TxHash(),
			Kind:   recovery.NodeKindArk,
		},
		{
			Query:  checkpointB1Out,
			TxHash: checkpointB1Tx.TxHash(),
			Kind:   recovery.NodeKindCheckpoint,
		},
		{
			Query:  leafBOut,
			TxHash: leafBTx.TxHash(),
			Kind:   recovery.NodeKindTree,
		},
		{
			Query:       batchBOut,
			OnChainRoot: true,
		},
	}, walkLineage(t, s, vtxoOut))
}

// TestGetVTXOLineageTxNoAssembler verifies the daemon surfaces a
// clear error when the proof assembler hasn't been wired (e.g. the
// unroll subsystem hasn't started yet) rather than returning a nil
// pointer.
func TestGetVTXOLineageTxNoAssembler(t *testing.T) {
	t.Parallel()

	s := &Server{}

	_, err := s.GetVTXOLineageTx(
		context.Background(), wire.OutPoint{
			Hash: chainhash.Hash{0x01},
		}, wire.OutPoint{
			Hash: chainhash.Hash{0x01},
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "proof assembler")
}

// TestGetVTXOLineageTxAssemblerErrorPropagates verifies a build
// failure in the assembler surfaces upward with context, so callers
// can distinguish "no record" (success with nil) from "lookup failed"
// (error with reason).
func TestGetVTXOLineageTxAssemblerErrorPropagates(t *testing.T) {
	t.Parallel()

	target := wire.OutPoint{Hash: chainhash.Hash{0xab}}

	s := &Server{
		proofAssembler: &fakeProofAssembler{
			target: target,
			err: &assemblerError{
				msg: "lineage missing",
			},
		},
	}

	_, err := s.GetVTXOLineageTx(
		context.Background(), target, target,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "lineage missing")
}
