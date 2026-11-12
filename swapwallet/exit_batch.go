//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
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

	// Capture amounts before dispatch; accepted inputs may disappear from
	// spendable inventory immediately. Missing metadata rejects before any
	// dispatch; the daemon still owns eligibility and reservation below.
	inventory, err := s.deps.RPCServer.ListVTXOs(
		ctx, &waverpc.ListVTXOsRequest{
			ExcludeCheckpointPsbts: true,
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "read "+
			"exit inputs before admission: %v", err)
	}
	amounts := make(map[string]int64, len(targets))
	for _, input := range inventory.GetVtxos() {
		amounts[input.GetOutpoint()] = input.GetAmountSat()
	}
	var selectedAmount int64
	for _, target := range targets {
		if amounts[target] <= 0 {
			return nil, status.Errorf(codes.FailedPrecondition,
				"exit input %s is not in current inventory",
				target)
		}
		if amounts[target] > math.MaxInt64-selectedAmount {
			return nil, status.Error(
				codes.InvalidArgument,
				"selected exit amount overflows",
			)
		}
		selectedAmount += amounts[target]
	}

	// A fresh activity identity distinguishes a later explicit attempt from
	// an older failed one. It is not an RPC replay key.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "allocate "+
			"exit activity identity: %v", err)
	}
	activityPrefix := "batch-exit-" + hex.EncodeToString(nonce[:]) + "-"

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

	// All positively admitted inputs belong to the one composed intent.
	// Project one aggregate row: each source reports the same round-level
	// fee, so one row per input would count that fee repeatedly.
	var admitted []string
	var amount int64
	for _, input := range response.Results {
		if input.Outcome != accepted {
			continue
		}
		admitted = append(admitted, input.Outpoint)
		amount += amounts[input.Outpoint]
	}
	if len(admitted) > 0 && s.runtime != nil {
		entry := leaveEntryStub(
			activityPrefix+admitted[0], admitted, destination,
			amount, "", false,
		)
		// Cancellation after admission cannot discard the row. Existing
		// EXIT rehydration and reconciliation follow the composed
		// intent through restart and confirmation of its first admitted
		// source.
		s.runtime.trackPendingEntryWithoutTimeout(entry)
		s.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)
		response.ActivityId = entry.Id
	}

	return response, nil
}
