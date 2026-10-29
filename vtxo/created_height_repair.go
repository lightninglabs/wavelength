package vtxo

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const createdHeightRepairTimeout = 2 * time.Second

// CreatedHeightRepairer authenticates and persists a replacement creation
// height. Returning successfully permits the actor to use the new height.
type CreatedHeightRepairer func(context.Context, *Descriptor) (int32, error)

// CreatedHeightRepairStore updates only the creation height of the exact row
// whose expiry and previous height were authenticated by the caller.
type CreatedHeightRepairStore interface {
	// RepairVTXOCreatedHeight changes only creation height while matching
	// the saved expiry and the observed previous height.
	RepairVTXOCreatedHeight(context.Context, *Descriptor,
		AuthenticatedBatchEvidence) error
}

// repairLegacyCreatedHeight restores a usable descriptor before the current
// block drives expiry. Lookup or write failure leaves both the actor and its
// row untouched, and the next epoch retries without blocking manual recovery.
func (a *VTXOActor) repairLegacyCreatedHeight(ctx context.Context) {
	desc := a.cfg.VTXO
	if a.cfg.RepairCreatedHeight == nil || desc.BatchExpiry <= 0 ||
		desc.CreatedHeight <= desc.BatchExpiry {
		return
	}

	repairCtx, cancel := context.WithTimeout(
		ctx, createdHeightRepairTimeout,
	)
	defer cancel()
	height, err := a.cfg.RepairCreatedHeight(repairCtx, desc)
	if err != nil {
		a.logger(ctx).InfoS(ctx, "Creation-height repair deferred",
			err,
			slog.String("outpoint", desc.Outpoint.String()),
		)

		return
	}
	desc.CreatedHeight = height
}

// RepairCreatedHeight repairs an old inconsistent creation height without
// guessing which of the two saved heights was wrong. The confirmed ancestry
// must reproduce the saved expiry before any metadata is changed.
func RepairCreatedHeight(ctx context.Context, desc *Descriptor,
	authenticate func(context.Context, []Ancestry) (
		AuthenticatedBatchEvidence, error),
	store CreatedHeightRepairStore) (int32, error) {

	evidence, err := authenticate(ctx, desc.Ancestry)
	if err != nil {
		return 0, err
	}
	if evidence.BatchExpiry != desc.BatchExpiry {
		return 0, fmt.Errorf("confirmed expiry %d differs from saved "+
			"expiry %d", evidence.BatchExpiry, desc.BatchExpiry)
	}
	if err := store.RepairVTXOCreatedHeight(
		ctx, desc, evidence,
	); err != nil {
		return 0, err
	}

	return evidence.EarliestCommitmentHeight, nil
}
