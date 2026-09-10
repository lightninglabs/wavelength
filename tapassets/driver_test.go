package tapassets

import (
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// TestCommitResultProjection checks the fields read by tree construction.
func TestCommitResultProjection(t *testing.T) {
	t.Parallel()

	assetRef := tapsdk.AssetRefFromAssetID(tapsdk.AssetID{0x01})
	firstOutpoint := tapsdk.Outpoint{Txid: tapsdk.Hash{0x02}, Index: 1}
	secondOutpoint := tapsdk.Outpoint{Txid: tapsdk.Hash{0x03}, Index: 2}
	transfer := &tapsdk.CustomAnchorTransferPackage{
		AnchorPsbt: []byte{
			0x04,
		},
		Funding: tapsdk.CustomAnchorFundingSummary{
			Mode: tapsdk.
				CustomAnchorFundingCallerFundedExact,
			ActualFeeSat: 5,
		},
		Inputs: []tapsdk.CustomAnchorAssetInputSummary{{
			LogicalInputID:    "input-0",
			PacketIndex:       3,
			PacketRole:        tapsdk.CustomAnchorPacketRoleActive,
			VirtualInputIndex: 4,
			AnchorOutpoint:    firstOutpoint,
			AssetRef:          assetRef,
			IssuanceID: tapsdk.AssetID{
				0x05,
			},
			Amount: 10,
			ProofSource: tapsdk.CustomAnchorProofSourceSummary{
				Kind: tapsdk.
					CustomAnchorProofSourceConfirmedFile,
				Blob: []byte{
					0x0D,
				},
			},
		}},
		Outputs: []tapsdk.CustomAnchorAssetOutputSummary{
			{
				LogicalOutputID: "output-0",
				PacketRole: tapsdk.
					CustomAnchorPacketRoleActive,
				PacketIndex:        0,
				VirtualOutputIndex: 0,
				AnchorOutputIndex:  1,
				AnchorOutpoint:     firstOutpoint,
				AnchorValueSat:     330,
				AssetRef:           assetRef,
				IssuanceID: tapsdk.AssetID{
					0x05,
				},
				Amount: 4,
				TaprootAssetRoot: tapsdk.Hash{
					0x06,
				},
				TaprootMerkleRoot: tapsdk.Hash{
					0x07,
				},
				ScriptMode: tapsdk.CustomAssetScriptOPTrue,
				OPTrueSpend: &tapsdk.CustomAssetOPTrueSpendInfo{
					LeafScript: []byte{
						0x51,
					},
					ControlBlock: []byte{
						0x08,
					},
				},
			},
			{
				LogicalOutputID: "output-1",
				PacketRole: tapsdk.
					CustomAnchorPacketRoleActive,
				PacketIndex:        1,
				VirtualOutputIndex: 0,
				AnchorOutputIndex:  2,
				AnchorOutpoint:     secondOutpoint,
				AssetRef:           assetRef,
				IssuanceID: tapsdk.AssetID{
					0x09,
				},
				Amount: 6,
			},
		},
		ProofUpdates: []tapsdk.CustomAnchorProofUpdate{
			{
				PacketRole: tapsdk.
					CustomAnchorPacketRoleActive,
				PacketIndex:        1,
				VirtualOutputIndex: 0,
				ProofBlob: []byte{
					0x0A,
				},
			},
			{
				PacketRole: tapsdk.
					CustomAnchorPacketRoleActive,
				PacketIndex:        0,
				VirtualOutputIndex: 0,
				ProofBlob: []byte{
					0x0B,
				},
			},
		},
	}

	packageBytes := []byte{0x0C}
	result, err := commitResultFromValidatedPackage(
		transfer, packageBytes,
	)
	require.NoError(t, err)
	require.Equal(t, packageBytes, result.packageBytes)
	require.Equal(t, transfer.AnchorPsbt, result.anchorPSBT)
	require.Equal(t, transfer.Funding.Mode, result.fundingMode)
	require.Equal(t, transfer.Funding.ActualFeeSat, result.actualFeeSat)
	require.Len(t, result.inputs, 1)
	require.Equal(t, "input-0", result.inputs[0].logicalInputID)
	require.Equal(t, uint32(3), result.inputs[0].packetIndex)
	require.Equal(
		t, tapsdk.CustomAnchorPacketRoleActive,
		result.inputs[0].packetRole,
	)
	require.Equal(t, uint32(4), result.inputs[0].virtualInputIndex)
	require.Equal(t, tapsdk.AssetID{0x05}, result.inputs[0].issuanceID)
	require.Equal(
		t, tapsdk.CustomAnchorProofSourceConfirmedFile,
		result.inputs[0].proofSource.kind,
	)
	require.Equal(t, []byte{0x0D}, result.inputs[0].proofSource.blob)
	require.Len(t, result.outputs, 2)
	require.Equal(t, "output-0", result.outputs[0].logicalOutputID)
	require.Equal(t, []byte{0x0B}, result.outputs[0].proofBlob)
	require.Equal(t, []byte{0x0A}, result.outputs[1].proofBlob)
	require.Equal(
		t, [][]byte{{0x51}, {0x08}}, result.outputs[0].opTrueWitness,
	)

	packageBytes[0]++
	transfer.AnchorPsbt[0]++
	transfer.Inputs[0].ProofSource.Blob[0]++
	transfer.ProofUpdates[1].ProofBlob[0]++
	require.Equal(t, []byte{0x0C}, result.packageBytes)
	require.Equal(t, []byte{0x04}, result.anchorPSBT)
	require.Equal(t, []byte{0x0D}, result.inputs[0].proofSource.blob)
	require.Equal(t, []byte{0x0B}, result.outputs[0].proofBlob)
}

// TestCommitResultProjectionRequiresProof checks missing proof updates.
func TestCommitResultProjectionRequiresProof(t *testing.T) {
	t.Parallel()

	_, err := commitResultFromValidatedPackage(
		&tapsdk.CustomAnchorTransferPackage{
			Outputs: []tapsdk.CustomAnchorAssetOutputSummary{{
				PacketRole: tapsdk.CustomAnchorPacketRoleActive,
			}},
		}, nil,
	)
	require.ErrorContains(t, err, "has no proof update")
}
