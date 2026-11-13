//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// batchExitTargets supplies two distinct canonical inputs without live funds.
func batchExitTargets() []string {
	return []string{strings.Repeat("a", 64) + ":0",
		strings.Repeat("b", 64) + ":1"}
}

// newBatchExitFixture supplies the pre-dispatch inventory used to retain
// each accepted input's original amount in canonical activity.
func newBatchExitFixture(t *testing.T) (*Service, *fakeRPCServer) {
	t.Helper()
	svc, rpc := newAdminFixture(t)
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{}
	for _, target := range batchExitTargets() {
		rpc.listVTXOsResp.Vtxos = append(
			rpc.listVTXOsResp.Vtxos, &waverpc.VTXO{
				Outpoint:  target,
				AmountSat: 5000,
			},
		)
	}

	return svc, rpc
}

// TestExitBatchExactSelection proves one destination and one actual dispatch,
// including when only a subset is admitted by the lower layer.
func TestExitBatchExactSelection(t *testing.T) {
	t.Parallel()
	targets := batchExitTargets()
	svc, rpc := newBatchExitFixture(t)
	rpc.newWalletAddressResp = "wallet-address"
	rpc.leaveResp = &waverpc.LeaveVTXOsResponse{
		QueuedOutpoints: targets[:1],
		Errors: map[string]string{
			targets[1]: "descriptor unavailable",
		},
	}
	response, err := svc.ExitBatch(
		t.Context(), &wavewalletrpc.ExitBatchRequest{
			Outpoints: targets,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, rpc.leaveCalls)
	require.Zero(t, rpc.unrollCalls)
	require.Equal(
		t, targets, rpc.leaveLastReq.GetOutpoints().GetOutpoints(),
	)
	require.Equal(t, "wallet-address", response.OnchainAddress)
	require.Equal(
		t, "wallet-address",
		rpc.leaveLastReq.GetDefaultDestination().GetAddress(),
	)
	require.Len(t, response.Results, 2)
	require.Equal(t, targets[0], response.Results[0].Outpoint)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED,
		response.Results[0].Outcome,
	)
	require.Equal(t, targets[1], response.Results[1].Outpoint)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_FAILED,
		response.Results[1].Outcome,
	)
	require.Equal(t, "descriptor unavailable", response.Results[1].Error)
	entries := svc.runtime.pendingSnapshot()
	require.Len(t, entries, 1)
	require.Equal(t, targets[0], entries[0].GetProgress().GetVtxoOutpoint())
	require.EqualValues(t, -5000, entries[0].GetAmountSat())
	require.Equal(
		t, "wallet-address",
		entries[0].GetRequest().GetOnchainAddress().GetAddress(),
	)

}

// TestExitBatchRejectsInvalidSelection proves invalid inputs have no side
// effects.
func TestExitBatchRejectsInvalidSelection(t *testing.T) {
	t.Parallel()
	targets := batchExitTargets()
	for name, inputs := range map[string][]string{
		"empty": nil,
		"empty entry": {
			targets[0],
			"",
		},
		"malformed": {
			targets[0],
			"bad",
		},
		"duplicate": {
			targets[0],
			targets[0],
		},
		"alias": {
			targets[0],
			strings.ToUpper(targets[0]),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, rpc := newBatchExitFixture(t)
			// If address allocation runs, this error would replace
			// the expected selection error. Dispatch must also
			// remain absent.
			rpc.newWalletAddressErr = errors.New("must not " +
				"allocate")
			_, err := svc.ExitBatch(
				t.Context(), &wavewalletrpc.ExitBatchRequest{
					Outpoints: inputs,
				},
			)
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
			)
			require.Zero(t, rpc.leaveCalls)
			require.Zero(t, rpc.unrollCalls)
		})
	}
}

// batchExitRPC models a response lost after the lower layer accepted the
// intent.
type batchExitRPC struct {
	*fakeRPCServer
	accepted []string
}

// LeaveVTXOs records the side effect then loses its response, with no replay.
func (r *batchExitRPC) LeaveVTXOs(_ context.Context,
	req *waverpc.LeaveVTXOsRequest) (*waverpc.LeaveVTXOsResponse, error) {

	r.leaveCalls++
	r.accepted = append(r.accepted, req.GetOutpoints().GetOutpoints()...)

	return nil, status.Error(codes.DeadlineExceeded, "response lost")
}

// TestExitBatchAmbiguousResponseNeverReplays proves accepted work stays unknown
// after response loss and neither single-exit nor unilateral fallback runs.
func TestExitBatchAmbiguousResponseNeverReplays(t *testing.T) {
	t.Parallel()
	svc, rpc := newBatchExitFixture(t)
	lower := &batchExitRPC{fakeRPCServer: rpc}
	svc.deps.RPCServer = lower
	targets := batchExitTargets()
	response, err := svc.ExitBatch(
		t.Context(), &wavewalletrpc.ExitBatchRequest{
			Outpoints:      targets,
			OnchainAddress: "supplied-address",
		},
	)
	require.NoError(t, err)
	require.Empty(t, svc.runtime.pendingSnapshot())
	require.Equal(t, targets, lower.accepted)
	require.Equal(t, 1, lower.leaveCalls)
	require.Zero(t, lower.unrollCalls)
	require.Len(t, response.Results, 2)
	for _, result := range response.Results {
		require.Equal(
			t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_UNKNOWN,
			result.Outcome,
		)
		require.Contains(t, result.Error, "response lost")
	}
}

// TestExitBatchMissingEvidenceIsUnknown prevents an incomplete daemon response
// from turning an omitted target into a definitive failure or success.
func TestExitBatchMissingEvidenceIsUnknown(t *testing.T) {
	t.Parallel()
	svc, rpc := newBatchExitFixture(t)
	targets := batchExitTargets()
	rpc.leaveResp = &waverpc.LeaveVTXOsResponse{
		QueuedOutpoints: targets[:1],
	}
	response, err := svc.ExitBatch(
		t.Context(), &wavewalletrpc.ExitBatchRequest{
			Outpoints:      targets,
			OnchainAddress: "supplied-address",
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED,
		response.Results[0].Outcome,
	)
	require.Equal(
		t, wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_UNKNOWN,
		response.Results[1].Outcome,
	)
}

// TestExitBatchPreAdmissionFailure preserves definitive validation/state
// refusal.
func TestExitBatchPreAdmissionFailure(t *testing.T) {
	t.Parallel()
	for _, code := range []codes.Code{
		codes.InvalidArgument,
		codes.FailedPrecondition,
	} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			svc, rpc := newBatchExitFixture(t)
			rpc.leaveErr = status.Error(code, "not admitted")
			response, err := svc.ExitBatch(
				t.Context(), &wavewalletrpc.ExitBatchRequest{
					Outpoints:      batchExitTargets(),
					OnchainAddress: "supplied-address",
				},
			)
			require.Equal(t, code, status.Code(err))
			require.Nil(t, response)
			require.Equal(t, 1, rpc.leaveCalls)
			require.Zero(t, rpc.unrollCalls)
		})
	}
}

// cancelAfterBatchRPC cancels the caller only after positive local admission.
type cancelAfterBatchRPC struct {
	*fakeRPCServer
	cancel context.CancelFunc
}

// LeaveVTXOs returns admission evidence after the caller stops waiting.
func (r *cancelAfterBatchRPC) LeaveVTXOs(_ context.Context,
	req *waverpc.LeaveVTXOsRequest) (*waverpc.LeaveVTXOsResponse, error) {

	r.leaveCalls++
	r.leaveLastReq = req
	r.cancel()

	return r.leaveResp, nil
}

// GetUnrollStatus enforces the canonical outpoint boundary for batch inputs.
// A prefixed activity id must fail here as it does in the real daemon.
func (r *cancelAfterBatchRPC) GetUnrollStatus(ctx context.Context,
	req *waverpc.GetUnrollStatusRequest) (*waverpc.GetUnrollStatusResponse,
	error) {

	if _, err := wire.NewOutPointFromString(req.GetOutpoint()); err != nil {
		return nil, err
	}

	return r.fakeRPCServer.GetUnrollStatus(ctx, req)
}

// TestExitBatchActivitySurvivesCancellationAndRestart proves one aggregate
// row retains principal and destination, resumes, and books a shared fee once.
func TestExitBatchActivitySurvivesCancellationAndRestart(t *testing.T) {
	t.Parallel()
	h, store := newStoreListFixture(t)
	_, rpc := newBatchExitFixture(t)
	targets := batchExitTargets()
	rpc.leaveResp = &waverpc.LeaveVTXOsResponse{QueuedOutpoints: targets}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.deps.RPCServer = &cancelAfterBatchRPC{
		fakeRPCServer: rpc,
		cancel:        cancel,
	}
	h.deps.SwapService = &fakeSwapService{}
	svc := newService(h.deps, h.runtime)
	response, err := svc.ExitBatch(ctx, &wavewalletrpc.ExitBatchRequest{
		Outpoints: targets, OnchainAddress: "destination",
	})
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NotEmpty(t, response.ActivityId)
	row, err := store.GetEntry(t.Context(), response.ActivityId)
	require.NoError(t, err)
	entry, err := rowToWalletEntry(row)
	require.NoError(t, err)
	require.EqualValues(t, -10000, entry.AmountSat)
	require.Equal(
		t, "destination", entry.Request.GetOnchainAddress().Address,
	)
	require.Equal(t, targets[0], entry.Progress.VtxoOutpoint)
	require.Len(t, h.runtime.pendingSnapshot(), 1)
	h.runtime.stop()

	resumed := newRuntime(t.Context(), h.deps)
	defer resumed.stop()
	resumed.rehydrateWalletLocalPending(t.Context())
	require.Len(t, resumed.pendingSnapshot(), 1)
	resumed.applyDeadlines(time.Now().Add(24 * time.Hour))
	_, timedOut := resumed.overlayFor(response.ActivityId)
	require.False(t, timedOut)
	forfeited := waverpc.VTXOStatus_VTXO_STATUS_FORFEITED
	settled := &waverpc.ListVTXOsResponse{}
	for _, target := range targets {
		settled.Vtxos = append(settled.Vtxos, &waverpc.VTXO{
			Outpoint: target,
			Settlement: &waverpc.VTXOSettlement{
				Txid:   strings.Repeat("c", 64),
				Height: 42,
				FeeSat: 73,
			},
		})
	}
	rpc.listVTXOsByStatus =
		map[waverpc.VTXOStatus]*waverpc.ListVTXOsResponse{
			forfeited: settled,
		}

	_, err = resumed.reprojectActivity(t.Context(), reconcilerKinds, true)
	require.NoError(t, err)
	row, err = store.GetEntry(t.Context(), response.ActivityId)
	require.NoError(t, err)
	entry, err = rowToWalletEntry(row)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.Status,
	)
	require.EqualValues(t, 73, entry.FeeSat)
	require.EqualValues(t, -9927, entry.AmountSat)
	require.Equal(t, strings.Repeat("c", 64), entry.Progress.Txid)
	rpc.mu.Lock()
	queriedOutpoint := rpc.unrollStatusLast.GetOutpoint()
	rpc.mu.Unlock()
	require.Equal(t, targets[0], queriedOutpoint)
}

// TestExitBatchInventoryFailureNeverDispatches prevents missing principal
// metadata from creating an accepted exit with a fabricated zero amount.
func TestExitBatchInventoryFailureNeverDispatches(t *testing.T) {
	t.Parallel()
	svc, rpc := newBatchExitFixture(t)
	rpc.listVTXOsErr = errors.New("inventory unavailable")
	_, err := svc.ExitBatch(t.Context(), &wavewalletrpc.ExitBatchRequest{
		Outpoints: batchExitTargets(),
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Zero(t, rpc.leaveCalls)
	require.Empty(t, svc.runtime.pendingSnapshot())
}
