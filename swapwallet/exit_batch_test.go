//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"strings"
	"testing"

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

// TestExitBatchExactSelection proves one destination and one actual dispatch,
// including when only a subset is admitted by the lower layer.
func TestExitBatchExactSelection(t *testing.T) {
	t.Parallel()
	targets := batchExitTargets()
	svc, rpc := newAdminFixture(t)
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
			svc, rpc := newAdminFixture(t)
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
	svc, rpc := newAdminFixture(t)
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
	svc, rpc := newAdminFixture(t)
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
			svc, rpc := newAdminFixture(t)
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
