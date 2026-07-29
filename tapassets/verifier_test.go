package tapassets

import (
	"context"
	"errors"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// TestProofLineageVerifierBootstrapsIssuance proves a fresh receiver teaches
// tapd the public issuance before asking it to verify the complete base chain.
func TestProofLineageVerifierBootstrapsIssuance(t *testing.T) {
	t.Parallel()

	client := &fakeLineageClient{
		rawProofs: [][]byte{
			[]byte("issuance"),
			[]byte("sender-transfer"),
		},
		decoded: &tapsdk.DecodedProof{
			IsIssuance: true,
		},
		verification: &tapsdk.VerifyProofResponse{
			Valid:        true,
			DecodedProof: &tapsdk.DecodedProof{},
		},
	}
	verifier := &proofLineageVerifier{client: client}

	result, err := verifier.VerifyConfirmedProof(
		t.Context(), []byte("proof-file"),
	)
	require.NoError(t, err)
	require.True(t, result.AnchorAssetInventoryComplete)
	require.Zero(t, result.PassiveAssetCount)
	require.Equal(
		t, []string{"unpack", "decode", "insert", "verify"},
		client.calls,
	)
	require.Equal(t, [][]byte{[]byte("issuance")}, client.inserted)
}

// TestProofLineageVerifierRejectsInvalidIssuanceBootstrap proves malformed or
// unavailable issuance data fails before tapd verifies the complete chain.
func TestProofLineageVerifierRejectsInvalidIssuanceBootstrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		client    *fakeLineageClient
		wantErr   string
		wantCalls []string
	}{
		{
			name: "unpack failure",
			client: &fakeLineageClient{
				unpackErr: errors.New("unavailable"),
			},
			wantErr: "unpack proof file: unavailable",
			wantCalls: []string{
				"unpack",
			},
		},
		{
			name: "empty proof file",
			client: &fakeLineageClient{
				rawProofsSet: true,
			},
			wantErr: "proof file contains no proofs",
			wantCalls: []string{
				"unpack",
			},
		},
		{
			name: "empty issuance proof",
			client: &fakeLineageClient{
				rawProofs: [][]byte{
					nil,
				},
			},
			wantErr: "issuance proof is empty",
			wantCalls: []string{
				"unpack",
			},
		},
		{
			name: "decode failure",
			client: &fakeLineageClient{
				rawProofs: [][]byte{
					[]byte("issuance"),
				},
				decodeErr: errors.New("malformed"),
			},
			wantErr: "decode issuance proof: malformed",
			wantCalls: []string{
				"unpack",
				"decode",
			},
		},
		{
			name: "empty decoded proof",
			client: &fakeLineageClient{
				rawProofs: [][]byte{
					[]byte("issuance"),
				},
			},
			wantErr: "decoded issuance proof is empty",
			wantCalls: []string{
				"unpack",
				"decode",
			},
		},
		{
			name: "first proof is transfer",
			client: &fakeLineageClient{
				rawProofs: [][]byte{
					[]byte("transfer"),
				},
				decoded: &tapsdk.DecodedProof{},
			},
			wantErr: "first proof is not an issuance",
			wantCalls: []string{
				"unpack",
				"decode",
			},
		},
		{
			name: "insert failure",
			client: &fakeLineageClient{
				rawProofs: [][]byte{
					[]byte("issuance"),
				},
				decoded: &tapsdk.DecodedProof{
					IsIssuance: true,
				},
				insertErr: errors.New("rejected"),
			},
			wantErr: "insert issuance proof: rejected",
			wantCalls: []string{
				"unpack", "decode", "insert",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			verifier := &proofLineageVerifier{
				client: test.client,
			}
			_, err := verifier.VerifyConfirmedProof(
				t.Context(), []byte("proof-file"),
			)
			require.ErrorContains(t, err, test.wantErr)
			require.Equal(t, test.wantCalls, test.client.calls)
		})
	}
}

type fakeLineageClient struct {
	rawProofs    [][]byte
	rawProofsSet bool
	decoded      *tapsdk.DecodedProof
	verification *tapsdk.VerifyProofResponse
	unpackErr    error
	decodeErr    error
	insertErr    error

	calls    []string
	inserted [][]byte
}

func (f *fakeLineageClient) UnpackProofFile(context.Context, []byte) ([][]byte,
	error) {

	f.calls = append(f.calls, "unpack")
	if f.unpackErr != nil {
		return nil, f.unpackErr
	}
	if f.rawProofsSet {
		return nil, nil
	}

	return f.rawProofs, nil
}

func (f *fakeLineageClient) DecodeProof(context.Context, []byte) (
	*tapsdk.DecodedProof, error) {

	f.calls = append(f.calls, "decode")
	if f.decodeErr != nil {
		return nil, f.decodeErr
	}

	return f.decoded, nil
}

func (f *fakeLineageClient) InsertProof(_ context.Context, rawProof []byte,
	_ *tapsdk.DecodedProof) error {

	f.calls = append(f.calls, "insert")
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(
		f.inserted, append([]byte(nil), rawProof...),
	)

	return nil
}

func (f *fakeLineageClient) VerifyProof(context.Context, []byte) (
	*tapsdk.VerifyProofResponse, error) {

	f.calls = append(f.calls, "verify")

	return f.verification, nil
}
