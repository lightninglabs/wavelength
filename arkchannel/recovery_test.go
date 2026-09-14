package arkchannel

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestRecoveryPackageClone preserves every value while isolating all mutable
// transport slices from the original package.
func TestRecoveryPackageClone(t *testing.T) {
	original := RecoveryPackage{
		Descriptor: RecoveryDescriptor{
			Ancestry: []RecoveryAncestry{{
				TreePath: []byte{
					1,
				}, CommitmentTxID: chainhash.Hash{
					2,
				},
				InputIndices: []uint32{
					3,
				},
			}},
		},
		Packages: []RecoveryOORPackage{{
			SessionID: chainhash.Hash{
				4,
			}, ArkPSBT: []byte{
				5,
			},
			Checkpoints: [][]byte{
				{
					6,
				},
			},
		}},
	}

	clone := original.Clone()
	require.Equal(t, original, clone)
	clone.Descriptor.Ancestry[0].TreePath[0] = 7
	clone.Descriptor.Ancestry[0].InputIndices[0] = 8
	clone.Packages[0].ArkPSBT[0] = 9
	clone.Packages[0].Checkpoints[0][0] = 10
	require.Equal(t, byte(1), original.Descriptor.Ancestry[0].TreePath[0])
	require.Equal(
		t, uint32(3), original.Descriptor.Ancestry[0].InputIndices[0],
	)
	require.Equal(t, byte(5), original.Packages[0].ArkPSBT[0])
	require.Equal(t, byte(6), original.Packages[0].Checkpoints[0][0])
}
