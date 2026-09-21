package waved

import (
	"fmt"
	"math"
	"slices"

	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
)

// validateAssetRefFilter rejects ambiguous and malformed asset identifiers.
func validateAssetRefFilter(value string) error {
	if value == "" {
		return nil
	}
	ref, err := tapsdk.ParseAssetRef(value)
	if err != nil {
		return fmt.Errorf("invalid asset reference: %w", err)
	}
	if ref.String() != value {
		return fmt.Errorf("asset reference must use canonical encoding")
	}

	return nil
}

// liveAssetBalances groups confirmed live holdings by semantic asset reference.
// Amounts from different assets are never summed, and no decimal conversion is
// inferred from an identifier. Carrier satoshis stay separate from asset units.
func liveAssetBalances(descs []*vtxo.Descriptor) ([]*waverpc.AssetBalance,
	error) {

	byRef := make(map[string]*waverpc.AssetBalance)
	for _, desc := range descs {
		if desc == nil || desc.Status != vtxo.VTXOStatusLive {
			continue
		}
		if desc.TaprootAssetRoot == nil {
			if desc.TaprootAssetRef != "" ||
				desc.TaprootAssetAmount != 0 {
				return nil, fmt.Errorf("asset holding lacks " +
					"commitment root")
			}

			continue
		}
		if desc.TaprootAssetRef == "" || desc.TaprootAssetAmount == 0 ||
			desc.Amount <= 0 {
			return nil, fmt.Errorf("asset holding is incomplete")
		}
		if err := validateAssetRefFilter(
			desc.TaprootAssetRef,
		); err != nil {
			return nil, err
		}
		balance := byRef[desc.TaprootAssetRef]
		if balance == nil {
			balance = &waverpc.AssetBalance{
				AssetRef: desc.TaprootAssetRef,
			}
			byRef[desc.TaprootAssetRef] = balance
		}
		if desc.TaprootAssetAmount > math.MaxUint64-balance.Amount ||
			int64(desc.Amount) > math.MaxInt64-balance.CarrierSat {
			return nil, fmt.Errorf("asset balance overflow for %s",
				desc.TaprootAssetRef)
		}
		balance.Amount += desc.TaprootAssetAmount
		balance.CarrierSat += int64(desc.Amount)
	}

	refs := make([]string, 0, len(byRef))
	for ref := range byRef {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	balances := make([]*waverpc.AssetBalance, 0, len(refs))
	for _, ref := range refs {
		balances = append(balances, byRef[ref])
	}

	return balances, nil
}
