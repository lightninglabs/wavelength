package tapassets

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestAnchorPlanCommitsToPolicyRoot verifies that the anchor sibling hashes
// to the Ark policy root for every leaf count. Assembling the same leaves by
// pairing them diverges from the policy root whenever the leaf count is not a
// power of two, which would leave the composed output unspendable through the
// policy's control blocks.
func TestAnchorPlanCommitsToPolicyRoot(t *testing.T) {
	t.Parallel()

	for n := 1; n <= 8; n++ {
		leaves := make([]arkscript.PolicyLeaf, n)
		tapLeaves := make([]txscript.TapLeaf, n)
		for i := range leaves {
			tapLeaves[i] = txscript.NewBaseTapLeaf(
				[]byte{byte(i + 1)},
			)
			leaves[i] = arkscript.PolicyLeaf{Leaf: tapLeaves[i]}
		}
		policy, err := arkscript.BuildTree(
			leaves, &arkscript.ARKNUMSKey,
		)
		require.NoError(t, err)

		plan, err := anchorPlan(policy.InternalKey, tapLeaves)
		require.NoError(t, err)
		require.NoError(t, plan.Validate())

		var sibling chainhash.Hash
		switch branch := plan.Tapscript.TapBranch; {
		case branch != nil:
			sibling = tapBranchHash(
				branch.LeftTapHash[:], branch.RightTapHash[:],
			)

		default:
			sdkLeaves := make(
				[]txscript.TapLeaf,
				len(plan.Tapscript.TapLeaves),
			)
			for i, leaf := range plan.Tapscript.TapLeaves {
				sdkLeaves[i] = txscript.NewBaseTapLeaf(
					leaf.Script,
				)
			}
			sibling = txscript.AssembleTaprootScriptTree(
				sdkLeaves...,
			).RootNode.TapHash()
		}
		require.Equal(t, policy.RootHash, sibling[:], "leaves=%d", n)
	}
}
