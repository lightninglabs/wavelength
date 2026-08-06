package waved

import (
	"context"
	"errors"

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
// This is an advisory pre-check. The reservation is only settled inside the
// VTXO actor's FSM, so a concurrent claim can still take a coin between this
// filter and the dispatch below; the FSM stays the authority for that race.
func (r *RPCServer) admitLeaveTargets(ctx context.Context,
	targets []wire.OutPoint, strict bool) ([]wire.OutPoint, error) {

	// Without a store there is nothing to check against. Fall through to
	// the reservation gate rather than failing a request the daemon could
	// otherwise serve.
	if r.server.vtxoStore == nil {
		return targets, nil
	}

	admitted := make([]wire.OutPoint, 0, len(targets))
	for _, op := range targets {
		desc, err := r.server.vtxoStore.GetVTXO(ctx, op)
		if err != nil {
			if errors.Is(err, vtxo.ErrVTXONotFound) {
				if !strict {
					continue
				}

				return nil, status.Errorf(codes.InvalidArgument,
					"unknown VTXO outpoint %s", op)
			}

			return nil, status.Errorf(codes.Internal, "get VTXO "+
				"%s: %v", op, err)
		}

		if admitErr := vtxo.CheckForfeitAdmission(
			desc,
		); admitErr != nil {

			if !strict {
				continue
			}

			// FailedPrecondition rather than InvalidArgument: the
			// request is well formed and the outpoint is real, the
			// daemon is simply not in a state that can serve it.
			// The distinction matters to the caller, because most
			// of these resolve on their own once the round holding
			// the coin confirms.
			return nil, status.Errorf(codes.FailedPrecondition,
				"cannot leave VTXO: %v", admitErr)
		}

		admitted = append(admitted, op)
	}

	return admitted, nil
}
