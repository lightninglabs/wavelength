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
	// conflict that reached finality. The VTXO is unusable; recovery
	// requires the conflicting transaction to itself reorg out (beyond
	// policy finality).
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
		return "available_unknown"

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
// parent (the worst rank). With no parents it returns AvailabilityUnknown.
func CombineAvailability(parents ...Availability) Availability {
	if len(parents) == 0 {
		return AvailabilityUnknown
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
// record yet (e.g. not registered with the manager during rollout) maps to
// AvailabilityUnknown, so a caller that wants a permissive posture can admit
// when no record blocks it. With no txids it returns AvailabilityUnknown.
//
// This is the gate logic the VTXO manager calls per candidate: a VTXO is
// admissible iff LineageAvailability(...).Usable() — or, permissively, iff it
// is not in a limbo/invalidated state.
func LineageAvailability(ctx context.Context, store Store,
	batchTxids ...chainhash.Hash) (Availability, error) {

	if len(batchTxids) == 0 {
		return AvailabilityUnknown, nil
	}

	avails := make([]Availability, 0, len(batchTxids))
	for _, txid := range batchTxids {
		record, err := store.GetBatch(ctx, txid)
		switch {
		case errors.Is(err, ErrBatchNotFound):
			avails = append(avails, AvailabilityUnknown)

		case err != nil:
			return AvailabilityUnknown, err

		default:
			avails = append(
				avails, AvailabilityForState(record.State),
			)
		}
	}

	return CombineAvailability(avails...), nil
}

// LineageBlocked reports whether a VTXO descending from the given batches must
// be refused admission because at least one parent batch is in a limbo or
// invalidated state. It is the permissive form of the gate: unseen or
// not-yet-registered lineage does NOT block (only positively-bad lineage
// does), which keeps the gate safe to enable before every producer registers
// its batches.
func LineageBlocked(ctx context.Context, store Store,
	batchTxids ...chainhash.Hash) (bool, Availability, error) {

	avail, err := LineageAvailability(ctx, store, batchTxids...)
	if err != nil {
		return false, avail, err
	}

	switch avail {
	case LimboReorg, LimboConflict, Invalidated:
		return true, avail, nil

	default:
		return false, avail, nil
	}
}
