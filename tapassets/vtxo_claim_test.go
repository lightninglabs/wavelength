package tapassets

import (
	"testing"

	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/stretchr/testify/require"
)

// TestClaimInputsFromCommit checks that a claim takes one input per asset in
// the leaf, each extending its input proof with the leaf transition, and is
// refused for a leaf whose assets are not OP_TRUE locked or merge inputs.
func TestClaimInputsFromCommit(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	output := f.committed.outputs[0]
	input := commitInput{
		packetRole:  output.packetRole,
		packetIndex: output.packetIndex,
		proofSource: commitProofSource{
			kind: customanchor.ProofSourceConfirmedFile,
			blob: []byte("deposit proof"),
		},
	}

	// A second issuance of the asset lands in the same leaf from its own
	// virtual packet.
	reissued := output
	reissued.packetIndex++
	reissued.amount = 75
	reissued.proofBlob = []byte("reissued transition")
	reissuedInput := input
	reissuedInput.packetIndex = reissued.packetIndex
	reissuedInput.proofSource.blob = []byte("reissued deposit proof")

	committed := cloneCommit(f.committed)
	committed.outputs = []commitOutput{output, reissued}
	committed.inputs = []commitInput{input, reissuedInput}
	inputs, err := claimInputsFromCommit(committed, f.leafOutpoint)
	require.NoError(t, err)
	require.Len(t, inputs, 2)
	for idx, want := range []struct {
		output commitOutput
		base   string
	}{
		{
			output: output,
			base:   "deposit proof",
		},
		{
			output: reissued,
			base:   "reissued deposit proof",
		},
	} {
		got := inputs[idx]
		require.Equal(t, want.output.amount, got.Amount)
		require.Equal(
			t, []byte(want.base), got.ProofPath.ConfirmedBaseProof,
		)
		require.Len(t, got.ProofPath.Steps, 1)
		require.Equal(
			t, want.output.proofBlob,
			got.ProofPath.Steps[0].TransitionProof,
		)
		require.Equal(
			t, want.output.opTrueWitness, got.OPTrueWitness,
		)
	}

	otherLeaf := f.leafOutpoint
	otherLeaf.Index++
	walletOutput := output
	walletOutput.scriptMode = customanchor.AssetScriptWallet

	testCases := []struct {
		name    string
		outputs []commitOutput
		inputs  []commitInput
		leaf    bool
		err     string
	}{{
		name: "no asset in the leaf",
		outputs: []commitOutput{
			output,
		},
		inputs: []commitInput{
			input,
		},
		err: "holds no asset",
	}, {
		name: "asset not OP_TRUE locked",
		outputs: []commitOutput{
			output,
			walletOutput,
		},
		inputs: []commitInput{
			input,
		},
		leaf: true,
		err:  "not OP_TRUE locked",
	}, {
		name: "no input",
		outputs: []commitOutput{
			output,
			reissued,
		},
		inputs: []commitInput{
			input,
		},
		leaf: true,
		err:  "has no asset input",
	}, {
		name: "merged inputs",
		outputs: []commitOutput{
			output,
		},
		inputs: []commitInput{
			input,
			input,
		},
		leaf: true,
		err:  "merges asset inputs",
	}}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			committed := cloneCommit(f.committed)
			committed.outputs = testCase.outputs
			committed.inputs = testCase.inputs
			leaf := otherLeaf
			if testCase.leaf {
				leaf = f.leafOutpoint
			}
			_, err := claimInputsFromCommit(committed, leaf)
			require.ErrorContains(t, err, testCase.err)
		})
	}
}
