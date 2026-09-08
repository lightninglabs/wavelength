//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestListVTXOs drives the ListVTXOs status selection
// through a full daemon: the default listing hides forfeited and spent rows,
// status lists select any set, and the spendable balance ignores the extra
// rows.
func TestListVTXOs(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t)

	// The fake operator never completes a round or an OOR spend, so the
	// terminal rows are seeded while the daemon is stopped.
	fixture.shutdown()
	seeded := map[vtxo.VTXOStatus]wire.OutPoint{
		vtxo.VTXOStatusLive: fixture.seededOutpoint,
	}
	for label, vtxoStatus := range map[string]vtxo.VTXOStatus{
		"forfeited": vtxo.VTXOStatusForfeited,
		"spent":     vtxo.VTXOStatusSpent,
		"failed":    vtxo.VTXOStatusFailed,
	} {
		seeded[vtxoStatus] = seedVTXO(
			t, fixture.cfg, fixture.operatorKey,
			btcutil.Amount(testSeededAmountSat), label, vtxoStatus,
		)
	}
	fixture.launch()

	list := func(req *waverpc.ListVTXOsRequest) ([]*waverpc.VTXO, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		resp, err := fixture.client.ListVTXOs(ctx, req)
		if err != nil {
			return nil, err
		}

		return resp.Vtxos, nil
	}
	outpoints := func(vtxos []*waverpc.VTXO) []string {
		out := make([]string, 0, len(vtxos))
		for _, v := range vtxos {
			out = append(out, v.Outpoint)
		}

		return out
	}
	expect := func(statuses ...vtxo.VTXOStatus) []string {
		out := make([]string, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, outpointString(seeded[s]))
		}

		return out
	}

	inventory, err := list(&waverpc.ListVTXOsRequest{})
	require.NoError(t, err)
	require.ElementsMatch(
		t, expect(vtxo.VTXOStatusLive, vtxo.VTXOStatusFailed),
		outpoints(inventory),
	)

	consumed, err := list(&waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
			waverpc.VTXOStatus_VTXO_STATUS_SPENT,
		},
	})
	require.NoError(t, err)
	require.ElementsMatch(
		t, expect(vtxo.VTXOStatusForfeited, vtxo.VTXOStatusSpent),
		outpoints(consumed),
	)

	live, err := list(&waverpc.ListVTXOsRequest{
		StatusFilter: waverpc.VTXOStatus_VTXO_STATUS_LIVE,
	})
	require.NoError(t, err)
	require.Equal(t, expect(vtxo.VTXOStatusLive), outpoints(live))

	// The full enum is what the CLI sends for --all; pending_round hits
	// the live round actor and contributes nothing here.
	all, err := list(&waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
			waverpc.VTXOStatus_VTXO_STATUS_FORFEITING,
			waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
			waverpc.VTXOStatus_VTXO_STATUS_SPENT,
			waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
			waverpc.VTXOStatus_VTXO_STATUS_FAILED,
			waverpc.VTXOStatus_VTXO_STATUS_SPENDING,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
			waverpc.VTXOStatus_VTXO_STATUS_EXPIRED,
		},
	})
	require.NoError(t, err)
	require.Len(t, all, len(seeded))

	_, err = list(&waverpc.ListVTXOsRequest{
		Statuses: []waverpc.VTXOStatus{
			waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
		},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
	)
}
