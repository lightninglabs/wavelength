package indexer

import (
	"sort"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// makeOutpoint constructs a deterministic outpoint from a label so each
// test fixture can identify its parents in failure messages. The label
// is used purely to derive a stable hash; outpoint contents do not affect
// the combineVirtualLineage logic itself.
func makeOutpoint(label string, idx uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash:  chainhash.HashH([]byte(label)),
		Index: idx,
	}
}

// makeCommitmentHash returns a stable chainhash for a commitment label.
func makeCommitmentHash(label string) chainhash.Hash {
	return chainhash.HashH([]byte("commitment-" + label))
}

// virtualParentBuilder constructs a synthetic virtual VTXO parent for
// combineVirtualLineage tests. The returned VTXORow has nil RoundID so
// tryResolveCombinedRoundPath bails out cleanly, exercising the rep-
// fragment path. The returned vtxoLineage carries one ancestry fragment
// with non-nil treePath and treePathTLV so the missing-tree-path hard
// error is not triggered. Every fragment has treeDepth >= 1 so the
// rep-picker (`candidate.treeDepth > rep.treeDepth`) sees at least one
// parent as strictly deeper than the zero-value rep.
type virtualParentBuilder struct {
	label          string
	commitmentHash chainhash.Hash
	roundID        string
	batchExpiry    int32
	relativeExpiry uint32
	chainDepth     int
	createdHeight  int32
	treeDepth      int
}

// build returns the (outpoint, row, lineage) triple a test passes into
// combineVirtualLineage. The fragment carries non-nil treePath +
// treePathTLV derived from the label so the rep-picker (which now
// requires a non-nil tree path post-H-4) resolves cleanly.
func (b virtualParentBuilder) build(idx uint32) (wire.OutPoint, VTXORow,
	*vtxoLineage) {

	op := makeOutpoint(b.label, idx)
	row := VTXORow{Outpoint: op}

	depth := b.treeDepth
	if depth == 0 {
		depth = 1
	}

	// Synthesize a minimal *tree.Tree so the rep-picker accepts the
	// fragment. The internal tests do not exercise the downstream
	// arkrpc.AncestryPathFromTree conversion so the tree shape itself
	// does not matter — only its non-nil presence does.
	minTree := &tree.Tree{Root: &tree.Node{}}

	lineage := &vtxoLineage{
		roundID:        b.roundID,
		commitmentTxID: b.commitmentHash,
		batchExpiry:    b.batchExpiry,
		relativeExpiry: b.relativeExpiry,
		chainDepth:     b.chainDepth,
		createdHeight:  b.createdHeight,
		ancestryPaths: []ancestryFragment{{
			treePath:       minTree,
			treePathTLV:    []byte("tlv-" + b.label),
			commitmentTxID: b.commitmentHash,
			treeDepth:      depth,
		}},
	}

	return op, row, lineage
}

// callCombine collects the (rows, outpoints, lineages) tuple a series of
// virtualParentBuilders produces, then invokes combineVirtualLineage on
// a fresh resolver. The child outpoint is fixed because combineVirtualLineage
// does not consult it (it is forwarded only to a future caller).
func callCombine(t *testing.T,
	parents []virtualParentBuilder) (*vtxoLineage, error) {

	t.Helper()

	rows := make([]VTXORow, 0, len(parents))
	ops := make([]wire.OutPoint, 0, len(parents))
	lineages := make([]*vtxoLineage, 0, len(parents))
	for i, p := range parents {
		op, row, l := p.build(uint32(i))
		rows = append(rows, row)
		ops = append(ops, op)
		lineages = append(lineages, l)
	}

	r := &lineageResolver{}

	return r.combineVirtualLineage(
		t.Context(), makeOutpoint("child", 0), rows, ops, lineages,
	)
}

// TestCombineVirtualLineageAllowsInheritedMissingTreePath verifies that
// the resolver hard-errors when the only parent fragment has neither a
// tree path nor TLV bytes. The previous mixedSingularLineage graceful-
// degrade branch silently dropped the path; the current resolver
// surfaces the break loudly.
func TestCombineVirtualLineageAllowsInheritedMissingTreePath(t *testing.T) {
	t.Parallel()

	resolver := &lineageResolver{}
	parentOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("parent")),
		Index: 1,
	}
	childOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("child")),
		Index: 0,
	}
	parentCommitment := chainhash.HashH([]byte("commitment"))
	parentLineage := &vtxoLineage{
		roundID:        "round-test",
		commitmentTxID: parentCommitment,
		batchExpiry:    144,
		relativeExpiry: 12,
		chainDepth:     1,
		createdHeight:  99,
	}

	_, err := resolver.combineVirtualLineage(
		t.Context(),
		childOutpoint,
		[]VTXORow{{
			Outpoint: parentOutpoint,
		}},
		[]wire.OutPoint{parentOutpoint},
		[]*vtxoLineage{parentLineage},
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "missing inherited tree path",
	)
}

// TestCombineVirtualLineageEmptyParentsRejected verifies the explicit
// guard at the top of combineVirtualLineage. An OOR session with zero
// resolved parents is a programming error: every Ark tx must consume at
// least one input.
func TestCombineVirtualLineageEmptyParentsRejected(t *testing.T) {
	t.Parallel()

	resolver := &lineageResolver{}
	_, err := resolver.combineVirtualLineage(
		t.Context(), makeOutpoint("child", 0), nil, nil, nil,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing parent lineage")
}

// TestCombineVirtualLineageSingleVirtualParent verifies the trivial
// single-parent case. The combined lineage carries exactly one
// ancestry fragment populated with the parent's commitment, the parent's
// chain depth + 1, and the parent's restrictive metadata.
func TestCombineVirtualLineageSingleVirtualParent(t *testing.T) {
	t.Parallel()

	parents := []virtualParentBuilder{{
		label:          "p0",
		commitmentHash: makeCommitmentHash("A"),
		roundID:        "round-A",
		batchExpiry:    200,
		relativeExpiry: 5,
		chainDepth:     2,
		createdHeight:  10,
		treeDepth:      3,
	}}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 1)
	require.Equal(
		t, makeCommitmentHash("A"),
		combined.ancestryPaths[0].commitmentTxID,
	)
	require.Equal(t, []uint32{0},
		combined.ancestryPaths[0].inputIndices)
	require.Equal(t, 3, combined.ancestryPaths[0].treeDepth)
	require.Equal(
		t, 3, combined.chainDepth,
		"chainDepth = max(parent.chainDepth)+1",
	)
	require.Equal(t, "round-A", combined.roundID)
	require.Equal(t, int32(200), combined.batchExpiry)
}

// TestCombineVirtualLineageSingleMultiRootParent verifies that a later OOR
// hop preserves every inherited root from a parent that was itself created by
// a cross-round multi-input transfer. The current Ark tx has only one input,
// so that input index is intentionally attached to both fragments.
func TestCombineVirtualLineageSingleMultiRootParent(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")

	parent := virtualParentBuilder{
		label: "p0", commitmentHash: hashA,
		batchExpiry: 100, treeDepth: 3, chainDepth: 1,
	}
	op, row, lineage := parent.build(0)
	lineage.ancestryPaths = append(
		lineage.ancestryPaths,
		ancestryFragment{
			treePath:       &tree.Tree{Root: &tree.Node{}},
			treePathTLV:    []byte("tlv-p0-b"),
			commitmentTxID: hashB,
			treeDepth:      2,
		},
	)

	resolver := &lineageResolver{}
	combined, err := resolver.combineVirtualLineage(
		t.Context(), makeOutpoint("child", 0), []VTXORow{row},
		[]wire.OutPoint{op}, []*vtxoLineage{lineage},
	)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 2)

	require.Equal(t, hashA, combined.ancestryPaths[0].commitmentTxID)
	require.Equal(t, []uint32{0},
		combined.ancestryPaths[0].inputIndices)

	require.Equal(t, hashB, combined.ancestryPaths[1].commitmentTxID)
	require.Equal(t, []uint32{0},
		combined.ancestryPaths[1].inputIndices)

	require.Equal(t, 2, combined.chainDepth)
}

// TestCombineVirtualLineageSameCommitmentVirtualParents verifies that
// two virtual parents sharing the same commitment_txid collapse to a
// single ancestry fragment via the rep-path (tryResolveCombinedRoundPath
// returns nil because the parents are not round-direct). Both input
// indices land in the one fragment.
func TestCombineVirtualLineageSameCommitmentVirtualParents(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			batchExpiry: 100, treeDepth: 4, chainDepth: 1,
		},
		{
			label: "p1", commitmentHash: hashA,
			batchExpiry: 110, treeDepth: 2, chainDepth: 1,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 1)
	require.Equal(t, hashA, combined.ancestryPaths[0].commitmentTxID)
	require.Equal(
		t, []uint32{0, 1}, combined.ancestryPaths[0].inputIndices,
		"input indices must include every parent in this group",
	)

	// rep-path picks the deepest parent's first fragment, so treeDepth
	// = 4 (from p0).
	require.Equal(t, 4, combined.ancestryPaths[0].treeDepth)

	// Most-restrictive parent is p0 (smaller batchExpiry).
	require.Equal(t, int32(100), combined.batchExpiry)
}

// TestCombineVirtualLineageDifferentCommitments is the core multi-tree
// case. Two parents from distinct commitments produce two ancestry
// fragments, each carrying exactly the input index whose parent
// contributed it. Fragment ordering matches first-appearance order.
func TestCombineVirtualLineageDifferentCommitments(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			batchExpiry: 200, treeDepth: 2,
		},
		{
			label: "p1", commitmentHash: hashB,
			batchExpiry: 150, treeDepth: 3,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 2)

	// First fragment carries commitmentA (first parent's commitment),
	// input index 0.
	require.Equal(t, hashA, combined.ancestryPaths[0].commitmentTxID)
	require.Equal(t, []uint32{0},
		combined.ancestryPaths[0].inputIndices)

	// Second fragment carries commitmentB, input index 1.
	require.Equal(t, hashB, combined.ancestryPaths[1].commitmentTxID)
	require.Equal(t, []uint32{1},
		combined.ancestryPaths[1].inputIndices)

	// p1 is more restrictive (smaller batchExpiry); the combined
	// lineage's scalar metadata inherits from p1.
	require.Equal(t, int32(150), combined.batchExpiry)
}

// TestCombineVirtualLineageMixedCommitments verifies the canonical
// three-input cross-round shape: two parents share commitmentA, one
// parent stands alone on commitmentB. The resolver must produce two
// fragments, group {0, 1} into the A-fragment, and place {2} into the
// B-fragment, in first-appearance order.
func TestCombineVirtualLineageMixedCommitments(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			batchExpiry: 300, treeDepth: 3,
		},
		{
			label: "p1", commitmentHash: hashB,
			batchExpiry: 250, treeDepth: 2,
		},
		{
			label: "p2", commitmentHash: hashA,
			batchExpiry: 280, treeDepth: 5,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 2)

	// First-appearance order: A then B.
	require.Equal(t, hashA, combined.ancestryPaths[0].commitmentTxID)
	require.Equal(t, hashB, combined.ancestryPaths[1].commitmentTxID)

	// A-fragment groups input indices 0 and 2 (in order they were
	// appended).
	require.Equal(t, []uint32{0, 2},
		combined.ancestryPaths[0].inputIndices)

	// B-fragment groups input index 1.
	require.Equal(t, []uint32{1},
		combined.ancestryPaths[1].inputIndices)

	// rep-path picks deepest parent in group A (p2 has treeDepth=5).
	require.Equal(t, 5, combined.ancestryPaths[0].treeDepth)
	require.Equal(t, 2, combined.ancestryPaths[1].treeDepth)

	// p1 is the most restrictive (smallest non-zero batchExpiry); the
	// combined lineage inherits its scalar metadata.
	require.Equal(t, int32(250), combined.batchExpiry)
}

// TestCombineVirtualLineageChainDepthIsMaxPlusOne verifies the
// chain-depth invariant under heterogeneous parent depths. The combined
// VTXO is one OOR hop deeper than its deepest parent.
func TestCombineVirtualLineageChainDepthIsMaxPlusOne(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			chainDepth: 1, treeDepth: 1,
		},
		{
			label: "p1", commitmentHash: hashA,
			chainDepth: 7, treeDepth: 1,
		},
		{
			label: "p2", commitmentHash: hashA,
			chainDepth: 4, treeDepth: 1,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Equal(
		t, 8, combined.chainDepth, "chainDepth = max(1, 7, 4) + 1 = 8",
	)
}

// TestCombineVirtualLineageMostRestrictiveBatchExpiry verifies that the
// scalar batchExpiry on the combined lineage is the minimum non-zero
// expiry across all parents, mirroring moreRestrictiveLineage's
// definition. Parents with zero batchExpiry never win the comparison.
func TestCombineVirtualLineageMostRestrictiveBatchExpiry(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			batchExpiry: 0, treeDepth: 1, // unknown
		},
		{
			label: "p1", commitmentHash: hashA,
			batchExpiry: 500, treeDepth: 1,
		},
		{
			label: "p2", commitmentHash: hashB,
			batchExpiry: 200, treeDepth: 1,
		},
		{
			label: "p3", commitmentHash: hashB,
			batchExpiry: 350, treeDepth: 1,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Equal(
		t, int32(200), combined.batchExpiry,
		"min non-zero batchExpiry across all parents wins",
	)
}

// TestCombineVirtualLineageDefensiveCopy verifies that mutating the
// returned ancestry slice does not perturb the parent lineages. This
// matches cloneLineage's documented contract — fragments are
// deep-copied, scalar fields shared.
func TestCombineVirtualLineageDefensiveCopy(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	parents := []virtualParentBuilder{
		{
			label: "p0", commitmentHash: hashA,
			batchExpiry: 100, treeDepth: 2,
		},
		{
			label: "p1", commitmentHash: hashA,
			batchExpiry: 200, treeDepth: 2,
		},
	}

	rows := make([]VTXORow, 0, len(parents))
	ops := make([]wire.OutPoint, 0, len(parents))
	lineages := make([]*vtxoLineage, 0, len(parents))
	for i, p := range parents {
		op, row, l := p.build(uint32(i))
		rows = append(rows, row)
		ops = append(ops, op)
		lineages = append(lineages, l)
	}
	originalIndices := append(
		[]uint32(nil), lineages[0].ancestryPaths[0].inputIndices...,
	)
	originalTLV := append(
		[]byte(nil), lineages[0].ancestryPaths[0].treePathTLV...,
	)

	r := &lineageResolver{}
	combined, err := r.combineVirtualLineage(
		t.Context(), makeOutpoint("child", 0), rows, ops, lineages,
	)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 1)

	// Mutate the combined lineage's ancestry slice.
	combined.ancestryPaths[0].inputIndices[0] = 99
	if len(combined.ancestryPaths[0].treePathTLV) > 0 {
		combined.ancestryPaths[0].treePathTLV[0] = 0xFF
	}

	// Parent lineage's primary fragment must be unchanged.
	require.Equal(
		t, originalIndices, lineages[0].ancestryPaths[0].inputIndices,
		"parent inputIndices must not alias combined output",
	)
	require.Equal(
		t, originalTLV, lineages[0].ancestryPaths[0].treePathTLV,
		"parent treePathTLV must not alias combined output",
	)
}

// TestCombineVirtualLineageFragmentsCoverAllInputs is a structural
// invariant check: the union of every fragment's inputIndices is
// exactly [0, len(parents)) with no duplicates. This is the
// fundamental partition property the unroller depends on to route each
// Ark input to its correct broadcast tree.
func TestCombineVirtualLineageFragmentsCoverAllInputs(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	hashC := makeCommitmentHash("C")
	parents := []virtualParentBuilder{
		{
			label:          "p0",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p1",
			commitmentHash: hashB,
			treeDepth:      1,
		},
		{
			label:          "p2",
			commitmentHash: hashC,
			treeDepth:      1,
		},
		{
			label:          "p3",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p4",
			commitmentHash: hashB,
			treeDepth:      1,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(
		t, combined.ancestryPaths, 3,
		"three distinct commitments -> three fragments",
	)

	// Collect every input index from every fragment.
	seen := make(map[uint32]int)
	for _, f := range combined.ancestryPaths {
		for _, idx := range f.inputIndices {
			seen[idx]++
		}
	}

	// Every input index from 0 to 4 must appear exactly once.
	require.Len(t, seen, len(parents))
	for i := uint32(0); i < uint32(len(parents)); i++ {
		require.Equal(
			t, 1, seen[i], "input index %d must appear in "+
				"exactly one fragment", i,
		)
	}
}

// TestSameSingularLineage walks the predicate's truth table.
func TestSameSingularLineage(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	base := &vtxoLineage{
		roundID:        "r1",
		commitmentTxID: hashA,
		batchExpiry:    100,
		createdHeight:  10,
		relativeExpiry: 5,
	}
	tests := []struct {
		name string
		a    *vtxoLineage
		b    *vtxoLineage
		want bool
	}{
		{
			name: "both nil", a: nil, b: nil, want: true,
		},
		{
			name: "first nil", a: nil, b: base, want: false,
		},
		{
			name: "second nil", a: base, b: nil, want: false,
		},
		{
			name: "identical", a: base,
			b: cloneLineage(base), want: true,
		},
		{
			name: "different round id",
			a:    base,
			b: &vtxoLineage{
				roundID:        "r2",
				commitmentTxID: hashA,
				batchExpiry:    100,
				createdHeight:  10,
				relativeExpiry: 5,
			},
			want: false,
		},
		{
			name: "different commitment",
			a:    base,
			b: &vtxoLineage{
				roundID:        "r1",
				commitmentTxID: hashB,
				batchExpiry:    100,
				createdHeight:  10,
				relativeExpiry: 5,
			},
			want: false,
		},
		{
			name: "different batch expiry",
			a:    base,
			b: &vtxoLineage{
				roundID:        "r1",
				commitmentTxID: hashA,
				batchExpiry:    200,
				createdHeight:  10,
				relativeExpiry: 5,
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sameSingularLineage(tc.a, tc.b)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestMoreRestrictiveLineageAxioms walks every branch of
// moreRestrictiveLineage. The predicate is the comparator that drives
// "earliest-expiring parent wins" semantics.
func TestMoreRestrictiveLineageAxioms(t *testing.T) {
	t.Parallel()

	known := func(expiry int32) *vtxoLineage {
		return &vtxoLineage{batchExpiry: expiry}
	}
	tests := []struct {
		name      string
		candidate *vtxoLineage
		current   *vtxoLineage
		want      bool
	}{
		{
			name: "candidate nil",
			want: false,
		},
		{
			name:      "current nil, candidate non-nil",
			candidate: known(100), want: true,
		},
		{
			name:      "candidate unknown, current known",
			candidate: known(0), current: known(100), want: false,
		},
		{
			name:      "candidate unknown, current unknown",
			candidate: known(0), current: known(0), want: false,
		},
		{
			name:      "candidate known, current unknown",
			candidate: known(100), current: known(0), want: true,
		},
		{
			name:      "candidate earlier than current",
			candidate: known(50), current: known(100), want: true,
		},
		{
			name:      "candidate later than current",
			candidate: known(200), current: known(100), want: false,
		},
		{
			name:      "candidate equal to current",
			candidate: known(100), current: known(100),
			want: false, // strict <, so equal does not win
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := moreRestrictiveLineage(
				tc.candidate, tc.current,
			)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestCloneLineageNilInput verifies the explicit nil short-circuit.
func TestCloneLineageNilInput(t *testing.T) {
	t.Parallel()
	require.Nil(t, cloneLineage(nil))
}

// TestCloneLineageDefensiveCopy verifies that cloneLineage produces a
// genuinely independent copy: per-fragment slices (treePathTLV,
// inputIndices) can be mutated on the clone without affecting the
// source. tree.Tree pointers are intentionally shared (documented
// in the function's doc-comment).
func TestCloneLineageDefensiveCopy(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	src := &vtxoLineage{
		roundID:        "round-test",
		commitmentTxID: hashA,
		batchExpiry:    400,
		relativeExpiry: 10,
		chainDepth:     2,
		createdHeight:  77,
		ancestryPaths: []ancestryFragment{
			{
				treePathTLV: []byte{
					1,
					2,
					3,
				},
				commitmentTxID: hashA,
				inputIndices: []uint32{
					0,
					1,
				},
				treeDepth: 3,
			},
			{
				treePathTLV: []byte{
					4,
					5,
					6,
				},
				commitmentTxID: hashB,
				inputIndices: []uint32{
					2,
				},
				treeDepth: 4,
			},
		},
	}

	dst := cloneLineage(src)
	require.NotSame(t, src, dst)
	require.Equal(t, src.roundID, dst.roundID)
	require.Equal(t, src.batchExpiry, dst.batchExpiry)
	require.Equal(t, len(src.ancestryPaths), len(dst.ancestryPaths))

	// Clone's per-fragment slices must not alias the source. Mutate
	// the clone and verify the source survives unchanged.
	dst.ancestryPaths[0].inputIndices[0] = 99
	dst.ancestryPaths[0].treePathTLV[0] = 0xFF
	require.Equal(t, []uint32{0, 1},
		src.ancestryPaths[0].inputIndices)
	require.Equal(t, []byte{1, 2, 3},
		src.ancestryPaths[0].treePathTLV)
}

// TestCloneLineageEmptyAncestryPaths verifies the early-return when
// ancestryPaths is nil. The clone's slice should also be nil (not an
// empty allocated slice) so byte-identical persistence still works.
func TestCloneLineageEmptyAncestryPaths(t *testing.T) {
	t.Parallel()

	src := &vtxoLineage{
		roundID:     "r1",
		batchExpiry: 100,
	}
	dst := cloneLineage(src)
	require.Nil(t, dst.ancestryPaths)
}

// TestCombineVirtualLineageStableOrdering verifies that fragment
// ordering is deterministic across repeated calls with identical
// inputs. (Map iteration is non-deterministic, but the resolver
// captures first-appearance order in groupOrder, so output is
// stable.) Run combineVirtualLineage 50 times and confirm the
// fragment commitment txid sequence is byte-identical every time.
func TestCombineVirtualLineageStableOrdering(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	hashB := makeCommitmentHash("B")
	hashC := makeCommitmentHash("C")
	parents := []virtualParentBuilder{
		{
			label:          "p0",
			commitmentHash: hashB,
			treeDepth:      1,
		},
		{
			label:          "p1",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p2",
			commitmentHash: hashC,
			treeDepth:      1,
		},
		{
			label:          "p3",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p4",
			commitmentHash: hashB,
			treeDepth:      1,
		},
	}

	want := []chainhash.Hash{hashB, hashA, hashC}

	for i := 0; i < 50; i++ {
		combined, err := callCombine(t, parents)
		require.NoError(t, err)
		require.Len(t, combined.ancestryPaths, 3)
		got := make([]chainhash.Hash, 0, 3)
		for _, f := range combined.ancestryPaths {
			got = append(got, f.commitmentTxID)
		}
		require.Equal(
			t, want, got,
			"fragment order must follow first-appearance order",
		)
	}
}

// TestCombineVirtualLineageInputIndicesSorted verifies that within a
// single fragment, inputIndices are appended in parent-iteration order
// (which equals strictly ascending order because we iterate
// parentLineages by index 0..N-1).
func TestCombineVirtualLineageInputIndicesSorted(t *testing.T) {
	t.Parallel()

	hashA := makeCommitmentHash("A")
	parents := []virtualParentBuilder{
		{
			label:          "p0",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p1",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p2",
			commitmentHash: hashA,
			treeDepth:      1,
		},
		{
			label:          "p3",
			commitmentHash: hashA,
			treeDepth:      1,
		},
	}

	combined, err := callCombine(t, parents)
	require.NoError(t, err)
	require.Len(t, combined.ancestryPaths, 1)

	indices := combined.ancestryPaths[0].inputIndices
	require.True(
		t, sort.SliceIsSorted(indices, func(i, j int) bool {
			return indices[i] < indices[j]
		}),
		"inputIndices must be ascending: %v",
		indices,
	)
	require.Equal(t, []uint32{0, 1, 2, 3}, indices)
}

// TestCombineVirtualLineageSameCommitmentDifferentBatchTrees verifies
// the M-5 fix: two round-direct parents that share a commitment_txid
// but live in different batch trees (distinct BatchOutputIndex values
// within the same round) must split into two AncestryPath entries
// rather than collapse into one. The legacy single-key grouping
// silently dropped one path; the (commitment, batch-tree) key keeps
// both paths so the recipient can publish each batch tree
// independently for unilateral exit.
func TestCombineVirtualLineageSameCommitmentDifferentBatchTrees(t *testing.T) {
	t.Parallel()

	commitment := makeCommitmentHash("shared-commitment")
	var roundID rounds.RoundID
	hash := chainhash.HashH([]byte("round-id"))
	copy(roundID[:], hash[:len(roundID)])

	// Two round-direct parents sharing a commitment, distinct batch
	// outputs. Synthesize the inherited fragment shape that
	// combineVirtualLineage would observe after a recursive resolve
	// on the parent rows: each fragment carries a real *tree.Tree
	// pointer and a treePathTLV, so the rep-picker downstream
	// accepts them without error.
	batch0 := int32(0)
	batch1 := int32(1)
	op0 := makeOutpoint("p0", 0)
	op1 := makeOutpoint("p1", 0)

	row0 := VTXORow{
		Outpoint:         op0,
		RoundID:          &roundID,
		BatchOutputIndex: &batch0,
	}
	row1 := VTXORow{
		Outpoint:         op1,
		RoundID:          &roundID,
		BatchOutputIndex: &batch1,
	}

	tree0 := &tree.Tree{Root: &tree.Node{}}
	tree1 := &tree.Tree{Root: &tree.Node{}}

	parent0 := &vtxoLineage{
		roundID:        "r0",
		commitmentTxID: commitment,
		batchExpiry:    100,
		ancestryPaths: []ancestryFragment{{
			treePath:       tree0,
			treePathTLV:    []byte("tlv-batch-0"),
			commitmentTxID: commitment,
			treeDepth:      3,
		}},
	}
	parent1 := &vtxoLineage{
		roundID:        "r0",
		commitmentTxID: commitment,
		batchExpiry:    100,
		ancestryPaths: []ancestryFragment{{
			treePath:       tree1,
			treePathTLV:    []byte("tlv-batch-1"),
			commitmentTxID: commitment,
			treeDepth:      3,
		}},
	}

	resolver := &lineageResolver{}
	combined, err := resolver.combineVirtualLineage(
		t.Context(), makeOutpoint("child", 0), []VTXORow{row0, row1},
		[]wire.OutPoint{op0, op1}, []*vtxoLineage{parent0, parent1},
	)
	require.NoError(t, err)

	require.Len(
		t, combined.ancestryPaths, 2, "two parents in same "+
			"commitment but distinct batch trees must produce "+
			"two AncestryPaths so the recipient can publish "+
			"each tree independently",
	)

	// Both fragments must carry the shared commitment_txid; their
	// inherited tree paths are the two distinct batch-tree roots.
	require.Equal(t, commitment, combined.ancestryPaths[0].commitmentTxID)
	require.Equal(t, commitment, combined.ancestryPaths[1].commitmentTxID)

	// First-appearance ordering: parent0 appears before parent1.
	require.Same(t, tree0, combined.ancestryPaths[0].treePath)
	require.Same(t, tree1, combined.ancestryPaths[1].treePath)

	// Each fragment carries its own input index (parent index in the
	// original lineage slice).
	require.Equal(t, []uint32{0}, combined.ancestryPaths[0].inputIndices)
	require.Equal(t, []uint32{1}, combined.ancestryPaths[1].inputIndices)
}

// TestCombineVirtualLineageSameCommitmentSameBatchTreeStillMerges
// verifies the M-5 fix preserves the existing merge behavior for the
// happy path: two parents sharing both commitment AND batch tree must
// still collapse into one AncestryPath via the rep-picker (with the
// real flow taking the tryResolveCombinedRoundPath fast path that the
// resolver short-circuits to in production). This pins the
// non-regression: M-5 only splits when batch trees differ, not when
// they match.
func TestCombineVirtualLineageSameCommitmentSameBatchMerges(t *testing.T) {
	t.Parallel()

	commitment := makeCommitmentHash("shared-commitment")
	sharedTree := &tree.Tree{Root: &tree.Node{}}

	// Inherited multi-fragment parents whose fragments reference the
	// same root *tree.Tree pointer collide on the batch-tree
	// discriminator (root TXID is identical) and merge into one
	// AncestryPath.
	parents := []*vtxoLineage{
		{
			roundID:        "r0",
			commitmentTxID: commitment,
			batchExpiry:    100,
			ancestryPaths: []ancestryFragment{{
				treePath:       sharedTree,
				treePathTLV:    []byte("tlv-shared"),
				commitmentTxID: commitment,
				treeDepth:      3,
			}},
		},
		{
			roundID:        "r0",
			commitmentTxID: commitment,
			batchExpiry:    100,
			ancestryPaths: []ancestryFragment{{
				treePath:       sharedTree,
				treePathTLV:    []byte("tlv-shared"),
				commitmentTxID: commitment,
				treeDepth:      3,
			}},
		},
	}

	resolver := &lineageResolver{}
	combined, err := resolver.combineVirtualLineage(
		t.Context(),
		makeOutpoint("child", 0),
		[]VTXORow{
			{Outpoint: makeOutpoint("p0", 0)},
			{Outpoint: makeOutpoint("p1", 0)},
		},
		[]wire.OutPoint{
			makeOutpoint("p0", 0),
			makeOutpoint("p1", 0),
		},
		parents,
	)
	require.NoError(t, err)
	require.Len(
		t, combined.ancestryPaths, 1, "two parents sharing "+
			"commitment AND batch tree must merge into one "+
			"fragment",
	)
	require.Equal(t, []uint32{0, 1},
		combined.ancestryPaths[0].inputIndices)
}

// TestCombineGroupAncestryRejectsNilTreePathRep verifies the H-4 fix:
// a fragment carrying populated treePathTLV but a nil treePath pointer
// must NOT survive the rep-picker. The legacy comparator picked solely
// by treeDepth, allowing such fragments to slip through and trip the
// nil-tree hard-error inside arkrpc.AncestryPathFromTree downstream
// with a confusing generic gRPC Internal. The rep-picker now skips
// nil-treePath candidates so the failure attaches to the typed
// "missing inherited tree path" error at its source.
func TestCombineGroupAncestryRejectsNilTreePathRep(t *testing.T) {
	t.Parallel()

	commitment := makeCommitmentHash("commit")

	// Two inherited fragments that share both commitment and batch
	// tree (so they fall into one group) but neither carries a real
	// *tree.Tree pointer. The rep-picker must reject the group
	// rather than silently picking the deepest TLV-only fragment.
	parent0 := &vtxoLineage{
		roundID:        "r0",
		commitmentTxID: commitment,
		ancestryPaths: []ancestryFragment{{
			treePathTLV:    []byte("tlv-only-shallow"),
			commitmentTxID: commitment,
			treeDepth:      1,
		}},
	}
	parent1 := &vtxoLineage{
		roundID:        "r0",
		commitmentTxID: commitment,
		ancestryPaths: []ancestryFragment{{
			treePathTLV:    []byte("tlv-only-deepest"),
			commitmentTxID: commitment,
			treeDepth:      5,
		}},
	}

	resolver := &lineageResolver{}
	_, err := resolver.combineVirtualLineage(
		t.Context(),
		makeOutpoint("child", 0),
		[]VTXORow{
			{Outpoint: makeOutpoint("p0", 0)},
			{Outpoint: makeOutpoint("p1", 0)},
		},
		[]wire.OutPoint{
			makeOutpoint("p0", 0),
			makeOutpoint("p1", 0),
		},
		[]*vtxoLineage{parent0, parent1},
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"missing inherited tree path", "rep-picker must surface a "+
			"typed lineage error rather than letting the "+
			"nil-tree fragment leak downstream",
	)
}
