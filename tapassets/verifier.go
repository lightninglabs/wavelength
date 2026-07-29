package tapassets

import (
	"bytes"
	"context"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

type proofInventoryClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	ListUtxos(context.Context,
		*tapsdk.ListUtxosRequest) (
		map[string]*tapsdk.ManagedUtxo,
		error,
	)
}

// proofLineageClient adds the public Universe operations needed by a fresh
// receiver to learn the issuance that anchors a grouped asset proof. It
// deliberately omits wallet transfer registration because Wavelength's
// unconfirmed OP_TRUE asset is not owned by the receiver's tapd wallet.
type proofLineageClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	UnpackProofFile(context.Context, []byte) ([][]byte, error)

	DecodeProof(context.Context, []byte) (*tapsdk.DecodedProof, error)

	InsertProof(context.Context, []byte, *tapsdk.DecodedProof) error
}

type expectedUnconfirmedAnchor struct {
	stepIndex        uint16
	previousOutpoint tapsdk.Outpoint
	anchorOutpoint   tapsdk.Outpoint
	transaction      []byte
}

type proofInventoryVerifier struct {
	client      proofInventoryClient
	assetRef    tapsdk.AssetRef
	amount      uint64
	anchor      tapsdk.Outpoint
	assetRoot   tapsdk.Hash
	unconfirmed *expectedUnconfirmedAnchor
}

// VerifyConfirmedProof asks tapd to verify the proof chain and then binds its
// tip to tapd's complete managed-anchor inventory. Compact unconfirmed paths
// are only safe when that confirmed anchor contains no passive assets.
func (v *proofInventoryVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.client == nil {
		return nil, fmt.Errorf("tapd proof inventory client is " +
			"required")
	}

	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("verify confirmed proof with tapd: %w",
			err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return nil, fmt.Errorf("tapd rejected confirmed proof")
	}
	tip := verified.DecodedProof
	if !tip.AssetRef.Equivalent(v.assetRef) || tip.Amount != v.amount ||
		tip.Outpoint != v.anchor {
		return nil, fmt.Errorf("confirmed proof tip does not match " +
			"OOR input")
	}

	utxos, err := v.client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		IncludeLeased: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list tapd anchor inventory: %w", err)
	}
	var anchor *tapsdk.ManagedUtxo
	for _, candidate := range utxos {
		if candidate != nil && candidate.OutPoint == v.anchor {
			anchor = candidate
			break
		}
	}
	if anchor == nil {
		return nil, fmt.Errorf("confirmed proof anchor is not " +
			"managed by tapd")
	}
	if anchor.TaprootAssetRoot != v.assetRoot {
		return nil, fmt.Errorf("tapd asset root does not match " +
			"Wavelength VTXO")
	}
	if len(anchor.Assets) == 0 {
		return nil, fmt.Errorf("tapd anchor inventory is empty")
	}

	var selected int
	for _, asset := range anchor.Assets {
		if asset == nil {
			continue
		}
		if asset.Genesis.IssuanceID == tip.IssuanceID &&
			asset.Amount == tip.Amount &&
			asset.ScriptKey.PubKey == tip.ScriptKey {

			selected++
		}
	}
	if selected != 1 {
		return nil, fmt.Errorf("tapd anchor inventory matched "+
			"selected asset %d times", selected)
	}

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
		PassiveAssetCount:            uint32(len(anchor.Assets) - 1),
	}, nil
}

// VerifyUnconfirmedAnchor binds the compact proof step to the exact committed
// checkpoint transaction that Wavelength will later submit and sign.
func (v *proofInventoryVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	if v == nil || v.unconfirmed == nil {
		return fmt.Errorf("unconfirmed Wavelength anchor is not " +
			"configured")
	}
	expected := v.unconfirmed
	if transition.StepIndex != expected.stepIndex {
		return fmt.Errorf("unexpected unconfirmed proof step %d",
			transition.StepIndex)
	}
	if transition.PreviousAnchorOutpoint != expected.previousOutpoint {
		return fmt.Errorf("unconfirmed proof previous outpoint " +
			"mismatch")
	}
	if transition.AnchorOutpoint != expected.anchorOutpoint {
		return fmt.Errorf("unconfirmed proof anchor outpoint mismatch")
	}
	if !bytes.Equal(transition.AnchorTransaction, expected.transaction) {
		return fmt.Errorf("unconfirmed proof anchor transaction " +
			"mismatch")
	}

	return nil
}

// proofLineageVerifier verifies a chained path's confirmed base with tapd and
// delegates cryptographic validation of every sealed unconfirmed step to
// tap-sdk. The exact package that created the local VTXO is the authority for
// passive isolation; a receiver must not need the sender's base anchor in its
// own ListUtxos inventory.
type proofLineageVerifier struct {
	client       proofLineageClient
	expectedLast *expectedUnconfirmedAnchor
}

// VerifyConfirmedProof verifies the confirmed base through tapd. Complete
// passive isolation is inherited from the exact, operator-accepted package
// lineage that supplied the compact path.
func (v *proofLineageVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	if v == nil || v.client == nil {
		return nil, fmt.Errorf("tapd proof client is required")
	}
	if err := bootstrapProofIssuance(ctx, v.client, proofFile); err != nil {
		return nil, fmt.Errorf("bootstrap chained proof issuance: %w",
			err)
	}
	verified, err := v.client.VerifyProof(ctx, proofFile)
	if err != nil {
		return nil, fmt.Errorf("verify chained proof base with "+
			"tapd: %w", err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return nil, fmt.Errorf("tapd rejected chained proof base")
	}

	return &tapsdk.ConfirmedProofVerification{
		AnchorAssetInventoryComplete: true,
		PassiveAssetCount:            0,
	}, nil
}

// bootstrapProofIssuance teaches a fresh receiver's local Universe about the
// public issuance at the start of the received proof chain. InsertProof
// validates and idempotently persists that issuance. Importing or registering
// the later transfer proofs would incorrectly claim the sender's asset in the
// receiver's tapd wallet.
func bootstrapProofIssuance(ctx context.Context, client proofLineageClient,
	proofFile []byte) error {

	rawProofs, err := client.UnpackProofFile(ctx, proofFile)
	if err != nil {
		return fmt.Errorf("unpack proof file: %w", err)
	}
	if len(rawProofs) == 0 {
		return fmt.Errorf("proof file contains no proofs")
	}
	if len(rawProofs[0]) == 0 {
		return fmt.Errorf("issuance proof is empty")
	}

	issuance, err := client.DecodeProof(ctx, rawProofs[0])
	if err != nil {
		return fmt.Errorf("decode issuance proof: %w", err)
	}
	if issuance == nil {
		return fmt.Errorf("decoded issuance proof is empty")
	}
	if !issuance.IsIssuance {
		return fmt.Errorf("first proof is not an issuance")
	}
	if err := client.InsertProof(ctx, rawProofs[0], issuance); err != nil {
		return fmt.Errorf("insert issuance proof: %w", err)
	}

	return nil
}

// VerifyUnconfirmedAnchor accepts sealed historical steps after tap-sdk has
// verified their transactions and asset transitions. When a new checkpoint
// step is appended, it additionally binds that last step to Wavelength's exact
// committed transaction.
func (v *proofLineageVerifier) VerifyUnconfirmedAnchor(_ context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	if v == nil || v.expectedLast == nil ||
		transition.StepIndex != v.expectedLast.stepIndex {
		return nil
	}

	expected := v.expectedLast
	if transition.PreviousAnchorOutpoint != expected.previousOutpoint {
		return fmt.Errorf("chained proof previous outpoint mismatch")
	}
	if transition.AnchorOutpoint != expected.anchorOutpoint {
		return fmt.Errorf("chained proof anchor outpoint mismatch")
	}
	if !bytes.Equal(transition.AnchorTransaction, expected.transaction) {
		return fmt.Errorf("chained proof anchor transaction mismatch")
	}

	return nil
}
