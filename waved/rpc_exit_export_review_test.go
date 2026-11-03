package waved

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExportExitRejectsMissingExpiryTree checks the evidence boundary itself
// and both RPC paths against a persisted tree without a root.
func TestExportExitRejectsMissingExpiryTree(t *testing.T) {
	t.Parallel()
	for _, missingPath := range []bool{false, true} {
		rpc, desc := newExitExportFixture(t)
		req := &ExitExportRequest{Outpoint: desc.Outpoint.String()}
		exported, err := rpc.ExportExit(t.Context(), req)
		require.NoError(t, err)
		if missingPath {
			desc.Ancestry[0].TreePath = nil
		} else {
			desc.Ancestry[0].TreePath.Root = nil
		}
		evidence, err := exportCommitmentSweeps(desc, exported.Ancestry)
		require.Nil(t, evidence)
		require.ErrorContains(t, err, "missing tree path or root")
		if missingPath {
			// The SQL schema rejects NULL paths. The helper still
			// defends its own boundary against a nil in-memory
			// path.
			continue
		}

		require.NoError(
			t,
			rpc.server.vtxoStore.DeleteVTXO(
				t.Context(), desc.Outpoint,
			),
		)
		require.NoError(
			t,
			rpc.server.vtxoStore.SaveVTXO(
				t.Context(), desc,
			),
		)
		exported, err = rpc.ExportExit(t.Context(), req)
		require.Nil(t, exported)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		exports, err := rpc.ExportExits(
			t.Context(), []*ExitExportRequest{req},
		)
		require.Nil(t, exports)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.ErrorContains(t, err, req.Outpoint)
	}
}

// TestExportExitsInvalidTargetContext identifies invalid and canceled targets
// as well as per-VTXO export failures, without exposing a partial backup.
func TestExportExitsInvalidTargetContext(t *testing.T) {
	t.Parallel()
	rpc, desc := newExitExportFixture(t)
	exports, err := rpc.ExportExits(t.Context(), []*ExitExportRequest{{
		Outpoint: "invalid-target",
	}})
	require.Nil(t, exports)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "invalid-target")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exports, err = rpc.ExportExits(ctx, []*ExitExportRequest{{
		Outpoint: desc.Outpoint.String(),
	}})
	require.Nil(t, exports)
	require.Equal(t, codes.Canceled, status.Code(err))
	require.ErrorContains(t, err, desc.Outpoint.String())
}

// TestExitExportTargetErrorPreservesStatus keeps machine-readable failures
// intact while identifying the target, including retry and cancellation codes.
func TestExitExportTargetErrorPreservesStatus(t *testing.T) {
	t.Parallel()
	detail := &errdetails.ErrorInfo{
		Reason: "TEST_EXPORT_FAILURE", Domain: "wavelength.test",
	}
	original, err := status.New(codes.FailedPrecondition,
		"missing sweep evidence").WithDetails(detail)
	require.NoError(t, err)
	err = exitExportTargetError("target:0", original.Err())
	require.Equal(t, original.Code(), status.Code(err))
	require.ErrorContains(t, err, "target:0")
	require.ErrorContains(t, err, original.Message())
	require.Equal(t, original.Details(), status.Convert(err).Details())
	for _, code := range []codes.Code{
		codes.NotFound, codes.InvalidArgument, codes.Unavailable,
		codes.Canceled, codes.DeadlineExceeded,
	} {
		err := exitExportTargetError(
			"target:1", status.Error(code,
				"cannot export"),
		)
		require.Equal(t, code, status.Code(err))
		require.ErrorContains(t, err, "target:1")
	}
}
