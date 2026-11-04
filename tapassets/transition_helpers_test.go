package tapassets

import (
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestAnchorPlanPolicyLayout verifies that an anchor plan is only built for
// leaves that tap-sdk assembles into the same root as their Ark policy.
func TestAnchorPlanPolicyLayout(t *testing.T) {
	t.Parallel()

	leaves := func(scripts ...byte) []txscript.TapLeaf {
		result := make([]txscript.TapLeaf, len(scripts))
		for idx, script := range scripts {
			result[idx] = txscript.NewBaseTapLeaf([]byte{script})
		}

		return result
	}

	tests := []struct {
		name    string
		leaves  []txscript.TapLeaf
		wantErr bool
	}{
		{
			name:   "one leaf",
			leaves: leaves(1),
		},
		{
			name:   "two leaves",
			leaves: leaves(2, 1),
		},
		{
			name:    "three leaves",
			leaves:  leaves(1, 2, 3),
			wantErr: true,
		},
		{
			name:   "four leaves",
			leaves: leaves(1, 2, 3, 4),
		},
		{
			name:    "four leaves out of order",
			leaves:  leaves(1, 3, 2, 4),
			wantErr: true,
		},
		{
			name:    "six leaves",
			leaves:  leaves(1, 2, 3, 4, 5, 6),
			wantErr: true,
		},
		{
			name:   "eight leaves",
			leaves: leaves(1, 2, 3, 4, 5, 6, 7, 8),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, err := anchorPlan(
				&arkscript.ARKNUMSKey, test.leaves,
			)
			if test.wantErr {
				require.ErrorContains(
					t, err, "do not assemble into "+
						"their policy tree",
				)

				return
			}
			require.NoError(t, err)

			policyLeaves := make(
				[]arkscript.PolicyLeaf, len(test.leaves),
			)
			for idx := range test.leaves {
				policyLeaves[idx].Leaf = test.leaves[idx]
			}
			policy, err := arkscript.BuildTree(
				policyLeaves, &arkscript.ARKNUMSKey,
			)
			require.NoError(t, err)

			planLeaves := make(
				[]txscript.TapLeaf,
				len(plan.Tapscript.TapLeaves),
			)
			for idx, leaf := range plan.Tapscript.TapLeaves {
				planLeaves[idx] = txscript.NewBaseTapLeaf(
					leaf.Script,
				)
			}
			root := txscript.AssembleTaprootScriptTree(
				planLeaves...,
			).RootNode.TapHash()
			require.Equal(t, policy.RootHash, root[:])
		})
	}
}
