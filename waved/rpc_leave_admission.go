package waved

import (
	"context"
	"errors"
	"log/slog"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// admitLeaveTargets filters a cooperative-leave selection down to the VTXOs
// that can actually be committed to a round, so the caller learns why a
// target was refused here rather than from the VTXO manager's reservation
// gate — which reports the refusal as a raw FSM event-type mismatch
// (wavelength#577).
//
// The two selection modes want opposite failure behaviour, which is what
// strict selects. A caller naming outpoints explicitly must be told that the
// specific coin it asked for is unavailable; silently dropping it would
// report a queued leave that never happens. A caller asking for "all" has
// asked for whatever is leavable, so an in-flight VTXO is simply not part of
// the answer — and must be dropped, because ListLiveVTXOs returns every
// non-terminal VTXO, PendingForfeit and Forfeiting included.
//
// It returns the admitted targets alongside the number dropped. The count is
// what keeps the drop from being silent: a `leave --all` over a wallet whose
// coins are all committed to a round otherwise answers with an empty queued
// list and no errors, byte-identical to the same call against a wallet
// holding nothing. The operator could not tell "nothing to leave" from
// "everything was skipped, retry once the round confirms", and the daemon log
// held no record of which coins were skipped or why.
//
// This is an advisory pre-check. The reservation is only settled inside the
// VTXO actor's FSM, so a concurrent claim can still take a coin between this
// filter and the dispatch below; the FSM stays the authority for that race,
// and refuses the late claim in both PendingForfeit and Forfeiting.
func (r *RPCServer) admitLeaveTargets(ctx context.Context,
	targets []wire.OutPoint, strict bool) ([]wire.OutPoint, int, error) {

	// Without a store there is nothing to check against. Fall through to
	// the reservation gate rather than failing a request the daemon could
	// otherwise serve.
	if r.server.vtxoStore == nil {
		return targets, 0, nil
	}

	var skipped int

	admitted := make([]wire.OutPoint, 0, len(targets))
	for _, op := range targets {
		desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
		if err != nil {
			if errors.Is(err, vtxo.ErrVTXONotFound) {
				if !strict {
					skipped++

					r.server.log.InfoS(ctx, "Skipping "+
						"leave target: VTXO not found",
						slog.String(
							"outpoint", op.String(),
						),
					)

					continue
				}

				return nil, 0, status.Errorf(
					codes.InvalidArgument, "unknown VTXO "+
						"outpoint %s", op)
			}

			return nil, 0, status.Errorf(codes.Internal, "get "+
				"VTXO %s: %v", op, err)
		}

		if admitErr := vtxo.CheckForfeitAdmission(
			desc,
		); admitErr != nil {

			if !strict {
				skipped++

				// The admission error already names the round
				// holding the coin, so this line is actionable
				// as it stands: it says which coin was left
				// out of the batch and what has to resolve
				// before a retry can include it.
				r.server.log.InfoS(ctx, "Skipping leave "+
					"target: not admissible for forfeit",
					slog.String("outpoint", op.String()),
					slog.String(
						"status", desc.Status.String(),
					),
					slog.String("reason",
						admitErr.Error()),
				)

				continue
			}

			// FailedPrecondition rather than InvalidArgument: the
			// request is well formed and the outpoint is real, the
			// daemon is simply not in a state that can serve it.
			// The distinction matters to the caller, because most
			// of these resolve on their own once the round holding
			// the coin confirms.
			return nil, 0, status.Errorf(codes.FailedPrecondition,
				"cannot leave VTXO: %v", admitErr)
		}

		admitted = append(admitted, op)
	}

	return admitted, skipped, nil
}

// sweepAllTargets enumerates the VTXOs a sweep_all SendOnChain can forfeit.
// It is the sweep-all counterpart of the selection=all branch in LeaveVTXOs
// and applies the same admission check, because the two RPCs share the
// failure it guards against: ListLiveVTXOs returns every non-terminal VTXO,
// PendingForfeit and Forfeiting included, and the wallet reserves the sweep
// set strictly, so one coin sitting in an unconfirmed round (an automatic
// refresh that fired moments earlier, say) would otherwise fail the whole
// sweep with the VTXO manager's raw reservation error.
//
// An empty admitted set is reported as FailedPrecondition and distinguishes
// "nothing to sweep" from "everything is committed to a round", since the
// second resolves on its own once that round confirms and the first does
// not.
func (r *RPCServer) sweepAllTargets(ctx context.Context) ([]wire.OutPoint,
	error) {

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "vtxo store not "+
			"initialized")
	}

	liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list live VTXOs: %v",
			err)
	}
	if len(liveVTXOs) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "no live "+
			"VTXOs to sweep")
	}

	// ListLiveVTXOs already returned hydrated descriptors, so the
	// admission check runs on those rather than re-reading each row
	// through admitLeaveTargets; on a large wallet that round trip is
	// one extra transaction plus an ancestry query per coin.
	var skipped int
	admitted := make([]wire.OutPoint, 0, len(liveVTXOs))
	for _, desc := range liveVTXOs {
		admitErr := vtxo.CheckForfeitAdmission(desc)
		if admitErr == nil {
			admitted = append(admitted, desc.Outpoint)

			continue
		}

		skipped++

		// The admission error names what holds the coin, so the
		// line says which coin was left out of the sweep and what
		// has to resolve before a retry can include it.
		r.server.log.InfoS(ctx, "Skipping sweep-all target: not "+
			"admissible for forfeit",
			slog.String("outpoint", desc.Outpoint.String()),
			slog.String("status", desc.Status.String()),
			slog.String("reason", admitErr.Error()),
		)
	}

	if skipped > 0 {
		r.server.log.InfoS(ctx, "Sweep-all targets skipped",
			slog.Int("skipped_count", skipped),
			slog.Int("remaining_count", len(admitted)),
		)
	}

	if len(admitted) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "no "+
			"sweepable VTXOs: %d skipped (committed to a round or "+
			"spend-reserved); retry once they resolve", skipped)
	}

	return admitted, nil
}
