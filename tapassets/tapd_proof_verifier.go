package tapassets

import (
	"context"
	"errors"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

// tapdProofClient is the slice of the tap-sdk client that verifies proof
// files through tapd.
type tapdProofClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	UnpackProofFile(context.Context, []byte) ([][]byte, error)

	DecodeProof(context.Context, []byte) (*tapsdk.DecodedProof, error)

	InsertProof(context.Context, []byte, *tapsdk.DecodedProof) error

	ListFederationServers(context.Context) (
		[]tapsdk.FederationServer,
		error,
	)

	SyncUniverse(context.Context,
		*tapsdk.SyncRequest) ([]tapsdk.SyncedUniverse, error)
}

// TapdProofChainVerifier verifies proof chains through a tapd node.
type TapdProofChainVerifier struct {
	client tapdProofClient
}

// NewTapdProofChainVerifier returns a proof chain verifier backed by the tapd
// node behind client.
func NewTapdProofChainVerifier(client tapsdk.Client) (*TapdProofChainVerifier,
	error) {

	if client == nil {
		return nil, fmt.Errorf("tapd client is required")
	}

	return &TapdProofChainVerifier{client: client}, nil
}

// VerifyProofChain verifies the proof file through tapd. tapd rejects a
// grouped asset whose group it does not know, and fails to describe a valid
// proof of an asset it has not stored. A rejected or failed file therefore
// gets its issuance proof inserted into tapd's universe and is verified
// again. Only that fallback writes to tapd.
func (v *TapdProofChainVerifier) VerifyProofChain(ctx context.Context,
	proofFile []byte) error {

	if v == nil || v.client == nil {
		return fmt.Errorf("tapd client is required")
	}

	valid, err := v.verify(ctx, proofFile)
	if valid {
		return nil
	}

	// A cancelled caller gets its cancellation back, without writes.
	if err != nil && ctx.Err() != nil {
		return err
	}

	// The first error is kept, so a transport failure still surfaces when
	// the insert fails too.
	if insertErr := v.insertIssuance(ctx, proofFile); insertErr != nil {
		return errors.Join(err, insertErr)
	}

	valid, err = v.verify(ctx, proofFile)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("tapd rejected the proof file")
	}

	return nil
}

// verify reports whether tapd accepts the proof file. tapd answers a failed
// verification with an invalid result, but a valid proof of an asset it has
// not stored with an error, so the caller retries after either once the
// issuance is inserted.
func (v *TapdProofChainVerifier) verify(ctx context.Context, proofFile []byte) (
	bool, error) {

	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return false, fmt.Errorf("verify proof with tapd: %w", err)
	}

	return verified != nil && verified.Valid, nil
}

// insertIssuance inserts the issuance proof of the file into tapd's universe.
// A group anchor issuance teaches tapd its group. A reissuance only inserts
// once tapd knows the group, so the group's issuances are synced from tapd's
// federation servers first.
func (v *TapdProofChainVerifier) insertIssuance(ctx context.Context,
	proofFile []byte) error {

	proofs, err := v.client.UnpackProofFile(ctx, proofFile)
	if err != nil {
		return fmt.Errorf("unpack proof file: %w", err)
	}
	if len(proofs) == 0 || len(proofs[0]) == 0 {
		return fmt.Errorf("proof file has no issuance proof")
	}
	issuance, err := v.client.DecodeProof(ctx, proofs[0])
	if err != nil {
		return fmt.Errorf("decode issuance proof: %w", err)
	}
	if issuance == nil || !issuance.IsIssuance {
		return fmt.Errorf("first proof is not an issuance")
	}

	err = v.client.InsertProof(ctx, proofs[0], issuance)
	if err == nil {
		return nil
	}
	if !issuance.AssetRef.IsGroupRef() {
		return fmt.Errorf("insert issuance proof: %w", err)
	}
	errs := []error{err}

	servers, err := v.client.ListFederationServers(ctx)
	if err != nil {
		return fmt.Errorf("list federation servers: %w", err)
	}
	req := &tapsdk.SyncRequest{
		SyncMode: tapsdk.SyncIssuanceOnly,
		SyncTargets: []tapsdk.SyncTarget{{
			ID: tapsdk.UniverseIDFromRef(
				issuance.AssetRef, tapsdk.ProofTypeIssuance,
			),
		}},
	}
	for _, server := range servers {
		req.UniverseHost = server.Host
		_, err := v.client.SyncUniverse(ctx, req)
		if err == nil {
			err = v.client.InsertProof(ctx, proofs[0], issuance)
		}
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", server.Host, err))
	}

	return fmt.Errorf("tapd does not know asset group %s and no "+
		"federation server provided it: %w", issuance.AssetRef,
		errors.Join(errs...))
}
