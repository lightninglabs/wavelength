package unroll

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// fakeArtifactStore is a packageResolver test double that returns the
// configured packages and unresolved-input set.
type fakeArtifactStore struct {
	packages     []*db.OORPackageBundle
	unresolved   []wire.OutPoint
	resolveErr   error
	resolveCalls int
}

// ResolveUnrollPackages records the call and returns the configured
// fixture, fulfilling the unroll.packageResolver interface.
func (f *fakeArtifactStore) ResolveUnrollPackages(_ context.Context,
	target wire.OutPoint) (*db.OORUnrollPackages, error) {

	f.resolveCalls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}

	return &db.OORUnrollPackages{
		TargetOutpoint:             target,
		Packages:                   f.packages,
		UnresolvedCheckpointInputs: f.unresolved,
	}, nil
}

// emptyTree builds a minimal tree.Tree fixture suitable for the
// descriptor resolver. The fragment carries no real signed nodes — the
// resolver only walks `Root.NodesIter()` for tree-txid lookups in the
// OOR artifact-cross-check path, which is exercised separately.
func emptyTree(label string) *tree.Tree {
	hash := chainhash.HashH([]byte("tree-" + label))

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  hash,
				Index: 0,
			},
			Outputs:   []*wire.TxOut{},
			CoSigners: []*btcec.PublicKey{},
			Children:  make(map[uint32]*tree.Node),
		},
	}
}

// makeValidDescriptor builds a Descriptor that satisfies every
// validateProofDescriptor pre-condition (non-nil ancestry, non-zero
// commitment, round id, height, expiry, ChainDepth >= 0, status not
// terminal). Tests then mutate the Ancestry slice to drive the
// multi-tree code path.
func makeValidDescriptor(t *testing.T) *vtxo.Descriptor {
	t.Helper()

	hash := chainhash.HashH([]byte("desc"))
	commit := chainhash.HashH([]byte("commit"))

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		CommitmentTxID: commit,
		RoundID:        "round-test",
		CreatedHeight:  10,
		BatchExpiry:    100,
		RelativeExpiry: 144,
		Status:         vtxo.VTXOStatusLive,
		// Caller appends Ancestry entries.
	}
}

// TestDescriptorResolverMultiTreePopulatesEveryFragment verifies that
// the resolver iterates every entry in `desc.Ancestry` and appends each
// fragment's TreePath into `mat.TreePaths`. This is the wire-up that
// makes cross-commitment multi-input OOR VTXOs unrollable: every
// contributing commitment tree must surface to the planner.
func TestDescriptorResolverMultiTreePopulatesEveryFragment(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.Ancestry = []vtxo.Ancestry{
		{
			TreePath:       emptyTree("A"),
			CommitmentTxID: chainhash.HashH([]byte("commit-A")),
			TreeDepth:      3,
		},
		{
			TreePath:       emptyTree("B"),
			CommitmentTxID: chainhash.HashH([]byte("commit-B")),
			TreeDepth:      4,
		},
		{
			TreePath:       emptyTree("C"),
			CommitmentTxID: chainhash.HashH([]byte("commit-C")),
			TreeDepth:      5,
		},
	}

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
	}

	mat, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	require.NotNil(t, mat)

	require.Len(
		t, mat.TreePaths, len(desc.Ancestry),
		"every ancestry fragment with a non-nil TreePath must "+
			"appear in mat.TreePaths",
	)

	// Order must match Ancestry slice order so callers can correlate
	// fragments to indices.
	for i, want := range desc.Ancestry {
		require.Same(
			t, want.TreePath, mat.TreePaths[i], "TreePath "+
				"identity must round-trip; resolver shares "+
				"pointers (immutable contract)",
		)
	}
}

// TestDescriptorResolverRejectsNilTreePathFragment verifies that an
// Ancestry entry with a nil TreePath is rejected loudly via
// validateProofDescriptor rather than silently skipped. A nil fragment
// in any position is structurally malformed; accepting it would let a
// partial proof slip through to the unroll FSM and surface as a
// confusing failure deep in addTreePathNodes only after the actor has
// committed to AwaitingMaterialization.
func TestDescriptorResolverRejectsNilTreePathFragment(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.Ancestry = []vtxo.Ancestry{
		{
			TreePath:       emptyTree("A"),
			CommitmentTxID: chainhash.HashH([]byte("commit-A")),
			TreeDepth:      3,
		},
		{
			// nil TreePath must trip validateProofDescriptor.
			CommitmentTxID: chainhash.HashH([]byte("commit-B")),
			TreeDepth:      4,
		},
		{
			TreePath:       emptyTree("C"),
			CommitmentTxID: chainhash.HashH([]byte("commit-C")),
			TreeDepth:      5,
		},
	}

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
	}

	_, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnrollProofUnavailable)
	require.Contains(
		t, err.Error(),
		"ancestry fragment 1 missing tree path",
	)
}

// TestDescriptorResolverSingleAncestrySingleTree verifies the legacy
// shape — exactly one ancestry fragment — produces exactly one tree
// path, matching the same-commitment fast-path output of the server-
// side resolver.
func TestDescriptorResolverSingleAncestrySingleTree(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.Ancestry = []vtxo.Ancestry{{
		TreePath:       emptyTree("solo"),
		CommitmentTxID: chainhash.HashH([]byte("commit-solo")),
		TreeDepth:      4,
	}}

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
	}

	mat, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	require.Len(t, mat.TreePaths, 1)
	require.Equal(
		t, desc.RelativeExpiry, mat.CSVDelay,
		"resolver propagates RelativeExpiry as CSVDelay",
	)
}

// TestDescriptorResolverChainDepthZeroSkipsArtifactStore verifies that
// a round-direct descriptor (ChainDepth == 0) does not consult the
// ArtifactStore. This is the fast path for VTXOs that came straight
// from a round; loading OOR artifacts would be wasted work and could
// surface false errors when the artifact store is intentionally
// not configured for round-direct VTXOs.
func TestDescriptorResolverChainDepthZeroSkipsArtifactStore(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.ChainDepth = 0
	desc.Ancestry = []vtxo.Ancestry{{
		TreePath:       emptyTree("zero"),
		CommitmentTxID: chainhash.HashH([]byte("commit-zero")),
		TreeDepth:      1,
	}}

	artifacts := &fakeArtifactStore{}
	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		ArtifactStore: artifacts,
	}

	mat, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.NoError(t, err)
	require.Empty(
		t, mat.ExtraNodes, "ChainDepth=0 produces no OOR extra nodes",
	)
	require.Equal(
		t, 0, artifacts.resolveCalls,
		"ChainDepth=0 must not invoke ArtifactStore",
	)
}

// TestDescriptorResolverChainDepthRequiresArtifactStore verifies that
// a non-zero ChainDepth without a configured ArtifactStore is a
// fail-closed configuration error rather than a silently-truncated
// proof. This catches a regression where an unset ArtifactStore would
// produce an incomplete proof during unroll.
func TestDescriptorResolverChainDepthRequiresArtifactStore(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.ChainDepth = 1
	desc.Ancestry = []vtxo.Ancestry{{
		TreePath:       emptyTree("oor"),
		CommitmentTxID: chainhash.HashH([]byte("commit-oor")),
		TreeDepth:      1,
	}}

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		// ArtifactStore intentionally nil.
	}

	_, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnrollProofUnavailable)
}

// TestDescriptorResolverEmptyAncestryRejected verifies the fail-fast
// contract on a descriptor with zero ancestry fragments. This is a
// defense against the post-multi-tree refactor where a malformed
// descriptor (e.g., a partial materialization) might surface no
// ancestry and the unroll flow needs to reject loudly rather than
// produce an empty proof graph.
func TestDescriptorResolverEmptyAncestryRejected(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.Ancestry = nil

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
	}

	_, err := resolver.ResolveLineage(t.Context(), desc.Outpoint)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnrollProofUnavailable)
}

// TestResolveLineageHistoricalAcceptsTerminalStatus locks in the
// load-bearing asymmetry between the production and harness lineage
// paths: a descriptor that has transitioned to a terminal status
// (Spent / Forfeited / Failed) is rejected by the production
// ResolveLineage entry point — production unroll jobs must never
// start from a terminal target — but is accepted by
// ResolveLineageHistorical so test harnesses can walk the historical
// lineage of an already-spent or already-forfeited VTXO.
func TestResolveLineageHistoricalAcceptsTerminalStatus(t *testing.T) {
	t.Parallel()

	terminalStatuses := []vtxo.VTXOStatus{
		vtxo.VTXOStatusSpent,
		vtxo.VTXOStatusForfeited,
		vtxo.VTXOStatusFailed,
	}

	for _, status := range terminalStatuses {
		t.Run(status.String(), func(t *testing.T) {
			t.Parallel()

			desc := makeValidDescriptor(t)
			desc.Status = status
			desc.Ancestry = []vtxo.Ancestry{{
				TreePath: emptyTree(
					"hist-" + status.String(),
				),
				CommitmentTxID: chainhash.HashH(
					[]byte("commit-hist"),
				),
				TreeDepth: 2,
			}}

			resolver := &DescriptorLineageResolver{
				VTXOStore: &mockVTXOStore{
					desc: desc,
				},
			}

			// Production path must reject.
			_, err := resolver.ResolveLineage(
				t.Context(), desc.Outpoint,
			)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrUnrollTargetNotFound)
			require.Contains(t, err.Error(), "terminal")

			// Harness path must accept and produce a populated
			// LineageMaterial bundle from the same descriptor.
			mat, err := resolver.ResolveLineageHistorical(
				t.Context(), desc.Outpoint,
			)
			require.NoError(t, err)
			require.NotNil(t, mat)
			require.Len(t, mat.TreePaths, 1)
		})
	}
}

// TestResolveLineageHistoricalStillEnforcesShape verifies the harness
// path bypasses ONLY the terminal-status arm — every other shape
// invariant (ancestry presence, tree path well-formedness, commitment
// txid, etc.) must still be enforced. A harness call site cannot
// trick the resolver into accepting a structurally malformed
// descriptor by setting Status to terminal.
func TestResolveLineageHistoricalStillEnforcesShape(t *testing.T) {
	t.Parallel()

	desc := makeValidDescriptor(t)
	desc.Status = vtxo.VTXOStatusForfeited
	desc.Ancestry = nil

	resolver := &DescriptorLineageResolver{
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
	}

	_, err := resolver.ResolveLineageHistorical(
		t.Context(), desc.Outpoint,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnrollProofUnavailable)
	require.Contains(t, err.Error(), "missing ancestry")
}
