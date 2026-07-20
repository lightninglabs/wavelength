package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRefreshRealPathRejectsUnknownOutpoint pins that a real
// (non-dry-run) refresh validates its explicit selection against the
// VTXO store exactly as the dry-run preview does. Before this, the real
// path handed an unknown outpoint straight to the wallet actor as a
// logged per-outpoint error, returned an empty queued set, and let the
// follow-on round join fail with an opaque "no pending round"; the
// caller never saw the clean InvalidArgument the dry-run probe returns.
// The validation runs before the wallet-ready gate, so the harness needs
// no wallet wiring.
func TestRefreshRealPathRejectsUnknownOutpoint(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, _ := newRefreshEstimateServer(t, svc, 700)

	missing := newRefreshEstimateVTXO(t, 0x07, 10_000, 1_000)

	_, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointStr(missing),
					},
				},
			},
			DryRun: false,
		},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), outpointStr(missing))
}

// TestRefreshRealPathEmptyAllIsNoop pins that a real refresh --all
// against a store with no live VTXOs is a clean no-op rather than an
// error, and that it short-circuits before the wallet-ready gate (the
// harness wires no wallet actor, so reaching the gate would surface a
// wallet error instead of the queued no-op).
func TestRefreshRealPathEmptyAllIsNoop(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{responseFn: scaledEstimateFn}
	r, _ := newRefreshEstimateServer(t, svc, 700)

	resp, err := r.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
			DryRun: false,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "queued", resp.Status)
	require.Empty(t, resp.QueuedOutpoints)
}
