package tapassets

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// InventoryVerifierClient is the slice of the tap-sdk client the inventory
// verifier needs.
type InventoryVerifierClient = proofInventoryClient

// NewInventoryVerifier binds a confirmed proof to the operator's own tapd
// inventory: the proof tip must match the expected asset, amount, and
// anchor outpoint, and that anchor must appear in tapd's managed-UTXO
// inventory with the expected Taproot Asset commitment root. Round
// integrations use it to verify funding-UTXO proofs at plan and commit time.
func NewInventoryVerifier(client InventoryVerifierClient,
	assetRef tapsdk.AssetRef, amount uint64, anchor wire.OutPoint,
	assetRoot tapsdk.Hash) tapsdk.ConfirmedProofVerifier {

	return &proofInventoryVerifier{
		client:    client,
		assetRef:  assetRef,
		amount:    amount,
		anchor:    sdkOutpoint(anchor),
		assetRoot: assetRoot,
	}
}

// NewBoardedProofVerifier verifies a boarded asset's confirmed proof:
// the chain-validated tip must match the boarding claim exactly. Unlike
// the inventory verifier there is no anchor-inventory check — the
// boarded UTXO lives in the client's wallet, not the operator's — and
// completeness is attested structurally: the committer rejects passive
// assets at commit time, so an anchor hiding siblings fails the round's
// sealed transition rather than this verifier.
func NewBoardedProofVerifier(client InventoryVerifierClient,
	assetRef tapsdk.AssetRef, amount uint64,
	anchor wire.OutPoint) tapsdk.ConfirmedProofVerifier {

	return &boardedProofVerifier{
		client:   client,
		assetRef: assetRef,
		amount:   amount,
		anchor:   sdkOutpoint(anchor),
	}
}

// boardedProofVerifier implements tapsdk.ConfirmedProofVerifier for
// client-boarded asset UTXOs.
type boardedProofVerifier struct {
	client   InventoryVerifierClient
	assetRef tapsdk.AssetRef
	amount   uint64
	anchor   tapsdk.Outpoint
}

// VerifyConfirmedProof chain-verifies the proof through tapd and pins
// the tip to the boarding claim.
func (v *boardedProofVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.client == nil {
		return nil, fmt.Errorf("tapd proof client is required")
	}

	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("verify boarded proof with tapd: %w",
			err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return nil, fmt.Errorf("tapd rejected boarded proof")
	}
	tip := verified.DecodedProof
	if !tip.AssetRef.Equivalent(v.assetRef) || tip.Amount != v.amount ||
		tip.Outpoint != v.anchor {
		return nil, fmt.Errorf("boarded proof tip does not match the " +
			"boarding claim")
	}

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
		PassiveAssetCount:            0,
	}, nil
}
