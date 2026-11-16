package tapassets

import (
	"context"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

// proofVerifierClient is the slice of the tap-sdk client that verifies proof
// files.
type proofVerifierClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)
}

// LocalProofChainVerifier verifies proof chains through a backend that
// decides on its own chain view and trusted asset groups, such as tap-sdk's
// embedded engine. It never writes to the backend.
type LocalProofChainVerifier struct {
	client proofVerifierClient
}

// NewLocalProofChainVerifier returns a proof chain verifier backed by
// client.
func NewLocalProofChainVerifier(client proofVerifierClient) (
	*LocalProofChainVerifier, error) {

	if client == nil {
		return nil, fmt.Errorf("asset backend client is required")
	}

	return &LocalProofChainVerifier{client: client}, nil
}

// VerifyProofChain returns an error unless the backend accepts the proof
// file.
func (v *LocalProofChainVerifier) VerifyProofChain(ctx context.Context,
	proofFile []byte) error {

	if v == nil || v.client == nil {
		return fmt.Errorf("asset backend client is required")
	}

	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return fmt.Errorf("verify proof: %w", err)
	}
	if verified == nil || !verified.Valid {
		return fmt.Errorf("asset backend rejected the proof file")
	}

	return nil
}
