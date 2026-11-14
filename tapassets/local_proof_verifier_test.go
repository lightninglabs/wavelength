package tapassets

import (
	"context"
	"errors"
	"testing"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// fakeProofVerifierClient answers VerifyProof with a scripted result.
type fakeProofVerifierClient struct {
	resp *tapsdk.VerifyProofResponse
	err  error
}

// VerifyProof returns the scripted verification result.
func (f *fakeProofVerifierClient) VerifyProof(context.Context, []byte) (
	*tapsdk.VerifyProofResponse, error) {

	return f.resp, f.err
}

// TestLocalProofChainVerifier accepts a proof file only when the backend
// reports it valid.
func TestLocalProofChainVerifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		client  *fakeProofVerifierClient
		wantErr string
	}{
		{
			name: "valid proof",
			client: &fakeProofVerifierClient{
				resp: &tapsdk.VerifyProofResponse{
					Valid: true,
				},
			},
		},
		{
			name: "invalid proof",
			client: &fakeProofVerifierClient{
				resp: &tapsdk.VerifyProofResponse{},
			},
			wantErr: "rejected the proof file",
		},
		{
			name:    "empty response",
			client:  &fakeProofVerifierClient{},
			wantErr: "rejected the proof file",
		},
		{
			name: "backend failure",
			client: &fakeProofVerifierClient{
				err: errors.New("chain backend down"),
			},
			wantErr: "chain backend down",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			verifier, err := NewLocalProofChainVerifier(tc.client)
			require.NoError(t, err)

			err = verifier.VerifyProofChain(t.Context(), []byte{1})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
		})
	}

	_, err := NewLocalProofChainVerifier(nil)
	require.Error(t, err)
}
