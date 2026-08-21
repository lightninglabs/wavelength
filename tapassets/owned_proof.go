package tapassets

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"

	tapsdk "github.com/lightninglabs/tap-sdk"
)

// ownedAssetUtxo is one candidate funding UTXO of the daemon's own tapd
// wallet: the anchor holding it plus the tranche coordinates the proof
// archive exports its proof file under.
type ownedAssetUtxo struct {
	outpoint   tapsdk.Outpoint
	issuanceID tapsdk.AssetID
	scriptKey  tapsdk.PubKey
	amount     uint64
}

// ResolveOwnedAssetProofs exports the proof files of the wallet-owned UTXOs
// funding one onboarding of the requested amount. Onboarding consumes whole
// anchors, so the selected UTXOs must cover the amount and any surplus
// re-anchors as asset change on the same transition.
//
// Only unleased UTXOs holding the asset in isolation are candidates.
// Confirmation is enforced by the export itself: the archive has no proof
// file for an anchor that has not confirmed.
func ResolveOwnedAssetProofs(ctx context.Context, wallet *tapsdk.Wallet,
	assetRef string, amount uint64) ([][]byte, error) {

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

	candidates := make([]ownedAssetUtxo, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil || len(utxo.LeaseOwner) != 0 ||
			len(utxo.Assets) != 1 || utxo.Assets[0] == nil {

			continue
		}
		asset := utxo.Assets[0]
		if !asset.AssetRef.Equivalent(ref) || asset.Amount == 0 {
			continue
		}

		candidates = append(candidates, ownedAssetUtxo{
			outpoint:   utxo.OutPoint,
			issuanceID: asset.Genesis.IssuanceID,
			scriptKey:  asset.ScriptKey.PubKey,
			amount:     asset.Amount,
		})
	}

	selected, err := selectOwnedAssetUtxos(candidates, amount)
	if err != nil {
		return nil, fmt.Errorf("select owned UTXOs holding %d units "+
			"of %s: %w", amount, assetRef, err)
	}

	proofs := make([][]byte, 0, len(selected))
	for idx := range selected {
		utxo := &selected[idx]

		// Proofs are exported per tranche, so the issuance ID is the
		// only ref the archive accepts here.
		exported, err := client.ExportProof(
			ctx, tapsdk.AssetRefFromAssetID(utxo.issuanceID),
			utxo.scriptKey, &utxo.outpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("export owned asset proof "+
				"%v: %w", utxo.outpoint, err)
		}
		if exported == nil || len(exported.RawProofFile) == 0 {
			return nil, fmt.Errorf("exported owned asset proof %v "+
				"is empty", utxo.outpoint)
		}

		proofs = append(proofs, exported.RawProofFile)
	}

	return proofs, nil
}

// selectOwnedAssetUtxos picks the funding set for one onboarding of the
// given amount: an exact single UTXO when the wallet holds one, otherwise
// the smallest single UTXO that covers the amount, otherwise the smallest
// UTXOs accumulated until they do. Candidates are ordered by amount and
// then by outpoint, so one wallet state always selects the same inputs and
// a replay rebuilds the same transition.
func selectOwnedAssetUtxos(candidates []ownedAssetUtxo,
	amount uint64) ([]ownedAssetUtxo, error) {

	if amount == 0 {
		return nil, fmt.Errorf("onboarding amount is required")
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no owned UTXO holds the asset")
	}

	ordered := append([]ownedAssetUtxo(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].amount != ordered[j].amount {
			return ordered[i].amount < ordered[j].amount
		}

		return outpointBefore(ordered[i].outpoint, ordered[j].outpoint)
	})

	// One anchor holding exactly the amount needs no change output at
	// all, so it always wins over any accumulation that would.
	for idx := range ordered {
		if ordered[idx].amount == amount {
			return ordered[idx : idx+1 : idx+1], nil
		}
	}

	// The smallest single anchor above the amount wastes the least: it
	// spends one input and leaves the least change behind.
	for idx := range ordered {
		if ordered[idx].amount > amount {
			return ordered[idx : idx+1 : idx+1], nil
		}
	}

	var (
		selected = make([]ownedAssetUtxo, 0, len(ordered))
		total    uint64
	)
	for idx := range ordered {
		if total > math.MaxUint64-ordered[idx].amount {
			return nil, fmt.Errorf("owned asset amounts overflow")
		}
		selected = append(selected, ordered[idx])
		total += ordered[idx].amount
		if total >= amount {
			return selected, nil
		}
	}

	return nil, fmt.Errorf("owned UTXOs hold %d units, need %d", total,
		amount)
}

// outpointBefore orders two outpoints, breaking amount ties in the funding
// selection with a total order.
func outpointBefore(left, right tapsdk.Outpoint) bool {
	if cmp := bytes.Compare(left.Txid[:], right.Txid[:]); cmp != 0 {
		return cmp < 0
	}

	return left.Index < right.Index
}
