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

	// ErrExitInFlight means the VTXO has been handed to the chain
	// resolver for a unilateral exit, so it is no longer available for
	// any cooperative operation.
	ErrExitInFlight = errors.New("vtxo is exiting unilaterally")

	// ErrVTXOTerminal means the VTXO has reached a terminal state and
	// holds no value that can still be moved.
	ErrVTXOTerminal = errors.New("vtxo is no longer active")

	// ErrRequiredVTXOInvalid means a caller-required outpoint is
	// duplicated, missing, or not in Live state.
	ErrRequiredVTXOInvalid = errors.New("invalid required vtxo")
)
