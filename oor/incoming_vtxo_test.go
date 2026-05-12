package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
func TestBuildIncomingVTXODescriptorNormalizesPrimaryAncestry(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, commitHash, recipientKey,
		operatorKey := buildTestIncomingMaterialization(t)

	otherHash := chainhash.Hash{0xee}
	ancestry := validTestIncomingAncestry(otherHash)
	ancestry = append(
		ancestry, validTestIncomingAncestry(commitHash)[0],
	)

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
				ChainDepth:     1,
				CreatedHeight:  500,
				Ancestry:       ancestry,
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, desc.Ancestry, 2)
	require.Equal(t, commitHash, desc.Ancestry[0].CommitmentTxID)
	require.Equal(t, otherHash, desc.Ancestry[1].CommitmentTxID)
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
			name: "duplicate commitment txid across fragments",
			mutate: func(m *IncomingVTXOMetadata) {
				dup := m.Ancestry[0]
				m.Ancestry = append(m.Ancestry, dup)
			},
			wantReason: "duplicate commitment txid",
		},
		{
			name: "nil tree path",
			mutate: func(m *IncomingVTXOMetadata) {
				m.Ancestry[0].TreePath = nil
			},
			wantReason: "nil tree path",
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
