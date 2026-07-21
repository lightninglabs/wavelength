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

	// ErrRequiredVTXOInvalid means a caller-required outpoint is
	// duplicated, missing, or not in Live state.
	ErrRequiredVTXOInvalid = errors.New("invalid required vtxo")
)
