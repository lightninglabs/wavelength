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
			OwnerKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				TreeDepth:      2,
				ChainDepth:     wantChainDepth,
				CreatedHeight:  500,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, wantChainDepth, desc.ChainDepth)
	require.Equal(t, 2, desc.TreeDepth)
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
			OwnerKey: keychain.KeyDescriptor{
				PubKey: recipientKey.PubKey(),
			},
			OperatorKey: operatorKey,
			ExitDelay:   10,
			Metadata: IncomingVTXOMetadata{
				RoundID:        "test-round",
				CommitmentTxID: commitHash,
				BatchExpiry:    1000,
				TreeDepth:      1,
				ChainDepth:     0,
				CreatedHeight:  500,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, 0, desc.ChainDepth)
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
