package waved

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
)

// ErrNoFeeFundingVTXO reports that the wallet holds no Bitcoin VTXO able to
// pay an asset round's fee. The message is the user's instruction, not a
// diagnostic: holding Bitcoin in Ark is what moving assets costs, the same
// way gas funds a token transfer.
var ErrNoFeeFundingVTXO = errors.New("moving assets needs Bitcoin in Ark to " +
	"pay the round fee: board Bitcoin first (ark board), then retry")

// feeFundingCandidate is a live Bitcoin-only VTXO the client could spend
// into an asset round so the round has a non-fixed output to charge its fee
// against.
type feeFundingCandidate struct {
	outpoint wire.OutPoint
	amount   btcutil.Amount
}

// feeFundingQuoter prices the round fee the given candidate would have to
// absorb. It is a seam so the selection rule can be tested without an
// operator, and so a quote failure degrades in one place.
type feeFundingQuoter func(ctx context.Context,
	candidate feeFundingCandidate) (btcutil.Amount, error)

// assetRoundFeeFunding describes the Bitcoin VTXO added to an asset round to
// carry its fee. Its residual returns to the client as change.
type assetRoundFeeFunding struct {
	// Outpoint is the forfeited Bitcoin VTXO.
	Outpoint wire.OutPoint

	// ValueSat is that VTXO's value. The operator stamps the round's
	// change output with ValueSat minus the fee at seal time.
	ValueSat int64
}

// selectFeeFundingVTXO picks the least-waste fee payer: candidates are
// walked smallest first and the first one that can absorb its own quoted
// fee and still leave floor behind wins, so a large VTXO is never churned
// when a small one suffices. The floor term is not optional headroom — the
// operator stamps the round's change output with (inputs - fixed outputs -
// fee) at seal time and rejects the client's whole intent when that
// residual lands below its minimum VTXO amount.
//
// Equal-value candidates tie-break on outpoint so the choice is stable
// across restarts and across the order the live-VTXO listing returns.
func selectFeeFundingVTXO(ctx context.Context, candidates []feeFundingCandidate,
	floor btcutil.Amount,
	quote feeFundingQuoter) (feeFundingCandidate, error) {

	ordered := make([]feeFundingCandidate, len(candidates))
	copy(ordered, candidates)
	sort.Slice(ordered, func(i, j int) bool {
		return preferFeeFunding(ordered[i], ordered[j])
	})

	for _, candidate := range ordered {
		fee, err := quote(ctx, candidate)
		if err != nil {
			return feeFundingCandidate{}, err
		}
		if candidate.amount < fee+floor {
			continue
		}

		return candidate, nil
	}

	return feeFundingCandidate{}, ErrNoFeeFundingVTXO
}

// preferFeeFunding orders candidates by ascending value, falling back to
// outpoint order so ties are deterministic.
func preferFeeFunding(a, b feeFundingCandidate) bool {
	if a.amount != b.amount {
		return a.amount < b.amount
	}

	cmp := bytes.Compare(a.outpoint.Hash[:], b.outpoint.Hash[:])
	if cmp != 0 {
		return cmp < 0
	}

	return a.outpoint.Index < b.outpoint.Index
}

// hasFeeFundingSlot reports whether the accumulated VTXO requests already
// contain an output the client owns whose amount the operator may shrink.
// That output is the round's fee payer, so a caller that finds one must not
// add a second.
//
// The IsChange marker itself is not what to look for: it is stamped
// centrally by the round FSM once the intent leaves assembly, so during
// assembly a fee-bearing slot is simply a non-fixed request the client
// owns. Asset requests are pinned with FixedAmount so the seal quote cannot
// shrink a carrier, which is exactly why an asset-only intent has no slot.
func hasFeeFundingSlot(reqs []types.VTXORequest) bool {
	for i := range reqs {
		req := &reqs[i]
		if !req.FixedAmount && req.HasLocalOwner() {
			return true
		}
	}

	return false
}

// assemblingRoundHasFeeFundingSlot reports whether the round the client is
// currently assembling intents into already carries a fee-bearing slot the
// client owns — because it is boarding Bitcoin in the same round, or
// refreshing a Bitcoin VTXO into it.
//
// Only PendingRoundAssembly is inspected. New intents join through the
// round actor's findAssemblingRound, which returns either that state or
// Idle, and an Idle round holds no requests at all. The actor assembles one
// round at a time, so the first assembling round found is the one a new
// intent joins.
func (s *Server) assemblingRoundHasFeeFundingSlot(ctx context.Context) (bool,
	error) {

	if s.actorSystem == nil {
		return false, fmt.Errorf("actor system not initialized")
	}

	roundRef := round.NewServiceKey().Ref(s.actorSystem)
	resp, err := roundRef.Ask(
		ctx, &round.GetClientStateRequest{},
	).Await(ctx).Unpack()
	if err != nil {
		return false, fmt.Errorf("query round state: %w", err)
	}

	stateResp, ok := resp.(*round.GetClientStateResponse)
	if !ok {
		return false, fmt.Errorf("unexpected round state response %T",
			resp)
	}

	for _, info := range stateResp.States {
		assembling, ok := info.State.(*round.PendingRoundAssembly)
		if !ok {
			continue
		}
		if hasFeeFundingSlot(assembling.VTXOs) {
			return true, nil
		}
	}

	return false, nil
}

// assetRoundFeeQuoter prices what a candidate must absorb: the round fee
// covers both the asset boarding input and the Bitcoin forfeit this adds,
// so both legs are quoted. The boarding leg is hoisted out of the walk
// since it does not vary with the candidate.
//
// remainingBlocks is left at zero deliberately. The operator reads zero as
// "price the full sweep-delay lifetime", which is the correct shape for a
// coin being refreshed into a fresh batch and errs high rather than low —
// the point of the quote is to keep a doomed round from being assembled,
// and the binding fee is still the operator's own at seal time.
//
// A quote the operator cannot serve degrades to the advertised flat
// minimum rather than failing the board: an unavailable estimate is not
// evidence the client cannot pay, and the seal handshake remains the
// authority either way.
func (s *Server) assetRoundFeeQuoter(boardedValueSat int64,
	fallback btcutil.Amount) feeFundingQuoter {

	var (
		boardingFee   btcutil.Amount
		boardingKnown bool
	)

	return func(ctx context.Context, candidate feeFundingCandidate) (
		btcutil.Amount, error) {

		if !boardingKnown {
			fee, err := s.quoteOperatorFee(
				ctx, boardedValueSat, true, /* isBoarding */
				0,
			)
			if err != nil {
				s.log.WarnS(ctx, "Asset round fee funding: "+
					"boarding quote unavailable", err)

				return fallback, nil
			}

			boardingFee = fee
			boardingKnown = true
		}

		forfeitFee, err := s.quoteOperatorFee(
			ctx, int64(candidate.amount), false, /* isBoarding */
			0,
		)
		if err != nil {
			s.log.WarnS(ctx, "Asset round fee funding: forfeit "+
				"quote unavailable", err)

			return fallback, nil
		}

		return boardingFee + forfeitFee, nil
	}
}

// fundAssetRoundFee makes sure the round an asset intent is about to join
// can pay its own fee. An asset VTXO request is fixed-amount, so a round
// holding nothing else gives the operator no output to charge against and
// its seal-time quote rejects the whole intent. When the client has no
// fee-bearing slot in that round yet, one Bitcoin VTXO is refreshed into
// it: the forfeit adds input value and the refresh output is the non-fixed
// slot the residual lands on, returning the remainder as change.
//
// Refreshing is what the wallet already does for exactly this shape, so
// this selects the coin and delegates; nothing here re-implements forfeit
// composition or reservation.
//
// A nil funding with no error means the round already had a slot and
// nothing was added.
func (s *Server) fundAssetRoundFee(ctx context.Context, boardedValueSat int64) (
	*assetRoundFeeFunding, error) {

	hasSlot, err := s.assemblingRoundHasFeeFundingSlot(ctx)
	if err != nil {
		return nil, err
	}
	if hasSlot {
		s.log.InfoS(
			ctx, "Asset round already carries a fee-bearing "+
				"output; adding none",
		)

		return nil, nil
	}

	if s.vtxoStore == nil {
		return nil, fmt.Errorf("vtxo store not initialized")
	}

	// Only strictly Live coins qualify. The broader non-terminal listing
	// also returns VTXOs already committed elsewhere (PendingForfeit,
	// Forfeiting, Spending), and picking one of those would just fail the
	// wallet's reservation.
	rows, err := s.vtxoStore.ListSelectionCandidatesByStatus(
		ctx, vtxo.VTXOStatusLive,
	)
	if err != nil {
		return nil, fmt.Errorf("list selection candidates: %w", err)
	}

	// An asset-bearing VTXO cannot pay the fee: its carrier value is
	// pinned by the asset transition that reissues its units.
	candidates := make([]feeFundingCandidate, 0, len(rows))
	for _, row := range rows {
		if row.TaprootAssetRoot != nil {
			continue
		}

		candidates = append(candidates, feeFundingCandidate{
			outpoint: row.Outpoint,
			amount:   row.Amount,
		})
	}

	terms, err := s.fetchCachedOperatorTerms(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch operator terms: %w", err)
	}
	if terms == nil {
		return nil, fmt.Errorf("operator terms are not ready")
	}

	floor := terms.MinVTXOAmountFloor()
	selected, err := selectFeeFundingVTXO(
		ctx, candidates, floor,
		s.assetRoundFeeQuoter(boardedValueSat, terms.MinOperatorFee),
	)
	if err != nil {
		s.log.WarnS(ctx, "No Bitcoin VTXO can fund the asset round fee",
			err,
			slog.Int("candidates", len(candidates)),
			slog.Int("floor_sat", int(floor)),
		)

		return nil, err
	}

	if err := s.refreshFeeFundingVTXO(ctx, selected.outpoint); err != nil {
		return nil, err
	}

	s.log.InfoS(ctx, "Taproot Asset round fee funded from a Bitcoin VTXO",
		slog.String("outpoint", selected.outpoint.String()),
		slog.Int("value_sat", int(selected.amount)),
		slog.Int("floor_sat", int(floor)),
	)

	return &assetRoundFeeFunding{
		Outpoint: selected.outpoint,
		ValueSat: int64(selected.amount),
	}, nil
}

// refreshFeeFundingVTXO refreshes the selected Bitcoin VTXO into the
// assembling round through the wallet, which reserves the forfeit and
// composes the non-fixed replacement output the round's fee is charged
// against.
func (s *Server) refreshFeeFundingVTXO(ctx context.Context,
	outpoint wire.OutPoint) error {

	if !s.walletRef.IsSome() {
		return fmt.Errorf("wallet actor not initialized")
	}
	walletRef := s.walletRef.UnsafeFromSome()

	// The refresh registers a round intent, so the same detachment the
	// other round-joining paths use applies: this call returning must not
	// cancel the registration handshake mid-flight.
	askCtx := context.WithoutCancel(ctx)
	resp, err := walletRef.Ask(askCtx, &wallet.RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{
			outpoint,
		},
		ForceRefresh: true,
	}).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("refresh fee funding VTXO: %w", err)
	}

	refreshResp, ok := resp.(*wallet.RefreshVTXOsResponse)
	if !ok {
		return fmt.Errorf("unexpected refresh response %T", resp)
	}

	// A per-outpoint error means the wallet never composed the forfeit,
	// so the round still has no fee-bearing output.
	if refreshErr, ok := refreshResp.Errors[outpoint]; ok {
		return fmt.Errorf("refresh fee funding VTXO %s: %w", outpoint,
			refreshErr)
	}
	if refreshResp.RefreshingCount == 0 {
		return fmt.Errorf("refresh of fee funding VTXO %s was "+
			"not queued", outpoint)
	}

	return nil
}
