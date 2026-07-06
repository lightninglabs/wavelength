package arkrpc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// makeTestTree returns a small two-level tree: one root with one child leaf.
func makeTestTree(t *testing.T) *tree.Tree {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("priv key: %v", err)
	}

	leaf := &tree.Node{
		Input: wire.OutPoint{
			Hash: chainhash.Hash{
				0xaa,
			},
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			priv.PubKey(),
		},
		Children: map[uint32]*tree.Node{},
		Amount:   btcutil.Amount(1_000),
	}

	root := &tree.Node{
		Input: wire.OutPoint{
			Hash: chainhash.Hash{
				0xbb,
			},
			Index: 1,
		},
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
				},
			},
		},
		CoSigners: []*btcec.PublicKey{
			priv.PubKey(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
		Amount: btcutil.Amount(1_000),
	}

	return &tree.Tree{
		Root: root,
		BatchOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0xcc,
			},
			Index: 2,
		},
		BatchOutput: &wire.TxOut{
			Value: 1_000, PkScript: []byte{
				0x51,
			},
		},
		SweepTapscriptRoot: bytes.Repeat([]byte{0x01}, 32),
	}
}

// TestAncestryPathRoundTrip ensures the AncestryPath encoder and decoder
// are inverses for a well-formed tree, and that all carried metadata
// (commitment txid, input indices, tree depth) survive the round-trip.
func TestAncestryPathRoundTrip(t *testing.T) {
	t.Parallel()

	original := makeTestTree(t)
	commitmentTxID := chainhash.Hash{0xde, 0xad, 0xbe, 0xef}
	indices := []uint32{0, 3}

	p, err := AncestryPathFromTree(original, commitmentTxID, indices)
	if err != nil {
		t.Fatalf("AncestryPathFromTree: %v", err)
	}

	if !bytes.Equal(p.CommitmentTxid, commitmentTxID[:]) {
		t.Fatalf("commitment txid round-trip mismatch")
	}

	if len(p.InputIndices) != len(indices) {
		t.Fatalf("input_indices length mismatch: got %d want %d",
			len(p.InputIndices), len(indices))
	}
	for i := range indices {
		if p.InputIndices[i] != indices[i] {
			t.Fatalf("input_indices[%d] mismatch: got %d want %d",
				i, p.InputIndices[i], indices[i])
		}
	}

	if p.TreeDepth != 2 {
		t.Fatalf("tree_depth: got %d want 2", p.TreeDepth)
	}

	got, err := AncestryPathToTree(p)
	if err != nil {
		t.Fatalf("AncestryPathToTree: %v", err)
	}

	if got == nil {
		t.Fatalf("decoded tree is nil")
	}

	if got.BatchOutpoint != original.BatchOutpoint {
		t.Fatalf("batch outpoint round-trip mismatch")
	}
}

// TestAncestryCommitmentTxIDRejectsBadLength ensures malformed commitment
// txid byte slices are rejected, not silently truncated.
func TestAncestryCommitmentTxIDRejectsBadLength(t *testing.T) {
	t.Parallel()

	p := &AncestryPath{CommitmentTxid: []byte{0x01, 0x02}}
	if _, err := AncestryCommitmentTxID(p); err == nil {
		t.Fatalf("expected error for short commitment_txid")
	}
}

// TestAncestryPathFromTreeRejectsNil guards against silently returning a
// zero AncestryPath when the caller misroutes a nil tree.
func TestAncestryPathFromTreeRejectsNil(t *testing.T) {
	t.Parallel()

	if _, err := AncestryPathFromTree(
		nil, chainhash.Hash{}, nil,
	); err == nil {

		t.Fatalf("expected error for nil ancestry tree")
	}
}

// makeChainTree builds a single-child-per-level tree of the requested
// depth (depth=1 yields one node). Used to cover the depth-walk bound.
func makeChainTree(depth int) *tree.Tree {
	if depth < 1 {
		depth = 1
	}

	leaf := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
		Children: map[uint32]*tree.Node{},
	}

	cur := leaf
	for i := 1; i < depth; i++ {
		parent := &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{},
				Index: uint32(i),
			},
			Children: map[uint32]*tree.Node{
				0: cur,
			},
		}
		cur = parent
	}

	return &tree.Tree{
		Root: cur,
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	}
}

// TestNodeMaxDepthAcceptsAtCap ensures a chain whose depth equals the
// walk-cap is accepted; this guards against a tight bound rejecting a
// legitimate worst-case tree.
func TestNodeMaxDepthAcceptsAtCap(t *testing.T) {
	t.Parallel()

	d, err := treeMaxDepth(makeChainTree(MaxAncestryTreeWalkDepth))
	if err != nil {
		t.Fatalf("treeMaxDepth: %v", err)
	}

	if d != MaxAncestryTreeWalkDepth {
		t.Fatalf("depth: got %d want %d", d, MaxAncestryTreeWalkDepth)
	}
}

// TestNodeMaxDepthRejectsOverCap ensures a chain one level past the
// cap is rejected, so an indexer-supplied tree cannot blow the
// goroutine stack.
func TestNodeMaxDepthRejectsOverCap(t *testing.T) {
	t.Parallel()

	_, err := treeMaxDepth(makeChainTree(MaxAncestryTreeWalkDepth + 1))
	if err == nil {
		t.Fatalf("expected error for over-cap tree depth")
	}
}

// TestValidateAncestryPathDepth exercises the indexer→client tree_depth
// guard introduced for darepo-client#370. Each case represents an attack
// or edge condition: zero claim, over-cap claim, mismatched claim,
// at-cap claim, and a valid leaf claim. The validator is the trust
// boundary that prevents an untrusted indexer from stranding an OOR
// VTXO via tree_depth, so coverage here is load-bearing.
func TestValidateAncestryPathDepth(t *testing.T) {
	t.Parallel()

	leafTree := makeChainTree(1)
	pairTree := makeChainTree(2)
	atCapTree := makeChainTree(MaxAncestryTreeWalkDepth)

	cases := []struct {
		name    string
		claimed uint32
		tree    *tree.Tree
		wantErr string
	}{
		{
			name:    "zero claim is rejected",
			claimed: 0,
			tree:    leafTree,
			wantErr: "must be non-zero",
		},
		{
			name:    "over-cap claim is rejected",
			claimed: MaxAncestryTreeWalkDepth + 1,
			tree:    nil,
			wantErr: "exceeds max",
		},
		{
			name:    "claim disagrees with reconstructed",
			claimed: 5,
			tree:    pairTree,
			wantErr: "does not match reconstructed",
		},
		{
			name:    "valid leaf claim",
			claimed: 1,
			tree:    leafTree,
		},
		{
			name:    "at-cap claim",
			claimed: MaxAncestryTreeWalkDepth,
			tree:    atCapTree,
		},
		{
			// Range-only check when the tree is absent. Callers
			// requiring usable ancestry enforce non-nil elsewhere.
			name:    "nil tree skips path comparison",
			claimed: 3,
			tree:    nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAncestryPathDepth(tc.claimed, tc.tree)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q",
					tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err %q does not contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateAncestryPathDepthBoundsReconstructedWalk ensures the
// validator does not invoke the unbounded tree.Tree.Depth() on an
// indexer-supplied reconstructed tree. A linear chain of nodes that
// would otherwise recurse deeply must be rejected via the local
// bounded walker (treeMaxDepth) without overflowing the stack. The
// claimed scalar is in-range so the early range check passes and we
// reach the actual-depth comparison; only the bounded walker
// terminates this case safely.
func TestValidateAncestryPathDepthBoundsReconstructedWalk(t *testing.T) {
	t.Parallel()

	// Build a chain deeper than the walk cap. A hostile indexer can
	// produce this from a valid-looking proto TreePath (children
	// must have a strictly higher index, but the overall chain
	// length is not capped at the proto layer).
	overCap := makeChainTree(MaxAncestryTreeWalkDepth + 10)

	err := ValidateAncestryPathDepth(MaxAncestryTreeWalkDepth, overCap)
	if err == nil {
		t.Fatalf("expected error for over-cap reconstructed tree")
	}

	if !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("err %q does not mention bounded walker", err.Error())
	}
}
