//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"strings"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ExitBatch submits the exact selection once, preserving partial admission.
// It never retries or starts unilateral exit. A lower-layer dispatch error is
// unknown because the round actor may already have accepted the intent.
func (s *Service) ExitBatch(ctx context.Context,
	req *wavewalletrpc.ExitBatchRequest) (*wavewalletrpc.ExitBatchResponse,
	error) {

	return s.exitBatch(ctx, req, "")
}

// keyedExitBackend commits the receipt with the durable round state.
type keyedExitBackend interface {
	// BeginExitReceipt claims the one-shot ID before any reservation.
	BeginExitReceipt(context.Context, string) error

	// LeaveWithReceipt records the actual subset with the accepted round.
	LeaveWithReceipt(context.Context, *waverpc.LeaveVTXOsRequest, string,
		func(map[string]string) ([]byte, error)) (
		*waverpc.LeaveVTXOsResponse, error)

	// ResolveExitReceipt returns the receipt or closes unused admission.
	ResolveExitReceipt(context.Context, string) ([]byte, error)
}

// exitRequestID validates the single-use request identity before touching
// funds.
func exitRequestID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return "", status.Error(
			codes.InvalidArgument,
			"a nonzero exit request UUID is required",
		)
	}

	return id.String(), nil
}

// SubmitExitBatch queues a reviewed selection exactly once under a fresh ID.
func (s *Service) SubmitExitBatch(ctx context.Context,
	req *wavewalletrpc.SubmitExitBatchRequest) (
	*wavewalletrpc.ExitBatchResponse, error) {

	id, err := exitRequestID(req.GetRequestId())
	if err != nil {
		return nil, err
	}
	backend, err := s.keyedExitBackend()
	if err != nil {
		return nil, err
	}
	if err := backend.BeginExitReceipt(ctx, id); err != nil {
		return nil, err
	}

	return s.exitBatch(ctx, req.GetSelection(), id)
}

// ResolveExitBatch fences late submissions or restores their original result.
func (s *Service) ResolveExitBatch(ctx context.Context,
	req *wavewalletrpc.ResolveExitBatchRequest) (
	*wavewalletrpc.ResolveExitBatchResponse, error) {

	id, err := exitRequestID(req.GetRequestId())
	if err != nil {
		return nil, err
	}
	backend, err := s.keyedExitBackend()
	if err != nil {
		return nil, err
	}
	raw, err := backend.ResolveExitReceipt(ctx, id)
	if err != nil {
		return nil, err
	}
	reply := &wavewalletrpc.ResolveExitBatchResponse{}
	if len(raw) > 0 {
		reply.Admission = &wavewalletrpc.ExitBatchResponse{}
		if err := proto.Unmarshal(raw, reply.Admission); err != nil {
			return nil, err
		}
		if err := s.restoreExitActivity(
			ctx, reply.Admission,
		); err != nil {
			return nil, err
		}
	}

	return reply, nil
}

// keyedExitBackend checks that the same wallet owns admission and recovery.
func (s *Service) keyedExitBackend() (keyedExitBackend, error) {
	if s.deps == nil || s.runtime == nil || s.deps.ActivityStore == nil {
		return nil, status.Error(
			codes.Unavailable, "wallet activity store unavailable",
		)
	}
	b, ok := s.deps.RPCServer.(keyedExitBackend)
	if !ok {
		return nil, status.Error(
			codes.Unimplemented,
			"keyed cooperative exits unavailable",
		)
	}

	return b, nil
}

// exitBatch validates the selection before choosing its admission boundary.
func (s *Service) exitBatch(ctx context.Context,
	req *wavewalletrpc.ExitBatchRequest, id string) (
	*wavewalletrpc.ExitBatchResponse, error) {

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
	activityID := "batch-exit-" + hex.EncodeToString(nonce[:])

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
	leaveReq := &waverpc.LeaveVTXOsRequest{
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
	}

	var leave *waverpc.LeaveVTXOsResponse
	if id != "" {
		backend, err := s.keyedExitBackend()
		if err != nil {
			return nil, err
		}
		encode := func(failures map[string]string) ([]byte, error) {
			accepted := &waverpc.LeaveVTXOsResponse{
				Errors: failures,
			}
			for _, target := range targets {
				if _, failed := failures[target]; !failed {
					accepted.QueuedOutpoints = append(
						accepted.QueuedOutpoints,
						target,
					)
				}
			}

			return proto.Marshal(
				batchExitResult(
					targets, amounts, destination,
					activityID, accepted, nil,
				),
			)
		}
		leave, err = backend.LeaveWithReceipt(ctx, leaveReq, id, encode)
		if err != nil {
			return nil, err
		}
		result := batchExitResult(
			targets, amounts, destination, activityID, leave, nil,
		)
		if result.ActivityId == "" {
			receipt, err := backend.ResolveExitReceipt(ctx, id)
			if err != nil {
				return nil, err
			}
			if len(receipt) > 0 {
				return nil, status.Error(
					codes.AlreadyExists,
					"exit request ID already admitted",
				)
			}

			return result, nil
		}
		if err := s.restoreExitActivity(
			context.WithoutCancel(ctx), result,
		); err != nil {
			return nil, err
		}

		return result, nil
	}

	leave, err = s.deps.RPCServer.LeaveVTXOs(ctx, leaveReq)
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

	result := batchExitResult(
		targets, amounts, destination, activityID, leave, err,
	)
	if result.ActivityId != "" && s.runtime != nil {
		entry := batchExitEntry(result)
		s.runtime.trackPendingEntryWithoutTimeout(entry)
		s.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)
	} else {
		result.ActivityId = ""
	}

	return result, nil
}

// batchExitResult preserves exactly which inputs entered the composed round.
func batchExitResult(targets []string, amounts map[string]int64, destination,
	activityID string, leave *waverpc.LeaveVTXOsResponse,
	err error) *wavewalletrpc.ExitBatchResponse {

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
			response.AcceptedAmountSat += amounts[target]
			response.ActivityId = activityID

		case leave.GetErrors()[target] != "":
			result.Outcome = failed
			result.Error = leave.GetErrors()[target]

		default:
			result.Error = "daemon did not report input admission"
		}
		response.Results = append(response.Results, result)
	}

	return response
}

// batchExitEntry builds the single activity row tracking the accepted subset.
func batchExitEntry(
	result *wavewalletrpc.ExitBatchResponse) *wavewalletrpc.WalletEntry {

	var admitted []string
	for _, input := range result.Results {
		if input.Outcome == wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED {
			admitted = append(admitted, input.Outpoint)
		}
	}

	return leaveEntryStub(
		result.ActivityId, admitted, result.OnchainAddress,
		result.AcceptedAmountSat, "", true,
	)
}

// restoreExitActivity fills the projection gap after a lost reply or restart.
// Existing activity may already be terminal and must never be reopened.
func (s *Service) restoreExitActivity(ctx context.Context,
	result *wavewalletrpc.ExitBatchResponse) error {

	if result.ActivityId == "" {
		return nil
	}
	r := s.runtime
	r.projectMu.Lock()
	defer r.projectMu.Unlock()
	_, err := r.deps.ActivityStore.GetEntry(ctx, result.ActivityId)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	entry := batchExitEntry(result)
	seq, effective, err := r.project(ctx, entry)
	if err != nil {
		return err
	}
	r.trackPendingEntryWithoutTimeout(entry)
	if seq > 0 {
		r.emit(seq, effective)
	}

	return nil
}
