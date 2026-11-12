//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestLeaveBatchCrossRoundRestart proves that two persisted origin rounds
// produce one eager operator intent, and both orphan reservations recover
// on restart when the operator never acknowledges admission.
func TestLeaveBatchCrossRoundRestart(t *testing.T) {
	ParallelN(t)
	fixture := newDirectedSendFixture(t, func(cfg *waved.Config) {
		cfg.EagerRoundJoin = true
		cfg.RegistrationTimeout = -time.Second
	})

	// Seed a second finalized origin round while the daemon is stopped.
	// Its distinct descriptor and amount must not be replaced or omitted.
	fixture.shutdown()
	second := seedVTXO(
		t, fixture.cfg, fixture.operatorKey, btcutil.Amount(80_000),
		"second-origin-round", vtxo.VTXOStatusLive,
	)
	fixture.launch()
	targets := []string{fixture.seededOutpoint.String(), second.String()}
	selection := &waverpc.OutpointSelection{Outpoints: targets}
	destination := &waverpc.LeaveDestination_Address{
		Address: newRegtestTaprootAddr(t),
	}
	response, err := fixture.client.LeaveVTXOs(t.Context(),
		&waverpc.LeaveVTXOsRequest{
			Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: selection,
			},
			DefaultDestination: &waverpc.LeaveDestination{
				Target: destination,
			},
		},
	)
	require.NoError(t, err)
	require.Empty(t, response.Errors)
	require.Equal(t, targets, response.QueuedOutpoints)
	for _, op := range []wire.OutPoint{fixture.seededOutpoint, second} {
		requireVTXOStatusEventually(
			t, fixture.client, op,
			waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
			parkedObserveWindow,
		)
	}
	require.Eventually(t, func() bool {
		return len(fixture.mailboxServer.joinRoundRequests()) > 0
	}, 20*time.Second, 100*time.Millisecond)
	joins := fixture.mailboxServer.joinRoundRequests()
	require.Len(t, joins, 1)
	require.Len(t, joins[0].ForfeitRequests, 2)
	require.Len(t, joins[0].LeaveRequests, 2)
	got := make([]wire.OutPoint, 0, 2)
	for _, request := range joins[0].ForfeitRequests {
		var op wire.OutPoint
		copy(op.Hash[:], request.VtxoOutpoint.TxHash)
		op.Index = request.VtxoOutpoint.OutputIndex
		got = append(got, op)
	}
	require.ElementsMatch(
		t, []wire.OutPoint{fixture.seededOutpoint, second}, got,
	)

	fixture.restart()
	for _, op := range []wire.OutPoint{fixture.seededOutpoint, second} {
		requireVTXOStatusEventually(
			t, fixture.client, op,
			waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			refreshRestartRecoveryWindow,
		)
	}
	require.Equal(t, int64(180_000), vtxoBalanceSat(t, fixture.client))
	require.Len(
		t, fixture.mailboxServer.joinRoundRequests(), 1, "restart "+
			"must release orphan reservations without "+
			"replaying the batch",
	)
}
