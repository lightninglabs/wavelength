package batchcanon

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// Availability is the derived spendability of a VTXO's lineage, computed from
// the canonicality State of the batch(es) the VTXO descends from. It is the
// vocabulary the VTXO manager's admission gate and the producers consume; it
// is never persisted (it is always recomputed from the current batch State).
type Availability int

const (
	// AvailableFinal means every parent batch reached policy finality. The
	// VTXO is usable and its lineage is as settled as policy allows.
	AvailableFinal Availability = iota

	// AvailableProvisional means every parent batch is confirmed but not
	// yet final. The VTXO is usable at one-confirmation usability depth,
	// but the lineage could still reorg.
	AvailableProvisional

	// AvailabilityUnknown means at least one parent batch has no
	// confirmation observation yet (unseen), and none is in limbo or
	// invalidated. The lineage is not yet usable, but nothing is wrong.
	AvailabilityUnknown

	// LineageReconciling means at least one lineage record is missing,
	// incomplete, quarantined, or lacks a Ready snapshot for its current
	// observation generation. It is a retryable fail-closed result.
	LineageReconciling

	// LimboReorg means at least one parent batch was reorged out with no
	// input conflict. The VTXO is temporarily unusable and may recover if
	// the batch reconfirms.
	LimboReorg

	// LimboConflict means at least one parent batch has a consumed input
	// double-spent by a conflicting transaction that has not yet reached
	// finality. The VTXO is unusable and may recover only if the conflict
	// reorgs out.
	LimboConflict

	// Invalidated means at least one parent batch has a consumed-input
	// conflict that reached policy finality. The VTXO is terminally
	// unusable within the configured basic-v1 claim.
	Invalidated
)

// availabilityRank orders availabilities from most to least available, so the
// combined availability of a multi-parent lineage is the worst (highest rank)
// of its parents.
func availabilityRank(a Availability) int {
	switch a {
	case AvailableFinal:
		return 0

	case AvailableProvisional:
		return 1

	case AvailabilityUnknown:
		return 2

	case LineageReconciling:
		return 6

	case LimboReorg:
		return 3

	case LimboConflict:
		return 4

	case Invalidated:
		return 5

	default:
		return 2
	}
}

// String returns a stable lower-snake-case name for the availability.
func (a Availability) String() string {
	switch a {
	case AvailableFinal:
		return "available_final"

	case AvailableProvisional:
		return "available_provisional"

	case AvailabilityUnknown:
		return "lineage_unseen"

	case LineageReconciling:
		return "lineage_reconciling"

	case LimboReorg:
		return "limbo_reorg"

	case LimboConflict:
		return "limbo_conflict"

	case Invalidated:
		return "invalidated"

	default:
		return fmt.Sprintf("unknown(%d)", int(a))
	}
}

// Usable reports whether a VTXO with this lineage availability may be admitted
// for spending or forfeiting. Only confirmed lineage (provisional or final) is
// usable; unseen, limbo, and invalidated lineage is not.
func (a Availability) Usable() bool {
	return a == AvailableFinal || a == AvailableProvisional
}

// AvailabilityForState maps a single batch's canonicality State to the
// availability it confers on its dependent VTXOs.
func AvailabilityForState(s State) Availability {
	switch s {
	case StateFinalized:
		return AvailableFinal

	case StateProvisional:
		return AvailableProvisional

	case StateReorgedOut:
		return LimboReorg

	case StateConflictProvisional:
		return LimboConflict

	case StateConflictFinalized:
		return Invalidated

	case StateUnseen:
		return AvailabilityUnknown

	default:
		return AvailabilityUnknown
	}
}

// CombineAvailability returns the availability of a VTXO that depends on
// several parent batches: a VTXO is only as available as its least-available
// parent (the worst rank). With no parents it returns LineageReconciling.
func CombineAvailability(parents ...Availability) Availability {
	if len(parents) == 0 {
		return LineageReconciling
	}

	worst := parents[0]
	for _, p := range parents[1:] {
		if availabilityRank(p) > availabilityRank(worst) {
			worst = p
		}
	}

	return worst
}

// LineageAvailability returns the combined availability of a VTXO that
// descends from the given batch txids, loading each batch's canonicality
// state from the store and taking the worst across them. A batch with no
// record yet maps to AvailabilityUnknown. Missing lineage is never usable:
// registration completeness is part of the safety proof, not a compatibility
// hint. With no txids it returns LineageReconciling.
//
// This is the gate logic the VTXO manager calls per candidate: a VTXO is
// admissible only when LineageAvailability(...).Usable().
func LineageAvailability(ctx context.Context, store Reader,
	batchTxids ...chainhash.Hash) (Availability, error) {

	if len(batchTxids) == 0 {
		return LineageReconciling, nil
	}

	avails := make([]Availability, 0, len(batchTxids))
	for _, txid := range batchTxids {
		record, err := store.GetBatch(ctx, txid)
		switch {
		case errors.Is(err, ErrBatchNotFound):
			avails = append(avails, LineageReconciling)

		case err != nil:
			return AvailabilityUnknown, err

		case !record.Ready():
			avails = append(avails, LineageReconciling)

		default:
			avails = append(
				avails, AvailabilityForState(record.State),
			)
		}
	}

	return CombineAvailability(avails...), nil
}

// LineageBlocked reports whether a VTXO descending from the given batches must
// be refused admission. Only fully registered, confirmed lineage is usable;
// unseen, missing, limbo, and invalidated lineage all fail closed.
func LineageBlocked(ctx context.Context, store Reader,
	batchTxids ...chainhash.Hash) (bool, Availability, error) {

	avail, err := LineageAvailability(ctx, store, batchTxids...)
	if err != nil {
		return false, avail, err
	}

	return !avail.Usable(), avail, nil
}
