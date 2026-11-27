package vtxo

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"

	"github.com/btcsuite/btcd/wire/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// SelectAssetVTXOCandidates picks up to maxInputs live asset VTXOs for
// assetRef using largest-first ordering until targetAmount units are covered.
// Candidates without an asset root, sealed package, or matching canonical
// assetRef are ignored so plain Bitcoin VTXOs and foreign assets are never
// mixed.
func SelectAssetVTXOCandidates(candidates []*Descriptor, assetRef string,
	targetAmount uint64, maxInputs int) ([]*Descriptor, uint64, error) {

	switch {
	case assetRef == "":
		return nil, 0, fmt.Errorf("asset ref must be provided")

	case targetAmount == 0:
		return nil, 0, fmt.Errorf("target asset amount must be " +
			"positive")

	case maxInputs <= 0:
		return nil, 0, fmt.Errorf("max asset inputs must be positive")
	}

	matching := make([]*Descriptor, 0, len(candidates))
	for _, desc := range candidates {
		if !isSpendableAssetDescriptor(desc, assetRef) {
			continue
		}

		matching = append(matching, desc)
	}

	// Sort largest asset balance first, breaking ties deterministically by
	// outpoint so selection is reproducible across runs and platforms.
	slices.SortFunc(matching, compareAssetSelectionOrder)

	selected := make([]*Descriptor, 0, min(len(matching), maxInputs))
	var selectedTotal uint64
	for _, desc := range matching {
		if len(selected) >= maxInputs {
			break
		}
		if desc.TaprootAssetAmount > math.MaxUint64-selectedTotal {
			return nil, 0, fmt.Errorf("selected asset amount " +
				"overflows uint64")
		}

		selected = append(selected, desc)
		selectedTotal += desc.TaprootAssetAmount
		if selectedTotal >= targetAmount {
			return selected, selectedTotal, nil
		}
	}

	// Distinguish a shortfall caused by the per-transfer merge cap from an
	// absolute balance shortfall so callers get an actionable message.
	var uncappedTotal uint64
	for _, desc := range matching {
		if desc.TaprootAssetAmount > math.MaxUint64-uncappedTotal {
			uncappedTotal = math.MaxUint64

			break
		}
		uncappedTotal += desc.TaprootAssetAmount
	}
	if len(matching) > maxInputs && uncappedTotal >= targetAmount {
		return nil, selectedTotal, fmt.Errorf("%w: top %d %s VTXOs "+
			"cover %d of %d units (total live %d across %d VTXOs)",
			ErrInsufficientSpendableFunds, maxInputs, assetRef,
			selectedTotal, targetAmount, uncappedTotal,
			len(matching))
	}

	return nil, selectedTotal, fmt.Errorf("%w: need %d %s units, "+
		"spendable %d", ErrInsufficientSpendableFunds, targetAmount,
		assetRef, selectedTotal)
}

// isSpendableAssetDescriptor reports whether desc is a complete asset-bearing
// VTXO matching assetRef with a sealed asset package available for transition.
func isSpendableAssetDescriptor(desc *Descriptor, assetRef string) bool {
	if desc == nil {
		return false
	}

	return desc.TaprootAssetRoot != nil &&
		strings.EqualFold(desc.TaprootAssetRef, assetRef) &&
		desc.TaprootAssetAmount > 0 &&
		len(desc.TaprootAssetSealedPackage) > 0
}

// compareAssetSelectionOrder orders candidates by descending asset units and
// ascending outpoint bytes for deterministic tie-breaking.
func compareAssetSelectionOrder(a, b *Descriptor) int {
	if a.TaprootAssetAmount != b.TaprootAssetAmount {
		if a.TaprootAssetAmount > b.TaprootAssetAmount {
			return -1
		}

		return 1
	}

	if cmp := bytes.Compare(
		a.Outpoint.Hash[:], b.Outpoint.Hash[:],
	); cmp != 0 {
		return cmp
	}

	switch {
	case a.Outpoint.Index < b.Outpoint.Index:
		return -1

	case a.Outpoint.Index > b.Outpoint.Index:
		return 1

	default:
		return 0
	}
}

// handleSelectAndReserveAssetSpend selects live Taproot Asset VTXOs for one
// asset reference and reserves them for an out-of-round asset transfer via the
// shared detached spend-reservation lifecycle.
func (m *Manager) handleSelectAndReserveAssetSpend(ctx context.Context,
	req *SelectAndReserveAssetSpendRequest) fn.Result[ManagerResp] {

	if req == nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("asset spend request must be provided"),
		)
	}

	liveDescs, err := m.cfg.Store.ListVTXOsByStatus(ctx, VTXOStatusLive)
	if err != nil {
		return fn.Err[ManagerResp](
			fmt.Errorf("list live asset vtxos: %w", err),
		)
	}

	candidates := make([]*Descriptor, 0, len(liveDescs))
	var inMemoryReserved uint64
	for _, desc := range liveDescs {
		if !isSpendableAssetDescriptor(desc, req.AssetRef) {
			continue
		}
		if m.isReserved(desc.Outpoint) {
			inMemoryReserved = saturatingAddUint64(
				inMemoryReserved, desc.TaprootAssetAmount,
			)

			continue
		}

		candidates = append(candidates, desc)
	}

	selected, totalAsset, err := SelectAssetVTXOCandidates(
		candidates, req.AssetRef, req.TargetAmount, req.MaxInputs,
	)
	if err != nil {
		return fn.Err[ManagerResp](
			m.classifyAssetShortfall(
				ctx, req.AssetRef, req.TargetAmount, totalAsset,
				inMemoryReserved, err,
			),
		)
	}

	var reserved []wire.OutPoint
	epochs := make(map[wire.OutPoint]uint64, len(selected))
	for _, desc := range selected {
		ref, ok := m.actors[desc.Outpoint]
		if !ok {
			lazyRef, err := m.respawnActorFromStore(
				ctx, desc.Outpoint,
			)
			if err != nil {
				m.rollbackSpend(ctx, reserved)

				return fn.Err[ManagerResp](
					fmt.Errorf("no actor for asset "+
						"outpoint %s: %w",
						desc.Outpoint, err),
				)
			}

			ref = lazyRef
		}

		epoch := m.markReserved(desc.Outpoint)
		epochs[desc.Outpoint] = epoch
		m.detachedReserve(
			ctx, ref, desc.Outpoint, epoch, &SpendReserveEvent{
				AllowAssetTransition: true,
			}, "asset-spend",
		)
		reserved = append(reserved, desc.Outpoint)
	}

	selectedOut := make([]SelectedAssetVTXO, 0, len(selected))
	for _, desc := range selected {
		selectedOut = append(selectedOut, SelectedAssetVTXO{
			Outpoint:     desc.Outpoint,
			ReserveEpoch: epochs[desc.Outpoint],
			Amount:       desc.Amount,
			AssetAmount:  desc.TaprootAssetAmount,
			PkScript:     bytes.Clone(desc.PkScript),
		})
	}

	m.logger(ctx).InfoS(ctx, "Reserved asset VTXOs for OOR spend",
		slog.String("asset_ref", req.AssetRef),
		slog.Int("count", len(selectedOut)),
		slog.Uint64("total_asset", totalAsset),
		slog.Uint64("target_asset", req.TargetAmount),
	)

	return fn.Ok[ManagerResp](&SelectAndReserveAssetSpendResponse{
		SelectedVTXOs:      selectedOut,
		TotalAssetSelected: totalAsset,
	})
}

// classifyAssetShortfall distinguishes temporarily locked asset liquidity from
// an actual balance shortfall so callers can retry locked-liquidity errors.
func (m *Manager) classifyAssetShortfall(ctx context.Context, assetRef string,
	targetAmount, liveSpendable, inMemoryReserved uint64,
	selectErr error) error {

	nonTerminal, err := m.cfg.Store.ListLiveVTXOs(ctx)
	if err != nil {
		return selectErr
	}

	locked := inMemoryReserved
	for _, desc := range nonTerminal {
		if !isSpendableAssetDescriptor(desc, assetRef) {
			continue
		}
		if desc.Status == VTXOStatusLive {
			continue
		}

		locked = saturatingAddUint64(locked, desc.TaprootAssetAmount)
	}

	if locked > 0 &&
		saturatingAddUint64(liveSpendable, locked) >= targetAmount {
		return fmt.Errorf("%w: need %d %s units, spendable %d, "+
			"locked %d", ErrVTXOLiquidityLocked, targetAmount,
			assetRef, liveSpendable, locked)
	}

	return selectErr
}

// saturatingAddUint64 adds a and b, clamping to math.MaxUint64 on overflow.
func saturatingAddUint64(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}

	return a + b
}
