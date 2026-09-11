package vtxo

import "errors"

var (
	// ErrInsufficientSpendableFunds means live VTXOs cannot cover the
	// requested amount.
	ErrInsufficientSpendableFunds = errors.New("insufficient spendable " +
		"funds")

	// ErrVTXOLiquidityLocked means enough non-terminal liquidity exists,
	// but some of it is currently reserved by another in-flight operation.
	ErrVTXOLiquidityLocked = errors.New("vtxo liquidity temporarily locked")

	// ErrForfeitInFlight means the VTXO is already committed to a round
	// (a leave, a refresh, or an in-round send) and cannot be committed
	// to a second one until that round resolves.
	ErrForfeitInFlight = errors.New("vtxo is already committed to a round")

	// ErrForfeitReleaseRoundMismatch means a forfeit release named a
	// round other than the one this VTXO signed its forfeit for. The
	// release is stale (its round failed and let go of the coin before
	// the current round claimed it) and is refused so it cannot return a
	// coin the current round is forfeiting to the spendable set.
	ErrForfeitReleaseRoundMismatch = errors.New("forfeit release names a " +
		"round that does not hold the reservation")

	// ErrExitInFlight means the VTXO has been handed to the chain
	// resolver for a unilateral exit, so it is no longer available for
	// any cooperative operation.
	ErrExitInFlight = errors.New("vtxo is exiting unilaterally")

	// ErrVTXOTerminal means the VTXO has reached a terminal state and
	// holds no value that can still be moved.
	ErrVTXOTerminal = errors.New("vtxo is no longer active")
)
