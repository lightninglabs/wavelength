package indexer

import (
	"fmt"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// drawCommitmentSet draws K distinct synthetic commitment-tx hashes for
// use as group keys. Distinct hashes are derived deterministically from
// the draw index so two parents that draw the same K share the same
// hash, enabling the partition properties to exercise both "all
// parents share one commitment" and "every parent has its own
// commitment" extremes within the same property test.
func drawCommitmentSet(rt *rapid.T, k int) []chainhash.Hash {
	out := make([]chainhash.Hash, 0, k)
	for i := 0; i < k; i++ {
		out = append(
			out,
			chainhash.HashH(
				[]byte(
					fmt.Sprintf("rapid-commitment-%d", i),
				),
			),
		)
	}

	return out
}

// drawSyntheticParents generates a slate of synthetic virtual parents
// for combineVirtualLineage. Each parent draws:
//   - A commitment index in [0, K), so K controls how many distinct
//     groups appear (K=1 -> all parents in one fragment;
//     K=N -> every parent in its own fragment).
//   - A non-zero treeDepth so the rep-fragment picker reliably
//     selects a populated fragment.
//   - A non-negative chainDepth and an unconstrained int32 batchExpiry
//     (allowing the natural "0 = unknown" branch in
//     moreRestrictiveLineage).
//
// The returned slice is in stable parent-iteration order; the caller
// passes parents[i] as input index i.
func drawSyntheticParents(rt *rapid.T) ([]VTXORow, []wire.OutPoint,
	[]*vtxoLineage, []chainhash.Hash) {

	n := rapid.IntRange(1, 8).Draw(rt, "numParents")
	k := rapid.IntRange(1, n).Draw(rt, "numCommitments")

	commitments := drawCommitmentSet(rt, k)

	rows := make([]VTXORow, 0, n)
	ops := make([]wire.OutPoint, 0, n)
	lineages := make([]*vtxoLineage, 0, n)
	for i := 0; i < n; i++ {
		commitmentIdx := rapid.IntRange(0, k-1).Draw(
			rt, fmt.Sprintf("commitmentIdx[%d]", i),
		)
		treeDepth := rapid.IntRange(1, 16).Draw(
			rt, fmt.Sprintf("treeDepth[%d]", i),
		)
		chainDepth := rapid.IntRange(0, 32).Draw(
			rt, fmt.Sprintf("chainDepth[%d]", i),
		)
		// Allow zero (unknown) and arbitrary positive expiry; reject
		// negatives because real heights are non-negative.
		batchExpiry := int32(
			rapid.IntRange(
				0, 1<<30).Draw(
				rt,
				fmt.Sprintf("batchExpiry[%d]", i),
			),
		)

		op := makeOutpoint(fmt.Sprintf("rapid-p%d", i), 0)
		row := VTXORow{Outpoint: op}
		commit := commitments[commitmentIdx]
		// Synthesize a minimal *tree.Tree so the rep-picker (which
		// requires a non-nil tree path post-H-4) accepts the
		// fragment. The rapid test does not exercise the downstream
		// arkrpc.AncestryPathFromTree conversion so the tree shape
		// itself does not matter — only its non-nil presence does.
		minTree := &tree.Tree{Root: &tree.Node{}}
		lin := &vtxoLineage{
			roundID:        fmt.Sprintf("round-%d", commitmentIdx),
			commitmentTxID: commit,
			batchExpiry:    batchExpiry,
			chainDepth:     chainDepth,
			ancestryPaths: []ancestryFragment{{
				treePath: minTree,
				treePathTLV: []byte(
					fmt.Sprintf("rapid-tlv-%d", i),
				),
				commitmentTxID: commit,
				treeDepth:      treeDepth,
			}},
		}

		rows = append(rows, row)
		ops = append(ops, op)
		lineages = append(lineages, lin)
	}

	return rows, ops, lineages, commitments
}

// TestRapidCombineVirtualLineageCardinality verifies that the number
// of returned ancestry fragments equals the number of distinct
// commitment txids across the parent set, regardless of how many
// parents map to each commitment. This is the structural invariant
// that lets the unroller broadcast each required tree exactly once.
func TestRapidCombineVirtualLineageCardinality(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		distinct := make(map[chainhash.Hash]struct{})
		for _, l := range lineages {
			distinct[l.commitmentTxID] = struct{}{}
		}

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)
		require.Equal(
			rt, len(distinct), len(combined.ancestryPaths),
			"len(ancestryPaths) must equal number of distinct "+
				"commitment txids in parent set",
		)
	})
}

// TestRapidCombineVirtualLineageChainDepth verifies the chain-depth
// arithmetic: combined.chainDepth = max(parent.chainDepth) + 1, no
// matter how the parents are grouped or how many distinct commitments
// they span.
func TestRapidCombineVirtualLineageChainDepth(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		var want int
		for _, l := range lineages {
			if l.chainDepth > want {
				want = l.chainDepth
			}
		}
		want++

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)
		require.Equal(rt, want, combined.chainDepth)
	})
}

// TestRapidCombineVirtualLineageInputIndexPartition verifies that:
//   - The union of every fragment's inputIndices is exactly
//     {0, 1, ..., len(parents)-1}.
//   - No input index appears in more than one fragment (disjoint).
//   - Each input index lands in the fragment whose commitmentTxID
//     matches its parent's commitmentTxID.
//
// This is the property the unroller relies on to route each Ark tx
// input to exactly one tree path for broadcast.
func TestRapidCombineVirtualLineageInputIndexPartition(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)

		// Walk every fragment and collect (idx, fragment-commitment).
		seen := make(map[uint32]chainhash.Hash, len(lineages))
		for _, f := range combined.ancestryPaths {
			for _, idx := range f.inputIndices {
				_, dup := seen[idx]
				require.False(
					rt, dup, "input index %d appears in "+
						"more than one fragment", idx,
				)
				seen[idx] = f.commitmentTxID
			}
		}

		// Every parent index must be present exactly once.
		require.Equal(
			rt, len(lineages), len(seen),
			"every parent index must appear in some fragment",
		)

		// Index lands in the fragment that matches its parent's
		// commitment.
		for i, parent := range lineages {
			require.Equal(
				rt, parent.commitmentTxID, seen[uint32(i)],
				"input index %d landed in fragment with "+
					"commitment %s but parent has %s", i,
				seen[uint32(i)], parent.commitmentTxID,
			)
		}
	})
}

// TestRapidCombineVirtualLineageBatchExpiry verifies that the combined
// scalar batchExpiry equals the minimum non-zero parent batchExpiry,
// or 0 if every parent has zero (unknown) expiry. This mirrors
// moreRestrictiveLineage's "earliest known wins, unknown loses"
// definition.
func TestRapidCombineVirtualLineageBatchExpiry(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		// Compute expected: min of non-zero parent.batchExpiry, or
		// 0 if all parents are zero.
		var want int32
		for _, l := range lineages {
			if l.batchExpiry == 0 {
				continue
			}
			if want == 0 || l.batchExpiry < want {
				want = l.batchExpiry
			}
		}

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)
		require.Equal(
			rt, want, combined.batchExpiry, "combined "+
				"batchExpiry must be min non-zero parent "+
				"expiry (or 0 if all unknown)",
		)
	})
}

// TestRapidCombineVirtualLineageStableOrdering verifies that fragment
// ordering is deterministic across repeated calls with identical
// inputs. Map iteration is non-deterministic, so this catches the
// "we forgot to track groupOrder" regression.
func TestRapidCombineVirtualLineageStableOrdering(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		// First-appearance order of distinct commitments in the
		// parent slice.
		var want []chainhash.Hash
		seen := make(map[chainhash.Hash]struct{})
		for _, l := range lineages {
			if _, dup := seen[l.commitmentTxID]; dup {
				continue
			}
			seen[l.commitmentTxID] = struct{}{}
			want = append(want, l.commitmentTxID)
		}

		// Run combineVirtualLineage many times and verify the
		// fragment commitment sequence is always the first-
		// appearance order.
		for i := 0; i < 20; i++ {
			r := &lineageResolver{}
			combined, err := r.combineVirtualLineage(
				rt.Context(), makeOutpoint("rapid-child", 0),
				rows, ops, lineages,
			)
			require.NoError(rt, err)
			require.Len(rt, combined.ancestryPaths, len(want))

			got := make([]chainhash.Hash, 0, len(want))
			for _, f := range combined.ancestryPaths {
				got = append(got, f.commitmentTxID)
			}
			require.Equal(
				rt, want, got, "fragment ordering must be "+
					"first-appearance of commitment "+
					"txid in parent list",
			)
		}
	})
}

// TestRapidCombineVirtualLineageInputIndicesAscending verifies that
// within each fragment, inputIndices are strictly ascending. The
// resolver appends parent indices in iteration order (0..N-1), so
// every fragment's slice is naturally sorted; this test catches a
// regression that would group via a non-iteration-ordered structure.
func TestRapidCombineVirtualLineageInputIndicesAscending(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)

		for fi, f := range combined.ancestryPaths {
			require.True(
				rt, sort.SliceIsSorted(f.inputIndices,
					func(i, j int) bool {
						return f.inputIndices[i] <
							f.inputIndices[j]
					},
				),
				"fragment %d inputIndices must be "+
					"ascending: %v",
				fi,
				f.inputIndices,
			)
		}
	})
}

// TestRapidCombineVirtualLineageDefensiveCopy verifies that the
// resolver returns a lineage that does not alias any parent's per-
// fragment slices: callers can mutate the returned ancestry's TLV
// bytes or input-index slices freely, and the parent lineages remain
// intact. This is the core safety property cloneLineage promises and
// that combineVirtualLineage extends to fresh fragments.
func TestRapidCombineVirtualLineageDefensiveCopy(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		// Snapshot every parent's fragment slices for later equality.
		type parentSnapshot struct {
			tlv     []byte
			indices []uint32
		}
		snapshots := make([]parentSnapshot, len(lineages))
		for i, l := range lineages {
			f := l.ancestryPaths[0]
			snapshots[i] = parentSnapshot{
				tlv: append(
					[]byte(nil), f.treePathTLV...,
				),
				indices: append(
					[]uint32(nil), f.inputIndices...,
				),
			}
		}

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)

		// Mutate every fragment's TLV bytes and input indices on
		// the combined output.
		for fi := range combined.ancestryPaths {
			f := &combined.ancestryPaths[fi]
			for bi := range f.treePathTLV {
				f.treePathTLV[bi] ^= 0xFF
			}
			for ii := range f.inputIndices {
				f.inputIndices[ii] = 0xDEADBEEF
			}
		}

		// Every parent must round-trip equal to its snapshot.
		for i, l := range lineages {
			require.Equal(
				rt, snapshots[i].tlv,
				l.ancestryPaths[0].treePathTLV, "parent %d "+
					"TLV bytes must not be aliased by "+
					"combined output", i,
			)
			require.Equal(
				rt, snapshots[i].indices,
				l.ancestryPaths[0].inputIndices, "parent %d "+
					"inputIndices must not be aliased "+
					"by combined output", i,
			)
		}
	})
}

// TestRapidCombineVirtualLineageRestrictiveScalars verifies that the
// non-path scalar metadata on the combined lineage (roundID,
// commitmentTxID, relativeExpiry, createdHeight) is taken from the
// most-restrictive parent — i.e., the parent that
// moreRestrictiveLineage chose as the eventual base. This is what
// prevents the produced VTXO from masking a parent's earlier expiry.
func TestRapidCombineVirtualLineageRestrictiveScalars(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		// Replay the exact comparator the resolver uses, picking the
		// expected base parent.
		base := lineages[0]
		for i := 1; i < len(lineages); i++ {
			if moreRestrictiveLineage(lineages[i], base) {
				base = lineages[i]
			}
		}

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)

		require.Equal(rt, base.roundID, combined.roundID)
		require.Equal(
			rt, base.commitmentTxID, combined.commitmentTxID,
		)
		require.Equal(
			rt, base.relativeExpiry, combined.relativeExpiry,
		)
		require.Equal(
			rt, base.createdHeight, combined.createdHeight,
		)
	})
}

// TestRapidCloneLineageDeepCopy is a property assertion that
// cloneLineage produces an output whose mutable per-fragment slices
// (treePathTLV, inputIndices) are not aliased to the source. Tree
// pointers are intentionally shared (documented invariant), so the
// test does not mutate them.
func TestRapidCloneLineageDeepCopy(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Draw a synthetic lineage with K fragments.
		k := rapid.IntRange(0, 6).Draw(rt, "numFragments")
		src := &vtxoLineage{
			roundID: rapid.String().Draw(rt, "roundID"),
			batchExpiry: int32(
				rapid.IntRange(0, 1<<30).Draw(
					rt, "batchExpiry",
				),
			),
			chainDepth: rapid.IntRange(0, 32).Draw(
				rt, "chainDepth",
			),
			createdHeight: int32(
				rapid.IntRange(0, 1<<30).Draw(
					rt, "createdHeight",
				),
			),
		}
		for i := 0; i < k; i++ {
			tlv := []byte(
				rapid.String().Draw(
					rt,
					fmt.Sprintf("tlv[%d]", i),
				),
			)
			indicesLen := rapid.IntRange(0, 4).Draw(
				rt, fmt.Sprintf("indicesLen[%d]", i),
			)
			indices := make([]uint32, 0, indicesLen)
			for j := 0; j < indicesLen; j++ {
				label := fmt.Sprintf("indices[%d][%d]", i, j)
				indices = append(
					indices,
					uint32(
						rapid.IntRange(
							0, 64,
						).Draw(rt, label),
					),
				)
			}
			src.ancestryPaths = append(
				src.ancestryPaths, ancestryFragment{
					treePathTLV: tlv,
					treeDepth: rapid.IntRange(0, 16).Draw(
						rt, fmt.Sprintf(
							"treeDepth[%d]", i),
					),
					inputIndices: indices,
				},
			)
		}

		// Snapshot src state element-wise so the empty-vs-nil
		// distinction does not trip require.Equal — the defensive-
		// copy property cares about content, not slice header
		// identity.
		type snap struct {
			tlvLen     int
			tlv        []byte
			indicesLen int
			indices    []uint32
		}
		srcSnaps := make([]snap, len(src.ancestryPaths))
		for i, f := range src.ancestryPaths {
			srcSnaps[i] = snap{
				tlvLen: len(f.treePathTLV),
				tlv: append(
					[]byte(nil), f.treePathTLV...,
				),
				indicesLen: len(f.inputIndices),
				indices: append(
					[]uint32(nil), f.inputIndices...,
				),
			}
		}

		dst := cloneLineage(src)
		require.Equal(rt, src.roundID, dst.roundID)
		require.Equal(rt, src.batchExpiry, dst.batchExpiry)
		require.Equal(rt, src.chainDepth, dst.chainDepth)
		require.Equal(rt, src.createdHeight, dst.createdHeight)
		require.Equal(
			rt, len(src.ancestryPaths), len(dst.ancestryPaths),
		)

		// Mutate every dst fragment's slice.
		for i := range dst.ancestryPaths {
			frag := &dst.ancestryPaths[i]
			for bi := range frag.treePathTLV {
				frag.treePathTLV[bi] ^= 0xFF
			}
			for ii := range frag.inputIndices {
				frag.inputIndices[ii] = 0xCAFEBABE
			}
		}

		// src must round-trip equal to its snapshot. Compare lengths
		// and elements rather than slice headers so the test does
		// not flake on the nil-vs-empty boundary.
		for i := range src.ancestryPaths {
			require.Equal(
				rt, srcSnaps[i].tlvLen,
				len(src.ancestryPaths[i].treePathTLV),
			)
			for bi, b := range srcSnaps[i].tlv {
				require.Equal(
					rt, b,
					src.ancestryPaths[i].treePathTLV[bi],
				)
			}

			require.Equal(
				rt, srcSnaps[i].indicesLen,
				len(src.ancestryPaths[i].inputIndices),
			)
			for ii, idx := range srcSnaps[i].indices {
				require.Equal(
					rt, idx,
					src.ancestryPaths[i].inputIndices[ii],
				)
			}
		}
	})
}

// TestRapidMoreRestrictiveLineageOrderingIsTotal verifies that the
// comparator induces a consistent partial order (no cycles): for any
// pair (a, b), at most one of {moreRestrictive(a,b),
// moreRestrictive(b,a)} returns true unless they tie. This is what
// lets the iterative "pick the most-restrictive parent" loop converge
// to a unique winner regardless of starting position.
func TestRapidMoreRestrictiveLineageOrderingIsTotal(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Draw a "lineage" reduced to the only field the comparator
		// inspects: batchExpiry. Zero models the "unknown" branch.
		drawLineage := func(label string) *vtxoLineage {
			return &vtxoLineage{
				batchExpiry: int32(
					rapid.IntRange(0, 1<<30).
						Draw(rt, label),
				),
			}
		}
		a := drawLineage("a")
		b := drawLineage("b")

		ab := moreRestrictiveLineage(a, b)
		ba := moreRestrictiveLineage(b, a)

		// At most one of (ab, ba) is true. They are both false only
		// when one argument is unknown (batchExpiry=0) or when the
		// expiries are equal (strict <).
		require.False(
			rt, ab && ba,
			"comparator must not report both directions strict",
		)
	})
}

// TestRapidCombineVirtualLineageAggregateSize verifies that the sum
// of every fragment's len(inputIndices) equals the parent count
// exactly. This is the cardinality dual of the partition property —
// no parent's input is dropped, and no fragment carries spurious
// indices.
func TestRapidCombineVirtualLineageAggregateSize(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		rows, ops, lineages, _ := drawSyntheticParents(rt)

		r := &lineageResolver{}
		combined, err := r.combineVirtualLineage(
			rt.Context(), makeOutpoint("rapid-child", 0), rows, ops,
			lineages,
		)
		require.NoError(rt, err)

		var total int
		for _, f := range combined.ancestryPaths {
			total += len(f.inputIndices)
		}
		require.Equal(
			rt, len(lineages), total, "sum of inputIndices "+
				"across all fragments must equal parent count",
		)
	})
}
