package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
)

// minChangeFloor is the absolute floor applied to the on-chain change
// output produced by a clipped board when the operator terms advertise
// no dust limit. It matches the P2TR dust threshold.
const minChangeFloor = btcutil.Amount(330)

// maxBoardOutputs bounds the number of VTXO outputs a single board may
// split into. It guards the per-VTXO split arithmetic: an operator
// advertising a pathologically small per-VTXO maximum could otherwise
// drive the required output count past a uint32 (silently wrapping to a
// tiny count that mints oversized outputs) or into a multi-gigabyte slice
// allocation. Under any sane terms the count is a handful, so this ceiling
// only ever trips on misconfiguration, where rejecting is safer than
// minting oversized or unboundedly many outputs.
const maxBoardOutputs = 1000

var (
	// ErrBoardingCapReached is returned when the operator's maximum
	// user balance leaves no usable headroom to board any of the
	// confirmed boarding balance. The wrapped message carries the
	// current balance and the cap.
	ErrBoardingCapReached = errors.New("maximum wallet balance reached, " +
		"cannot board")

	// ErrBoardAmountBelowFloor is returned when the confirmed boarding
	// balance (or what survives the change-floor adjustment) is below the
	// minimum boardable amount, independent of any balance cap. It is
	// distinct from ErrBoardingCapReached so callers do not surface a
	// "maximum balance reached" message for a board that is simply too
	// small to be worth a VTXO.
	ErrBoardAmountBelowFloor = errors.New("boarding amount below the " +
		"minimum boardable floor")

	// ErrTooManyBoardOutputs is returned when satisfying the per-VTXO
	// maximum would require more than maxBoardOutputs VTXO outputs. It
	// only trips on a misconfigured (pathologically small) per-VTXO
	// maximum and exists as a hard guard against the per-VTXO split
	// arithmetic overflowing or over-allocating.
	ErrTooManyBoardOutputs = errors.New("boarding requires too many " +
		"VTXOs under the per-VTXO maximum")

	// ErrMaxVTXOBelowFloor is returned when the operator's per-VTXO
	// maximum is itself below the dust floor. No VTXO can then be both
	// at or under the maximum and at or above the floor, so no spendable
	// output exists to board into. It only trips on a misconfigured
	// operator advertising a per-VTXO maximum smaller than the dust
	// limit / minimum boarding amount.
	ErrMaxVTXOBelowFloor = errors.New("per-VTXO maximum is below the " +
		"minimum boardable floor")
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
	// into. It is raised above the caller's requested count when the
	// per-VTXO maximum needs more pieces, and lowered below it when the
	// dust floor allows fewer, so an even split of BoardAmount always
	// lands every piece within [floor, maxVTXO].
	VTXOCount uint32

	// DustToFee is the sub-floor remainder dropped to the miner fee
	// rather than minted as a dust VTXO. It is non-zero only when the
	// per-VTXO maximum is so small relative to the floor that BoardAmount
	// cannot be divided into whole [floor, maxVTXO] pieces; the leftover
	// is provably below the floor, so it could never have been a
	// spendable output on its own. BoardAmount + Change + DustToFee
	// always equals the original confirmed balance.
	DustToFee btcutil.Amount
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

	// Clip the boardable amount to the remaining cap headroom, tracking
	// whether the cap (rather than the dust floor) was the binding
	// constraint so the rejection error names the real cause.
	boardAmt := total
	cappedByHeadroom := false
	if boardAmt > headroom {
		boardAmt = headroom
		cappedByHeadroom = true
	}
	switch {
	case boardAmt <= 0:
		// Headroom is zero or negative: the wallet is already at or
		// over the cap, so nothing can board.
		return nil, fmt.Errorf("%w: balance headroom %v leaves "+
			"nothing to board", ErrBoardingCapReached, headroom)

	case boardAmt < floor && cappedByHeadroom:
		// The cap clipped the board below the dust floor.
		return nil, fmt.Errorf("%w: balance headroom %v is below the "+
			"minimum boardable amount %v", ErrBoardingCapReached,
			headroom, floor)

	case boardAmt < floor:
		// No cap was in play; the confirmed balance itself is simply
		// below the floor.
		return nil, fmt.Errorf("%w: confirmed boarding balance %v is "+
			"below the minimum boardable amount %v",
			ErrBoardAmountBelowFloor, total, floor)
	}

	// Keep the change remainder spendable: if clipping leaves a
	// sub-floor remainder, shrink the boarded amount until the change
	// output clears the floor.
	change := total - boardAmt
	if change > 0 && change < floor {
		boardAmt -= floor - change
		change = floor

		if boardAmt < floor {
			return nil, fmt.Errorf("%w: clipping to the cap "+
				"leaves no boardable amount above the dust "+
				"floor %v", ErrBoardAmountBelowFloor, floor)
		}
	}

	count := targetCount
	if count == 0 {
		count = 1
	}

	// Bound the split so an even division of boardAmt lands every VTXO
	// piece within [floor, maxVTXO]. Two opposing constraints set the
	// window: the per-VTXO maximum forces the count UP (more, smaller
	// pieces) while the dust floor forces it DOWN (fewer, larger pieces).
	//
	// maxByFloor is the most pieces an even split can yield while keeping
	// each at or above the floor. It is at least 1 because boardAmt >=
	// floor is guaranteed above.
	maxByFloor := int64(boardAmt) / int64(floor)

	var dustToFee btcutil.Amount
	switch {
	// No per-VTXO maximum: only the floor bounds the split, so cap the
	// requested count so an even split never drops a piece below it.
	case maxVTXO <= 0:
		if int64(count) > maxByFloor {
			count = uint32(maxByFloor)
		}

	// A per-VTXO maximum below the floor admits no valid VTXO at all:
	// every piece would be either above the maximum or below the floor.
	// Nothing can board, and the server would reject each output anyway,
	// so fail with a clear misconfiguration error rather than mint
	// guaranteed-invalid outputs.
	case maxVTXO < floor:
		return nil, fmt.Errorf("%w: per-VTXO maximum %v, floor %v",
			ErrMaxVTXOBelowFloor, maxVTXO, floor)

	default:
		// minByMax is the fewest pieces that keep each at or under the
		// per-VTXO maximum. Computed in int64 and bounded by
		// maxBoardOutputs BEFORE the uint32 narrowing, so a
		// pathologically small maximum cannot wrap the count or trigger
		// a huge allocation downstream.
		minByMax := (int64(boardAmt) + int64(maxVTXO) - 1) /
			int64(maxVTXO)
		if minByMax > maxBoardOutputs {
			return nil, fmt.Errorf("%w: boarding %v under "+
				"per-VTXO maximum %v needs %d VTXOs, "+
				"exceeding the %d-output limit",
				ErrTooManyBoardOutputs, boardAmt, maxVTXO,
				minByMax, maxBoardOutputs)
		}

		switch {
		// Feasible: an even split into any count within
		// [minByMax, maxByFloor] keeps every piece inside
		// [floor, maxVTXO]. Honor the requested count within that
		// window.
		case minByMax <= maxByFloor:
			if int64(count) < minByMax {
				count = uint32(minByMax)
			}
			if int64(count) > maxByFloor {
				count = uint32(maxByFloor)
			}

		// Infeasible even at the fewest pieces the maximum allows:
		// boardAmt cannot be divided into whole [floor, maxVTXO]
		// pieces. Board as many full maximum-size VTXOs as fit and
		// let the leftover fall to the miner fee rather than minting a
		// dust VTXO. The leftover is provably below the floor, so it
		// could never have been a spendable output on its own.
		//
		// Sub-dust value is creditable elsewhere in the system (the
		// msat-credit path), but the boarding transaction itself must
		// never carry a sub-dust output, so here we burn it to fee.
		default:
			fullPieces := int64(boardAmt) / int64(maxVTXO)
			boarded := btcutil.Amount(fullPieces * int64(maxVTXO))
			dustToFee = boardAmt - boarded
			boardAmt = boarded
			count = uint32(fullPieces)
		}
	}

	return &boardingClamp{
		BoardAmount: boardAmt,
		Change:      change,
		VTXOCount:   count,
		DustToFee:   dustToFee,
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
	if terms.MaxVTXOAmount == 0 && terms.MaxUserBalance == 0 {
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
		total, targetCount, terms.MaxVTXOAmount, headroom, floor,
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
//
// NOTE: this "current balance" definition deliberately differs from the
// receive pre-flight in swapwallet.checkReceiveLimits, which counts live
// VTXOs PLUS every boarding bucket (confirmed, unconfirmed, adopted). The
// boarding path is converting its confirmed boarding balance into VTXOs,
// so that confirmed balance is the `total` being clamped here -- counting
// it again in `current` would double-charge it against the cap. The
// receive path is adding funds on top of everything, so it counts all
// buckets. Keep the two definitions in sync in intent if either changes.
//
// Neither definition counts value promised by an in-flight round that has
// not yet confirmed (projected separately as VTXO_STATUS_PENDING_ROUND);
// the operator re-validates at round time, so this advisory headroom can
// briefly under-count without consequence.
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
