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

type expectedUnconfirmedAnchor struct {
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
	if transition.StepIndex != 0 {
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
