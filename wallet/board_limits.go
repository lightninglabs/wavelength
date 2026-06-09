package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// minChangeFloor is the absolute floor applied to the on-chain change
// output produced by a clipped board when the operator terms advertise
// no dust limit. It matches the P2TR dust threshold.
const minChangeFloor = btcutil.Amount(330)

var (
	// ErrBoardingCapReached is returned when the operator's maximum
	// user balance leaves no usable headroom to board any of the
	// confirmed boarding balance. The wrapped message carries the
	// current balance and the cap.
	ErrBoardingCapReached = errors.New("maximum wallet balance reached, " +
		"cannot board")
)

// boardingClamp describes how a confirmed boarding balance is divided
// once the operator's per-VTXO and total-balance limits are applied.
type boardingClamp struct {
	// BoardAmount is the portion of the confirmed balance that boards
	// into VTXOs this round.
	BoardAmount btcutil.Amount

	// Change is the clipped remainder that returns on-chain via a
	// leave output. Zero when the full balance boards.
	Change btcutil.Amount

	// VTXOCount is the number of VTXO outputs BoardAmount splits
	// into. Always at least the caller's requested count, raised
	// further when needed so every output fits under the per-VTXO
	// maximum.
	VTXOCount uint32
}

// clampBoardingAmount applies the operator limits to a confirmed
// boarding balance. The headroom parameter is the remaining capacity
// under the operator's maximum user balance (negative when the wallet
// is already over the cap); pass fn-style "no cap" as a headroom of
// total or more. maxVTXO of zero disables the per-VTXO cap. The floor
// parameter is the smallest viable output (dust), applied both to the
// boarded amount and to the change remainder.
//
// When the clipped remainder would land below the floor, the boarded
// amount is reduced so the change output stays spendable: a sub-dust
// change output could neither be created in the batch transaction nor
// re-boarded later.
func clampBoardingAmount(total btcutil.Amount, targetCount uint32, maxVTXO,
	headroom, floor btcutil.Amount) (*boardingClamp, error) {

	if total <= 0 {
		return nil, fmt.Errorf("boarding balance must be positive")
	}

	boardAmt := total
	if boardAmt > headroom {
		boardAmt = headroom
	}
	if boardAmt < floor || boardAmt <= 0 {
		return nil, fmt.Errorf("%w: balance headroom %v is below the "+
			"minimum boardable amount %v", ErrBoardingCapReached,
			headroom, floor)
	}

	// Keep the change remainder spendable: if clipping leaves a
	// sub-floor remainder, shrink the boarded amount until the change
	// output clears the floor.
	change := total - boardAmt
	if change > 0 && change < floor {
		boardAmt -= floor - change
		change = floor

		if boardAmt < floor || boardAmt <= 0 {
			return nil, fmt.Errorf("%w: balance headroom leaves "+
				"no boardable amount above the dust floor %v",
				ErrBoardingCapReached, floor)
		}
	}

	count := targetCount
	if count == 0 {
		count = 1
	}

	// Raise the output count until every split piece fits under the
	// per-VTXO maximum. splitBoardingAmount distributes the total as
	// evenly as possible, so ceil(boardAmt/maxVTXO) outputs suffice.
	if maxVTXO > 0 {
		minCount := uint32(
			(int64(boardAmt) + int64(maxVTXO) - 1) /
				int64(maxVTXO),
		)
		if count < minCount {
			count = minCount
		}
	}

	return &boardingClamp{
		BoardAmount: boardAmt,
		Change:      change,
		VTXOCount:   count,
	}, nil
}

// boardingTerms resolves the operator terms relevant to boarding limit
// enforcement. It returns nil (and no error) when the wallet has no
// terms source wired or when the operator advertises no caps, so the
// caller can skip clamping and preserve the unbounded boarding flow.
func (a *Ark) boardingTerms(ctx context.Context) (*types.OperatorTerms, error) {
	if a.fetchOperatorTerms == nil {
		return nil, nil
	}

	terms, err := a.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, err
	}
	if terms == nil {
		return nil, nil
	}
	if terms.MaxBoardingAmount == 0 && terms.MaxUserBalance == 0 {
		return nil, nil
	}

	return terms, nil
}

// applyBoardingLimits clamps a confirmed boarding balance to the
// operator's advertised limits and, when the balance is clipped, builds
// the change leave output carrying the remainder back on-chain.
func (a *Ark) applyBoardingLimits(ctx context.Context, total btcutil.Amount,
	targetCount uint32, terms *types.OperatorTerms) (*boardingClamp,
	*types.LeaveRequest, error) {

	// With no balance cap the headroom equals the full balance, so
	// only the per-VTXO maximum shapes the split.
	headroom := total
	if terms.MaxUserBalance > 0 {
		var err error
		headroom, err = a.boardingHeadroom(ctx, terms.MaxUserBalance)
		if err != nil {
			return nil, nil, err
		}
	}

	// The floor guards both the boarded amount and the change
	// remainder against unspendable outputs.
	floor := terms.DustLimit
	if terms.MinBoardingAmount > floor {
		floor = terms.MinBoardingAmount
	}
	if floor < minChangeFloor {
		floor = minChangeFloor
	}

	clamp, err := clampBoardingAmount(
		total, targetCount, terms.MaxBoardingAmount, headroom, floor,
	)
	if err != nil {
		return nil, nil, err
	}

	if clamp.Change == 0 {
		return clamp, nil, nil
	}

	leave, err := a.boardingChangeLeave(ctx, clamp.Change, terms)
	if err != nil {
		return nil, nil, err
	}

	return clamp, leave, nil
}

// boardingHeadroom computes the remaining capacity under the operator's
// maximum user balance. The current holdings are the wallet's live VTXO
// balance plus boarding intents already adopted into an in-flight round
// (their VTXOs are committed but not yet live, so skipping them would
// let back-to-back boards overshoot the cap).
func (a *Ark) boardingHeadroom(ctx context.Context,
	maxUserBalance btcutil.Amount) (btcutil.Amount, error) {

	var current btcutil.Amount
	if a.fetchLiveBalance != nil {
		liveBalance, err := a.fetchLiveBalance(ctx)
		if err != nil {
			return 0, fmt.Errorf("fetch live VTXO balance: %w", err)
		}

		current += liveBalance
	}

	adopted, err := a.store.FetchBoardingIntentsByStatus(
		ctx, BoardingStatusAdopted,
	)
	if err != nil {
		return 0, fmt.Errorf("fetch adopted boarding intents: %w", err)
	}
	for _, intent := range adopted {
		current += intent.ChainInfo.Amount
	}

	return maxUserBalance - current, nil
}

// boardingChangeLeave derives a fresh boarding address and wraps the
// clipped remainder into a leave request paying back to it. Because the
// script is a boarding script the wallet already watches, the change
// confirms as a brand-new boarding intent once the batch transaction
// hits the chain, ready to board again when headroom frees up.
func (a *Ark) boardingChangeLeave(ctx context.Context, change btcutil.Amount,
	terms *types.OperatorTerms) (*types.LeaveRequest, error) {

	resp := a.handleCreateBoardingAddress(
		ctx, &CreateBoardingAddressRequest{
			OperatorKey: terms.PubKey,
			ExitDelay:   terms.BoardingExitDelay,
		},
	)
	walletResp, err := resp.Unpack()
	if err != nil {
		return nil, fmt.Errorf("create change boarding address: %w",
			err)
	}

	addrResp, ok := walletResp.(*CreateBoardingAddressResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected wallet response type: %T",
			walletResp)
	}

	pkScript, err := txscript.PayToAddrScript(addrResp.Address)
	if err != nil {
		return nil, fmt.Errorf("change address pkScript: %w", err)
	}

	return &types.LeaveRequest{
		Output: &wire.TxOut{
			Value:    int64(change),
			PkScript: pkScript,
		},
	}, nil
}
