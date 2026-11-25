package tapassets

import (
	"testing"

	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/stretchr/testify/require"
)

// TestClaimInputsFromCommit checks that a claim takes one input per asset in
// the leaf, each extending its input proof with the leaf transition, and is
// refused for a leaf whose assets are not OP_TRUE locked.
func TestClaimInputsFromCommit(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	output := f.committed.outputs[0]
	input := commitInput{
		packetRole:  output.packetRole,
		packetIndex: output.packetIndex,
		issuanceID:  output.issuanceID,
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

	otherIssuance := withVirtualIndex(input, 1)
	otherIssuance.issuanceID[0] ^= 1

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
		name: "input indexes with a gap",
		outputs: []commitOutput{
			output,
		},
		inputs: []commitInput{
			input,
			withVirtualIndex(input, 2),
		},
		leaf: true,
		err:  "not contiguous",
	}, {
		name: "merged input of another issuance",
		outputs: []commitOutput{
			output,
		},
		inputs: []commitInput{
			input,
			otherIssuance,
		},
		leaf: true,
		err:  "has another issuance",
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

// TestClaimInputsFromMergedCommit checks that a leaf transition merging
// several inputs of one issuance extends the path of its first virtual input
// and carries the others as co-input paths, whatever order the package lists
// them in.
func TestClaimInputsFromMergedCommit(t *testing.T) {
	t.Parallel()

	f := newAssetVTXOFixture(t)
	output := f.committed.outputs[0]
	inputs := make([]commitInput, 3)
	for idx := range inputs {
		inputs[idx] = commitInput{
			packetRole:        output.packetRole,
			packetIndex:       output.packetIndex,
			virtualInputIndex: uint32(idx),
			issuanceID:        output.issuanceID,
			proofSource: commitProofSource{
				kind: customanchor.ProofSourceConfirmedFile,
				blob: []byte{
					byte(idx),
				},
			},
		}
	}

	committed := cloneCommit(f.committed)
	committed.outputs = []commitOutput{output}
	committed.inputs = []commitInput{inputs[2], inputs[0], inputs[1]}
	claimInputs, err := claimInputsFromCommit(committed, f.leafOutpoint)
	require.NoError(t, err)
	require.Len(t, claimInputs, 1)

	path := claimInputs[0].ProofPath
	require.Equal(t, []byte{0}, path.ConfirmedBaseProof)
	require.Len(t, path.Steps, 1)
	require.Equal(t, output.proofBlob, path.Steps[0].TransitionProof)
	coPaths := path.Steps[0].CoInputPaths
	require.Len(t, coPaths, 2)
	require.Equal(t, []byte{1}, coPaths[0].ConfirmedBaseProof)
	require.Equal(t, []byte{2}, coPaths[1].ConfirmedBaseProof)
}

// withVirtualIndex returns input at another index of its virtual packet.
func withVirtualIndex(input commitInput, index uint32) commitInput {
	input.virtualInputIndex = index

	return input
}
