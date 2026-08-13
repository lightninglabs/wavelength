package tapassets

import (
	"context"
	"fmt"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

// ResolveOwnedAssetProof exports the proof file of the wallet-owned UTXO
// holding exactly the requested amount of the given asset. Onboarding
// consumes one complete anchor, so the wallet must hold the amount in
// exactly one UTXO; zero or multiple candidates are an error.
func ResolveOwnedAssetProof(ctx context.Context, wallet *tapsdk.Wallet,
	assetRef string, amount uint64) ([]byte, error) {

	if wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}

	ref, err := tapsdk.ParseAssetRef(assetRef)
	if err != nil {
		return nil, fmt.Errorf("parse asset ref: %w", err)
	}

	client := wallet.Client()
	utxos, err := client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{})
	if err != nil {
		return nil, fmt.Errorf("list Taproot Asset UTXOs: %w", err)
	}

	var (
		matched    *tapsdk.ManagedUtxo
		candidates int
	)
	for _, utxo := range utxos {
		if utxo == nil || len(utxo.Assets) != 1 ||
			utxo.Assets[0] == nil {

			continue
		}
		asset := utxo.Assets[0]
		if !asset.AssetRef.Equivalent(ref) || asset.Amount != amount {
			continue
		}

		candidates++
		matched = utxo
	}
	if candidates != 1 {
		return nil, fmt.Errorf("expected exactly one owned UTXO "+
			"holding %d units of %s, found %d; onboarding "+
			"consumes one complete anchor, so hold exactly the "+
			"amount in one UTXO", amount, assetRef, candidates)
	}

	// Proofs are exported per tranche, so the issuance ID is the only
	// ref the archive accepts here.
	asset := matched.Assets[0]
	exported, err := client.ExportProof(
		ctx, tapsdk.AssetRefFromAssetID(asset.Genesis.IssuanceID),
		asset.ScriptKey.PubKey, &matched.OutPoint,
	)
	if err != nil {
		return nil, fmt.Errorf("export owned asset proof: %w", err)
	}
	if exported == nil || len(exported.RawProofFile) == 0 {
		return nil, fmt.Errorf("exported owned asset proof is empty")
	}

	return exported.RawProofFile, nil
}
