package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	lib_tree "github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBuildIncomingVTXODescriptorChainDepth verifies that
// BuildIncomingVTXODescriptor propagates ChainDepth from the incoming
// metadata to the resulting descriptor without modification.
func TestBuildIncomingVTXODescriptorChainDepth(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	const wantChainDepth = 3

	desc, err := BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				ChainDepth:     wantChainDepth,
				CreatedHeight:  500,
				Ancestry: validTestIncomingAncestry(
					commitHash,
				),
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, wantChainDepth, desc.ChainDepth)
	require.Equal(t, 1, desc.MaxTreeDepth())
}

// TestBuildIncomingVTXODescriptorZeroChainDepth verifies that a VTXO
// built with ChainDepth 0 (e.g. first OOR hop from a round VTXO)
// preserves the zero value explicitly.
func TestBuildIncomingVTXODescriptorZeroChainDepth(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	desc, err := BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				ChainDepth:     0,
				CreatedHeight:  500,
				Ancestry: validTestIncomingAncestry(
					commitHash,
				),
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 0, desc.ChainDepth)
}

// TestBuildIncomingVTXODescriptorNormalizesPrimaryAncestry verifies that
// cross-round multi-input metadata may carry the descriptor's commitment
// fragment after another valid fragment, and descriptor construction still
// preserves legacy Ancestry[0] primary semantics.
//
// The test exercises the genuine cross-round multi-input shape: a
// two-input Ark tx with one fragment per input. The secondary fragment
// is supplied first in the metadata so descriptor construction must
// reorder it before persistence.
func TestBuildIncomingVTXODescriptorNormalizesPrimaryAncestry(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commits, recipientKey,
		operatorKey := buildTestIncomingMaterializationMultiInput(t)

	// BuildArkPSBT applies BIP69 input ordering, so locate which
	// of the two commitment hashes ends up at Ark input index 0
	// vs 1 in the canonical PSBT. Each fragment must name the
	// input it actually serves.
	indexOf := func(h chainhash.Hash) uint32 {
		for i, in := range arkPSBT.UnsignedTx.TxIn {
			if in.PreviousOutPoint.Hash == h {
				return uint32(i)
			}
		}
		t.Fatalf("commit %s not found in ark inputs", h)

		return 0
	}
	primaryCommit := commits[0]
	secondaryCommit := commits[1]

	ancestry := []vtxo.Ancestry{
		// Secondary fragment first — descriptor construction must
		// re-order so Ancestry[0] is the primary commitment.
		{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash: secondaryCommit,
				},
			},
			CommitmentTxID: secondaryCommit,
			InputIndices: []uint32{
				indexOf(secondaryCommit),
			},
			TreeDepth: 1,
		},
		{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash: primaryCommit,
				},
			},
			CommitmentTxID: primaryCommit,
			InputIndices: []uint32{
				indexOf(primaryCommit),
			},
			TreeDepth: 1,
		},
	}

	desc, err := BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: primaryCommit,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry:       ancestry,
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, desc.Ancestry, 2)
	require.Equal(t, primaryCommit, desc.Ancestry[0].CommitmentTxID)
	require.Equal(t, secondaryCommit, desc.Ancestry[1].CommitmentTxID)
}

// TestBuildIncomingVTXODescriptorSameCommitmentMultiLeaf verifies that
// two ancestry fragments anchored at the SAME commitment txid but
// carrying distinct tree paths are accepted. This is the shape the
// indexer produces for an OOR spend whose inputs sit at different
// leaves of one commitment tree (one AncestryPath per leaf), and is a
// regression test for wavelength#969, where the receive-side duplicate
// check keyed on commitment txid alone rejected the incoming change
// VTXO and stranded it unmaterialized.
func TestBuildIncomingVTXODescriptorSameCommitmentMultiLeaf(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commits, recipientKey,
		operatorKey := buildTestIncomingMaterializationMultiInput(t)

	commit := commits[0]

	// Two fragments anchored at the same commitment. The tree paths
	// differ (distinct batch outpoint indices stand in for distinct
	// leaf paths within the commitment tree), so the fragments are
	// NOT duplicates of one another. Together they cover both Ark tx
	// inputs.
	ancestry := []vtxo.Ancestry{
		{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash:  commit,
					Index: 0,
				},
			},
			CommitmentTxID: commit,
			InputIndices: []uint32{
				0,
			},
			TreeDepth: 1,
		},
		{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash:  commit,
					Index: 1,
				},
			},
			CommitmentTxID: commit,
			InputIndices: []uint32{
				1,
			},
			TreeDepth: 1,
		},
	}

	desc, err := BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commit,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry:       ancestry,
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, desc.Ancestry, 2)
	require.Equal(t, commit, desc.Ancestry[0].CommitmentTxID)
	require.Equal(t, commit, desc.Ancestry[1].CommitmentTxID)
	require.NotEqual(
		t, desc.Ancestry[0].TreePath.BatchOutpoint,
		desc.Ancestry[1].TreePath.BatchOutpoint,
	)
}

// TestBuildIncomingVTXODescriptorRejectsConflictingLineageNode verifies
// the cross-fragment proof-node consistency gate: two fragments whose
// trees carry the SAME transaction (same txid) with DIFFERENT bytes —
// here, one signed and one signature-stripped variant of one node —
// must be rejected at the receive boundary. Without the gate, the
// descriptor persists cleanly and the conflicting duplicate only
// surfaces as an ErrUnrollProofInvalid failure of the ENTIRE proof at
// unilateral-exit time, when the user is racing a CSV.
func TestBuildIncomingVTXODescriptorRejectsConflictingLineageNode(
	t *testing.T) {

	t.Parallel()

	arkPSBT, _, recipients, commits, recipientKey,
		operatorKey := buildTestIncomingMaterializationMultiInput(t)

	commit := commits[0]

	// One lineage node in two variants: signed, and with the
	// signature stripped. Both build a transaction with the same
	// txid (the witness does not enter the txid), but their
	// broadcastable bytes differ — exactly the conflicting-duplicate
	// shape addProofNode rejects during proof assembly.
	nodePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	digest := chainhash.HashH([]byte("conflicting-lineage-node"))
	nodeSig, err := schnorr.Sign(nodePriv, digest[:])
	require.NoError(t, err)

	nodeInput := wire.OutPoint{Hash: chainhash.Hash{0x77}, Index: 0}
	nodeOutputs := []*wire.TxOut{{
		Value:    5_000,
		PkScript: newTestTaprootPkScript(t, operatorKey),
	}}

	signedNode := &lib_tree.Node{
		Input:     nodeInput,
		Outputs:   nodeOutputs,
		Signature: nodeSig,
	}
	strippedNode := &lib_tree.Node{
		Input:   nodeInput,
		Outputs: nodeOutputs,
	}

	ancestry := []vtxo.Ancestry{
		{
			TreePath: &lib_tree.Tree{
				Root: signedNode,
				BatchOutpoint: wire.OutPoint{
					Hash:  commit,
					Index: 0,
				},
			},
			CommitmentTxID: commit,
			InputIndices: []uint32{
				0,
			},
			TreeDepth: 1,
		},
		{
			TreePath: &lib_tree.Tree{
				Root: strippedNode,
				BatchOutpoint: wire.OutPoint{
					Hash:  commit,
					Index: 1,
				},
			},
			CommitmentTxID: commit,
			InputIndices: []uint32{
				1,
			},
			TreeDepth: 1,
		},
	}

	_, err = BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commit,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry:       ancestry,
			},
		},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicting duplicate of lineage tx")

	var ancestryErr *ErrInvalidAncestry
	require.ErrorAs(t, err, &ancestryErr)
}

// TestBuildIncomingVTXODescriptorRejectsNilArk verifies that a nil Ark
// PSBT is rejected early.
func TestBuildIncomingVTXODescriptorRejectsNilArk(t *testing.T) {
	t.Parallel()

	_, err := BuildIncomingVTXODescriptor(nil, IncomingVTXOConfig{
		Metadata: IncomingVTXOMetadata{
			RoundID:        "test-round",
			CommitmentTxID: chainhash.Hash{0x01},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ark psbt must be provided")
}

// TestBuildIncomingVTXODescriptorPreservesMatchingPolicyTemplate verifies that
// a server-supplied policy template that binds to the recipient output pkScript
// is preserved verbatim on the descriptor rather than being re-derived or
// silently downgraded to the standard template.
func TestBuildIncomingVTXODescriptorPreservesMatchingPolicyTemplate(
	t *testing.T) {

	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	// The fixture builds the recipient output as a standard VTXO taproot
	// for (recipientKey, operatorKey, exitDelay=10), so the matching
	// template is the standard template over the same parameters.
	template, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)

	desc, err := BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			ExitDelay:      10,
			PolicyTemplate: template,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry: validTestIncomingAncestry(
					commitHash,
				),
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, template, desc.PolicyTemplate)
}

// TestBuildIncomingVTXODescriptorPreservesTaprootAssetRoot verifies the
// receiver binds the policy and asset root to the actual Ark output before
// persisting the root needed for later spends.
func TestBuildIncomingVTXODescriptorPreservesTaprootAssetRoot(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	template, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)
	assetRoot := chainhash.Hash{0x91, 0x92, 0x93}
	assetDesc := &vtxo.Descriptor{
		PolicyTemplate:     template,
		TaprootAssetRoot:   &assetRoot,
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
	}
	assetPkScript, err := assetDesc.EffectivePkScript()
	require.NoError(t, err)
	arkPSBT.UnsignedTx.TxOut[recipients[0].OutputIndex].PkScript =
		assetPkScript

	cfg := IncomingVTXOConfig{
		OutputIndex: recipients[0].OutputIndex,
		ClientKey: keychain.KeyDescriptor{
			PubKey: recipientKey.PubKey(),
		},
		OperatorKey:        operatorKey,
		ExitDelay:          10,
		PolicyTemplate:     template,
		TaprootAssetRoot:   &assetRoot,
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
		Metadata: IncomingVTXOMetadata{
			RoundID:        "test-round",
			CommitmentTxID: commitHash,
			BatchExpiry:    1000,
			ChainDepth:     1,
			CreatedHeight:  500,
			Ancestry: validTestIncomingAncestry(
				commitHash,
			),
		},
	}
	desc, err := BuildIncomingVTXODescriptor(arkPSBT, cfg)
	require.NoError(t, err)
	require.Equal(t, &assetRoot, desc.TaprootAssetRoot)
	require.Equal(t, "asset-id:010203", desc.TaprootAssetRef)
	require.Equal(t, uint64(21), desc.TaprootAssetAmount)
	require.Equal(t, assetPkScript, desc.PkScript)

	wrongRoot := assetRoot
	wrongRoot[0] ^= 0xff
	cfg.TaprootAssetRoot = &wrongRoot
	_, err = BuildIncomingVTXODescriptor(arkPSBT, cfg)
	require.ErrorContains(t, err, "does not match ark output pkscript")
}

// TestBuildIncomingVTXODescriptorRejectsMismatchedPolicyTemplate verifies that
// a server-supplied policy template that decodes cleanly but does not bind to
// the recipient output pkScript is rejected, so a mismatched template can never
// be silently materialized.
func TestBuildIncomingVTXODescriptorRejectsMismatchedPolicyTemplate(
	t *testing.T) {

	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	// A standard template over a different owner key decodes cleanly but
	// reconstructs a different pkScript, so it must fail the bind check
	// against the recipient output rather than downgrade silently.
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	mismatched, err := arkscript.EncodeStandardVTXOTemplate(
		otherKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)

	_, err = BuildIncomingVTXODescriptor(arkPSBT,
		IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			ExitDelay:      10,
			PolicyTemplate: mismatched,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry: validTestIncomingAncestry(
					commitHash,
				),
			},
		},
	)
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"does not match ark output pkscript",
	)
}

// TestBuildIncomingVTXODescriptorRejectsInvalidAncestry exercises every
// rejection branch of the receive-side ancestry cross-check. Each case
// is a structurally valid IncomingVTXOMetadata except for the named
// invariant; the test asserts both that an error is returned and that
// the typed *ErrInvalidAncestry chain is preserved so wallet callers
// can route on the cause via errors.As.
func TestBuildIncomingVTXODescriptorRejectsInvalidAncestry(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	baseCfg := func(meta IncomingVTXOMetadata) IncomingVTXOConfig {
		return IncomingVTXOConfig{
			OutputIndex: recipients[0].OutputIndex,
			ClientKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata:    meta,
		}
	}

	otherHash := chainhash.Hash{0xee}

	cases := []struct {
		name       string
		mutate     func(m *IncomingVTXOMetadata)
		wantReason string
	}{
		{
			name: "empty ancestry",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry = nil
			},
			wantReason: "empty ancestry",
		},
		{
			name: "metadata commitment txid missing",
			mutate: func(m *IncomingVTXOMetadata) {
				// Move the fragment to a different commitment
				// (and re-anchor its batch outpoint so the
				// fragment-to-commitment binding stays
				// consistent — we want this case to exercise
				// only the "no primary fragment" branch).
				m.Ancestry[0].CommitmentTxID = otherHash
				m.Ancestry[0].TreePath.BatchOutpoint.Hash =
					otherHash
			},
			wantReason: "no ancestry fragment matches",
		},
		{
			name: "fragment tree path commitment mismatch",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].TreePath.BatchOutpoint.Hash =
					otherHash
			},
			wantReason: "tree path batch outpoint hash",
		},
		{
			// Same commitment AND same tree path is a true
			// duplicate. Same commitment with a DIFFERENT tree
			// path is legal (different leaves of one commitment
			// tree) and is covered by the multi-leaf success
			// test below.
			name: "identical fragment duplicated across slice",
			mutate: func(m *IncomingVTXOMetadata) {
				dup := m.Ancestry[0]
				m.Ancestry = append(m.Ancestry, dup)
			},
			wantReason: "duplicates the tree path",
		},
		{
			name: "nil tree path",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].TreePath = nil
			},
			wantReason: "nil tree path",
		},
		{
			// Regression for wavelength#370: a zero TreeDepth
			// would otherwise persist and either silently strand
			// the VTXO at unroll time or under-report the expiry
			// window.
			name: "zero tree depth",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].TreeDepth = 0
			},
			wantReason: "must be non-zero",
		},
		{
			// Regression for wavelength#370: a non-zero claim
			// that disagrees with the actual tree path is the
			// more dangerous variant because it survives the
			// obvious zero check downstream.
			name: "tree depth disagrees with path",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].TreeDepth = 9
			},
			wantReason: "does not match reconstructed",
		},
		{
			name: "empty input indices",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].InputIndices = nil
			},
			wantReason: "empty input indices",
		},
		{
			name: "input index out of range",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].InputIndices = []uint32{
					99,
				}
			},
			wantReason: "out of range",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry: validTestIncomingAncestry(
					commitHash,
				),
			}
			tc.mutate(&meta)

			_, err := BuildIncomingVTXODescriptor(
				arkPSBT, baseCfg(meta),
			)
			require.Error(t, err)
			require.ErrorIs(t, err, &ErrInvalidAncestry{})
			require.Contains(t, err.Error(), tc.wantReason)
		})
	}
}

// TestValidateIncomingAncestryInputCoverage exercises the InputIndices
// partition checks for multi-input Ark transactions. The other rejection
// branches are covered via BuildIncomingVTXODescriptor in
// TestBuildIncomingVTXODescriptorRejectsInvalidAncestry; here we drive
// validateIncomingAncestry directly so we can vary arkTxInputCount
// without rebuilding a real PSBT.
//
// The scenarios assert two properties that the receive boundary must
// enforce so that a malicious or truncated indexer response cannot
// strand received OOR funds:
//
//   - The union of all fragments' InputIndices covers every Ark tx
//     input (0..arkTxInputCount-1).
//   - No input index appears in more than one fragment (or twice
//     within a single fragment), since a duplicate hides a missing
//     fragment behind apparently-full coverage.
func TestValidateIncomingAncestryInputCoverage(t *testing.T) {
	t.Parallel()

	primary := chainhash.Hash{0x01}
	secondary := chainhash.Hash{0x02}

	fragment := func(commit chainhash.Hash,
		indices ...uint32) vtxo.Ancestry {

		return vtxo.Ancestry{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash: commit,
				},
			},
			CommitmentTxID: commit,
			InputIndices: append(
				[]uint32(nil), indices...,
			),
			TreeDepth: 1,
		}
	}

	cases := []struct {
		name            string
		arkTxInputCount uint32
		ancestry        []vtxo.Ancestry
		wantReason      string
	}{
		{
			name:            "single fragment covers single input",
			arkTxInputCount: 1,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0),
			},
		},
		{
			name:            "two fragments partition two inputs",
			arkTxInputCount: 2,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0),
				fragment(secondary, 1),
			},
		},
		{
			name:            "single fragment covers both inputs",
			arkTxInputCount: 2,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0, 1),
			},
		},
		{
			name:            "chained input may have two fragments",
			arkTxInputCount: 1,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0),
				fragment(secondary, 0),
			},
		},
		{
			name:            "missing coverage truncated fragment",
			arkTxInputCount: 2,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0),
			},
			wantReason: "ark tx input 1 is not covered",
		},
		{
			name:            "missing coverage gap mid range",
			arkTxInputCount: 3,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0, 2),
			},
			wantReason: "ark tx input 1 is not covered",
		},
		{
			name:            "duplicate within fragment",
			arkTxInputCount: 2,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0, 0),
			},
			wantReason: "duplicates an earlier index",
		},
		{
			name:            "cross duplicate misses input",
			arkTxInputCount: 2,
			ancestry: []vtxo.Ancestry{
				fragment(primary, 0),
				fragment(secondary, 0),
			},
			wantReason: "ark tx input 1 is not covered",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: primary,
				BatchExpiry:    1000,
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry:       tc.ancestry,
			}

			err := validateIncomingAncestry(
				meta, tc.arkTxInputCount,
			)

			if tc.wantReason == "" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
			require.ErrorIs(t, err, &ErrInvalidAncestry{})
			require.Contains(t, err.Error(), tc.wantReason)
		})
	}
}
