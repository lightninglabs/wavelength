package tapassets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

// ErrOnboardedProofPending reports an onboarding transfer that tapd has not
// seen confirm yet, so its proof file cannot be exported.
var ErrOnboardedProofPending = errors.New("onboarded asset proof is not " +
	"confirmed yet")

// onboardingProofClient is the slice of the tap-sdk client that selects and
// exports onboarding proofs.
type onboardingProofClient interface {
	ListUtxos(context.Context,
		*tapsdk.ListUtxosRequest) (
		map[string]*tapsdk.ManagedUtxo,
		error,
	)

	ListTransfers(context.Context,
		*tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error)

	ExportProof(context.Context, tapsdk.AssetRef, tapsdk.PubKey,
		*tapsdk.Outpoint) (*tapsdk.ProofFile, error)
}

// ownedAssetUtxo is one tapd anchor that can fund an onboarding: it holds a
// single asset record of the requested asset.
type ownedAssetUtxo struct {
	outpoint   tapsdk.Outpoint
	issuanceID tapsdk.AssetID
	scriptKey  tapsdk.PubKey
	amount     uint64
}

// SelectOnboardingProofs exports the proof files of the tapd UTXOs that fund
// an onboarding of amount units of assetRef. An onboarding spends whole
// anchors from one issuance, and any surplus returns to tapd as change.
// Candidates hold one BIP-86 asset record and nothing else of any script key
// type, since spending an anchor consumes every record on it. tapd omits
// anchors under an active lease. tapd exports proofs only for confirmed
// anchors, so the export enforces confirmation.
func SelectOnboardingProofs(ctx context.Context, client onboardingProofClient,
	assetRef string, amount uint64) ([][]byte, error) {

	if client == nil {
		return nil, fmt.Errorf("tapd client is required")
	}
	ref, err := tapsdk.ParseAssetRef(assetRef)
	if err != nil {
		return nil, fmt.Errorf("parse asset ref: %w", err)
	}

	utxos, err := client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{})
	if err != nil {
		return nil, fmt.Errorf("list tapd asset UTXOs: %w", err)
	}
	held, err := client.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{AllTypes: true},
	})
	if err != nil {
		return nil, fmt.Errorf("list tapd asset UTXOs: %w", err)
	}
	records := make(map[tapsdk.Outpoint]int, len(held))
	for _, utxo := range held {
		if utxo != nil {
			records[utxo.OutPoint] = len(utxo.Assets)
		}
	}

	candidates := make([]ownedAssetUtxo, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil || len(utxo.Assets) != 1 ||
			utxo.Assets[0] == nil || records[utxo.OutPoint] != 1 {

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
		return nil, fmt.Errorf("select tapd UTXOs holding %d units of "+
			"%s: %w", amount, assetRef, err)
	}

	proofs := make([][]byte, 0, len(selected))
	for _, utxo := range selected {
		exported, err := client.ExportProof(
			ctx, tapsdk.AssetRefFromAssetID(utxo.issuanceID),
			utxo.scriptKey, &utxo.outpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("export asset proof %v: %w",
				utxo.outpoint, err)
		}
		if exported == nil || len(exported.RawProofFile) == 0 {
			return nil, fmt.Errorf("exported asset proof %v "+
				"is empty", utxo.outpoint)
		}
		proofs = append(proofs, exported.RawProofFile)
	}

	return proofs, nil
}

// selectOwnedAssetUtxos picks the anchors of one issuance that fund amount:
// the smallest anchor covering it, else the largest anchors accumulated until
// they do. Issuances are tried in order, so one wallet state always selects
// the same anchors.
func selectOwnedAssetUtxos(candidates []ownedAssetUtxo,
	amount uint64) ([]ownedAssetUtxo, error) {

	if amount == 0 {
		return nil, fmt.Errorf("onboarding amount is required")
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no tapd UTXO holds the asset alone")
	}

	ordered := slices.Clone(candidates)
	slices.SortFunc(ordered, func(a, b ownedAssetUtxo) int {
		if c := bytes.Compare(
			a.issuanceID[:], b.issuanceID[:],
		); c != 0 {
			return c
		}
		if a.amount != b.amount {
			if a.amount < b.amount {
				return -1
			}

			return 1
		}
		if c := bytes.Compare(
			a.outpoint.Txid[:], b.outpoint.Txid[:],
		); c != 0 {
			return c
		}

		return int(a.outpoint.Index) - int(b.outpoint.Index)
	})

	var (
		best     uint64
		limitErr error
	)
	for start := 0; start < len(ordered); {
		end := start + 1
		for end < len(ordered) &&
			ordered[end].issuanceID == ordered[start].issuanceID {

			end++
		}
		selected, total, err := selectFromIssuance(
			ordered[start:end], amount,
		)
		switch {
		// A later issuance may cover the amount with fewer anchors.
		case errors.Is(err, ErrTooManyTransitionInputs):
			limitErr = err

		case err != nil:
			return nil, err

		case selected != nil:
			return selected, nil
		}
		best = max(best, total)
		start = end
	}
	if limitErr != nil {
		return nil, limitErr
	}

	return nil, fmt.Errorf("one issuance holds at most %d units, need %d",
		best, amount)
}

// selectFromIssuance selects from the amount-ordered anchors of one
// issuance. It returns nil and the issuance total when they cannot cover the
// amount.
func selectFromIssuance(ordered []ownedAssetUtxo, amount uint64) (
	[]ownedAssetUtxo, uint64, error) {

	for idx := range ordered {
		if ordered[idx].amount >= amount {
			return ordered[idx : idx+1 : idx+1], 0, nil
		}
	}

	// Accumulate the largest anchors first, so the transition stays within
	// MaxTransitionInputs whenever any selection can.
	var total uint64
	for idx := len(ordered) - 1; idx >= 0; idx-- {
		if total > math.MaxUint64-ordered[idx].amount {
			return nil, 0, fmt.Errorf("asset amounts overflow")
		}
		total += ordered[idx].amount
		if total < amount {
			continue
		}
		if len(ordered)-idx > MaxTransitionInputs {
			return nil, 0, fmt.Errorf("%w: %d anchors cover "+
				"the amount", ErrTooManyTransitionInputs,
				len(ordered)-idx)
		}

		return slices.Clone(ordered[idx:]), 0, nil
	}

	return nil, total, nil
}

// ExportOnboardedProof exports the confirmed proof file of an onboarding
// output from the tapd that funded it, with its confirmation height. The
// output's OP_TRUE script key is not tapd wallet inventory, so the proof is
// found through the transfer that created it. An unconfirmed transfer returns
// ErrOnboardedProofPending.
func ExportOnboardedProof(ctx context.Context, client onboardingProofClient,
	outpoint wire.OutPoint) ([]byte, uint32, error) {

	if client == nil {
		return nil, 0, fmt.Errorf("tapd client is required")
	}

	transfers, err := client.ListTransfers(
		ctx, &tapsdk.ListTransfersRequest{
			AnchorTxid: outpoint.Hash.String(),
		},
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list tapd transfers: %w", err)
	}

	target := sdkOutpoint(outpoint)
	var (
		found  *tapsdk.TransferOutput
		height uint32
	)
	for _, transfer := range transfers {
		if transfer == nil ||
			transfer.AnchorTxid != outpoint.Hash.String() {

			continue
		}
		for idx := range transfer.Outputs {
			output := &transfer.Outputs[idx]
			if output.AnchorOutpoint != target {
				continue
			}
			if found != nil {
				return nil, 0, fmt.Errorf("onboarding output "+
					"%v holds more than one asset",
					outpoint)
			}
			if transfer.AnchorTxBlockHash == ([32]byte{}) {
				return nil, 0, ErrOnboardedProofPending
			}
			found = output
			height = transfer.AnchorTxBlockHeight
		}
	}
	if found == nil {
		return nil, 0, fmt.Errorf("no tapd transfer creates %v",
			outpoint)
	}

	exported, err := client.ExportProof(
		ctx, tapsdk.AssetRefFromAssetID(found.IssuanceID),
		found.ScriptKey, &found.AnchorOutpoint,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("export onboarded proof: %w", err)
	}
	if exported == nil || len(exported.RawProofFile) == 0 {
		return nil, 0, fmt.Errorf("exported onboarded proof is empty")
	}

	return exported.RawProofFile, height, nil
}
