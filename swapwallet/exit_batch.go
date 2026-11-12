//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"strings"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExitBatch submits the exact selection once, preserving partial admission.
// It never retries or starts unilateral exit. A lower-layer dispatch error is
// unknown because the round actor may already have accepted the intent.
func (s *Service) ExitBatch(ctx context.Context,
	req *wavewalletrpc.ExitBatchRequest) (*wavewalletrpc.ExitBatchResponse,
	error) {

	if len(req.GetOutpoints()) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "outpoints list is empty",
		)
	}

	// Canonicalize before allocating an address or dispatching. Comparing
	// parsed identities also rejects duplicate spellings of the same input.
	targets := make([]string, 0, len(req.GetOutpoints()))
	seen := make(map[wire.OutPoint]struct{})
	for _, target := range req.GetOutpoints() {
		op, err := wire.NewOutPointFromString(target)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid outpoint %q: %v", target, err)
		}
		if _, duplicate := seen[*op]; duplicate {
			return nil, status.Errorf(codes.InvalidArgument,
				"duplicate outpoint %s", op)
		}
		seen[*op] = struct{}{}
		targets = append(targets, op.String())
	}

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	destination := strings.TrimSpace(req.GetOnchainAddress())
	if destination == "" {
		addr, err := s.deps.RPCServer.NewWalletAddress(ctx)
		if err != nil {
			return nil, status.Errorf(status.Code(err), "new "+
				"wallet address: %v", err)
		}
		destination = addr
	}

	// LeaveVTXOs validates the destination and preflights every explicit
	// target before reservation. It submits one composed round intent, even
	// with eager joining enabled. Do not replace this with singular calls.
	leave, err := s.deps.RPCServer.LeaveVTXOs(
		ctx, &waverpc.LeaveVTXOsRequest{
			Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: targets,
				},
			},
			DefaultDestination: &waverpc.LeaveDestination{
				Target: &waverpc.LeaveDestination_Address{
					Address: destination,
				},
			},
		},
	)
	if err != nil {
		// These codes are returned only by the daemon's pre-dispatch
		// validation/admission gates. Dispatch and actor failures are
		// Internal and cannot prove the absence of an accepted intent.
		code := status.Code(err)
		if code == codes.InvalidArgument ||
			code == codes.FailedPrecondition {
			return nil, err
		}
	}

	response := &wavewalletrpc.ExitBatchResponse{
		OnchainAddress: destination,
	}
	queued := make(map[string]bool)
	for _, target := range leave.GetQueuedOutpoints() {
		queued[target] = true
	}
	accepted := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED
	failed := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_FAILED
	unknown := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_UNKNOWN
	for _, target := range targets {
		result := &wavewalletrpc.ExitBatchInputResult{
			Outpoint: target,
			Outcome:  unknown,
		}
		switch {
		case err != nil:
			result.Error = err.Error()

		case queued[target]:
			result.Outcome = accepted

		case leave.GetErrors()[target] != "":
			result.Outcome = failed
			result.Error = leave.GetErrors()[target]

		default:
			result.Error = "daemon did not report input admission"
		}
		response.Results = append(response.Results, result)
	}

	return response, nil
}
