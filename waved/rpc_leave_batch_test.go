package waved

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// batchLeaveWallet captures the selected inputs at the actual actor boundary.
type batchLeaveWallet struct {
	request  *wallet.LeaveVTXOsRequest
	response *wallet.LeaveVTXOsResponse
	calls    int
}

// Receive returns explicit admission evidence without performing a transaction.
func (w *batchLeaveWallet) Receive(_ context.Context,
	message wallet.WalletMsg) fn.Result[wallet.WalletResp] {

	w.calls++
	request, ok := message.(*wallet.LeaveVTXOsRequest)
	if !ok {
		return fn.Err[wallet.WalletResp](
			fmt.Errorf("unexpected message %T", message),
		)
	}
	w.request = request

	return fn.Ok[wallet.WalletResp](w.response)
}

// TestLeaveBatchCrossRoundResults proves different origin rounds survive SQL
// admission and one actor dispatch, preserving authoritative per-input failure.
func TestLeaveBatchCrossRoundResults(t *testing.T) {
	t.Parallel()
	r, store := newLeaveAdmissionServer(t)
	first := saveVTXOWithStatus(t, store, 0xa1, vtxo.VTXOStatusLive)
	second := saveVTXOWithStatus(t, store, 0xb2, vtxo.VTXOStatusLive)
	require.NotEqual(t, first.RoundID, second.RoundID)

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})
	behavior := &batchLeaveWallet{response: &wallet.LeaveVTXOsResponse{
		LeavingCount: 1,
		Errors: map[wire.OutPoint]error{
			second.Outpoint: fmt.Errorf("descriptor read failed"),
		},
	}}
	key := actor.NewServiceKey[wallet.WalletMsg, wallet.WalletResp](
		"batch-leave",
	)
	ref := key.Spawn(system, "batch-leave", behavior)
	r.server.walletRef = fn.Some(ref)
	r.server.walletReady = make(chan struct{})
	close(r.server.walletReady)
	targets := []string{first.Outpoint.String(), second.Outpoint.String()}
	response, err := r.LeaveVTXOs(t.Context(), &waverpc.LeaveVTXOsRequest{
		Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &waverpc.OutpointSelection{
				Outpoints: targets,
			},
		},
		DefaultDestination: &waverpc.LeaveDestination{
			Target: &waverpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x11),
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, behavior.calls)
	require.Equal(
		t, []wire.OutPoint{first.Outpoint, second.Outpoint},
		behavior.request.TargetOutpoints,
	)
	require.Equal(t, targets[:1], response.QueuedOutpoints)
	require.Equal(
		t, map[string]string{
			targets[1]: "descriptor read failed",
		},
		response.Errors,
	)
}

// TestLeaveBatchPreflightRejectsWholeSelection proves a bad destination or an
// unavailable input never reaches the wallet actor or reserves the live input.
func TestLeaveBatchPreflightRejectsWholeSelection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name               string
		unavailable        vtxo.VTXOStatus
		invalidDestination bool
		duplicate          bool
		want               codes.Code
	}{
		{
			name:        "committed",
			unavailable: vtxo.VTXOStatusPendingForfeit,
			want:        codes.FailedPrecondition,
		},
		{
			name:        "spent",
			unavailable: vtxo.VTXOStatusSpent,
			want:        codes.FailedPrecondition,
		},
		{
			name:               "destination",
			unavailable:        vtxo.VTXOStatusLive,
			invalidDestination: true,
			want:               codes.InvalidArgument,
		},
		{
			name:        "duplicate",
			unavailable: vtxo.VTXOStatusLive,
			duplicate:   true,
			want:        codes.InvalidArgument,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r, store := newLeaveAdmissionServer(t)
			first := saveVTXOWithStatus(
				t, store, 0xa1, vtxo.VTXOStatusLive,
			)
			second := saveVTXOWithStatus(
				t, store, 0xb2, test.unavailable,
			)
			targets := []string{
				first.Outpoint.String(),
				second.Outpoint.String(),
			}
			if test.duplicate {
				targets[1] = targets[0]
			}
			script := validP2TRPkScript(0x11)
			if test.invalidDestination {
				script = []byte{0x01}
			}
			// No wallet actor is installed. Reaching it would
			// return wallet-not-ready rather than the expected
			// validation code.
			selection := &waverpc.OutpointSelection{
				Outpoints: targets,
			}
			dest := &waverpc.LeaveDestination_PkScript{
				PkScript: script,
			}
			request := &waverpc.LeaveVTXOsRequest{
				Selection: &waverpc.LeaveVTXOsRequest_Outpoints{
					Outpoints: selection,
				},
				DefaultDestination: &waverpc.LeaveDestination{
					Target: dest,
				},
			}
			_, err := r.LeaveVTXOs(t.Context(), request)
			require.Equal(t, test.want, status.Code(err))
			live, err := store.GetVTXO(t.Context(), first.Outpoint)
			require.NoError(t, err)
			require.Equal(t, vtxo.VTXOStatusLive, live.Status)
		})
	}
}
