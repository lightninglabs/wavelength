package arkrpc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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
