package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestValidateProofDescriptorRejectsMalformedAncestry locks in the
// per-fragment well-formedness checks added on top of the
// slice-length / commitment / round-id / height / expiry / status
// pre-conditions. Each case builds an otherwise-valid descriptor and
// injects exactly one structural defect on a fragment so the test
// exercises one rejection branch per row.
func TestValidateProofDescriptorRejectsMalformedAncestry(t *testing.T) {
	t.Parallel()

	makeFragment := func() vtxo.Ancestry {
		return vtxo.Ancestry{
			TreePath: &tree.Tree{
				Root: &tree.Node{},
			},
			CommitmentTxID: chainhash.HashH([]byte("frag")),
			TreeDepth:      1,
		}
	}

	makeBaseDescriptor := func() *vtxo.Descriptor {
		return &vtxo.Descriptor{
			CommitmentTxID: chainhash.HashH([]byte("commit")),
			RoundID:        "round-1",
			CreatedHeight:  100,
			BatchExpiry:    1000,
			RelativeExpiry: 144,
			Status:         vtxo.VTXOStatusLive,
			Ancestry: []vtxo.Ancestry{
				makeFragment(),
			},
		}
	}

	cases := []struct {
		name       string
		mutate     func(d *vtxo.Descriptor)
		wantReason string
	}{
		{
			name: "fragment 0 nil tree path",
			mutate: func(d *vtxo.Descriptor) {
				d.Ancestry[0].TreePath = nil
			},
			wantReason: "ancestry fragment 0 missing tree path",
		},
		{
			name: "fragment 0 empty tree (nil root)",
			mutate: func(d *vtxo.Descriptor) {
				d.Ancestry[0].TreePath = &tree.Tree{}
			},
			wantReason: "ancestry fragment 0 has empty tree",
		},
		{
			name: "fragment 0 zero commitment txid",
			mutate: func(d *vtxo.Descriptor) {
				d.Ancestry[0].CommitmentTxID = chainhash.Hash{}
			},
			wantReason: "fragment 0 missing commitment txid",
		},
		{
			name: "fragment 1 nil tree path (multi-fragment)",
			mutate: func(d *vtxo.Descriptor) {
				d.Ancestry = append(d.Ancestry, vtxo.Ancestry{
					CommitmentTxID: chainhash.HashH(
						[]byte("frag-2"),
					),
					TreeDepth: 2,
				})
			},
			wantReason: "ancestry fragment 1 missing tree path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desc := makeBaseDescriptor()
			tc.mutate(desc)

			err := validateProofDescriptor(desc)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrUnrollProofUnavailable)
			require.Contains(t, err.Error(), tc.wantReason)
		})
	}
}

// TestValidateProofDescriptorAcceptsZeroTreeDepth is the regression
// guard for #372 ("Untrusted zero tree depth can block unroll
// proofs"). TreeDepth is expiry-timing metadata, not proof material —
// the proof assembler walks TreePath directly. A malicious indexer
// that supplies a non-empty TreePath but a defaulted/forged TreeDepth
// of zero must NOT prevent unilateral exit; otherwise the operator
// can strand otherwise-recoverable funds simply by zeroing one
// scalar.
//
// The test also asserts the gate has no sticky state: repeated calls
// with the same zero-depth descriptor keep succeeding, so an unroll
// retry after a transient failure earlier in the pipeline still
// reaches proof assembly.
func TestValidateProofDescriptorAcceptsZeroTreeDepth(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{
		CommitmentTxID: chainhash.HashH([]byte("commit")),
		RoundID:        "round-1",
		CreatedHeight:  100,
		BatchExpiry:    1000,
		RelativeExpiry: 144,
		Status:         vtxo.VTXOStatusLive,
		Ancestry: []vtxo.Ancestry{{
			TreePath: &tree.Tree{
				Root: &tree.Node{},
			},
			CommitmentTxID: chainhash.HashH([]byte("frag")),
			// Zero TreeDepth: hostile/legacy/forged indexer value.
			// Proof assembly only needs TreePath, so this must
			// pass.
			TreeDepth: 0,
		}},
	}

	// Two calls in a row exercise the "no sticky state" invariant:
	// the unroll boundary cannot persist a rejection from one call
	// into the next.
	require.NoError(t, validateProofDescriptor(desc))
	require.NoError(t, validateProofDescriptor(desc))
}

// TestValidateProofDescriptorAcceptsWellFormedMultiFragment is the
// positive companion to the rejection table above: a structurally clean
// multi-fragment descriptor must pass validation cleanly so the unroll
// path is not blocked by an over-zealous gate.
func TestValidateProofDescriptorAcceptsWellFormedMultiFragment(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{
		CommitmentTxID: chainhash.HashH([]byte("commit")),
		RoundID:        "round-1",
		CreatedHeight:  100,
		BatchExpiry:    1000,
		RelativeExpiry: 144,
		Status:         vtxo.VTXOStatusLive,
		Ancestry: []vtxo.Ancestry{
			{
				TreePath: &tree.Tree{
					Root: &tree.Node{},
				},
				CommitmentTxID: chainhash.HashH([]byte("a")),
				TreeDepth:      1,
			},
			{
				TreePath: &tree.Tree{
					Root: &tree.Node{},
				},
				CommitmentTxID: chainhash.HashH([]byte("b")),
				TreeDepth:      2,
			},
		},
	}

	require.NoError(t, validateProofDescriptor(desc))
}
