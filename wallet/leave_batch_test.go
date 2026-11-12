package wallet

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/stretchr/testify/require"
)

// TestLeaveBatchSingleIntent proves mixed descriptor lifetimes stay paired
// with their inputs in one eager registration, including partial load failure.
func TestLeaveBatchSingleIntent(t *testing.T) {
	t.Parallel()
	first, second := testOutpoint(0), testOutpoint(1)
	missing := testOutpoint(2)
	descriptors := map[wire.OutPoint]*VTXODescriptor{
		first: {
			Outpoint: first,
			Amount:   50_000,
			Expiry:   100,
		},
		second: {
			Outpoint: second,
			Amount:   80_000,
			Expiry:   200,
		},
	}
	manager := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	round := &mockRoundActorBehavior{}
	w := newTestWalletWithManagerAndRound(
		t, manager, round, testVTXOReader(descriptors),
	)
	w.eagerRoundJoin = true
	result := w.Receive(t.Context(), &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{first, missing, second},
		DestOutput:      &wire.TxOut{PkScript: []byte{0x51}},
	})
	response, err := result.Unpack()
	require.NoError(t, err)
	leave, ok := response.(*LeaveVTXOsResponse)
	require.True(t, ok)
	require.Equal(t, 2, leave.LeavingCount)
	require.Len(t, leave.Errors, 1)
	require.Error(t, leave.Errors[missing])
	require.Equal(t, 1, manager.forfeitReserveCalls)
	require.Equal(t, 1, round.registerCalls)
	require.True(t, round.capturedIntent.TriggerRegistration)
	require.Len(t, round.capturedIntent.Forfeits, 2)
	require.Len(t, round.capturedIntent.Leaves, 2)
	for index, op := range []wire.OutPoint{first, second} {
		forfeit := round.capturedIntent.Forfeits[index]
		leave := round.capturedIntent.Leaves[index]
		require.Equal(t, op, *forfeit.VTXOOutpoint)
		require.Equal(t, descriptors[op].Amount, forfeit.Amount)
		require.Equal(
			t, int64(descriptors[op].Amount), leave.Output.Value,
		)
		require.Equal(t, []byte{0x51}, leave.Output.PkScript)
	}
}

// TestLeaveBatchReservationAndRegistrationFailure proves a failed reservation
// never registers, and a registration rejection requests release of the batch.
func TestLeaveBatchReservationAndRegistrationFailure(t *testing.T) {
	t.Parallel()
	for _, reserveFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("reserveFailure=%v",
			reserveFailure), func(t *testing.T) {
			t.Parallel()
			first, second := testOutpoint(0), testOutpoint(1)
			reserved := &actormsg.ReserveForfeitResponse{}
			released := &actormsg.ReleaseForfeitResponse{
				ReleasedCount: 2,
			}
			manager := &mockVTXOManagerBehavior{
				forfeitReserveResp: reserved,
				forfeitReleaseResp: released,
			}
			round := &mockRoundActorBehavior{}
			if reserveFailure {
				manager.forfeitReserveErr = fmt.Errorf(
					"input busy")
			} else {
				round.registerErr = fmt.Errorf("round rejected")
			}
			w := newTestWalletWithManagerAndRound(
				t, manager, round,
				testVTXOReader(
					map[wire.OutPoint]*VTXODescriptor{
						first: {
							Outpoint: first,
							Amount:   50_000,
						},
						second: {
							Outpoint: second,
							Amount:   80_000,
						},
					},
				),
			)
			result := w.Receive(
				t.Context(), &LeaveVTXOsRequest{
					TargetOutpoints: []wire.OutPoint{
						first, second,
					},
					DestOutput: &wire.TxOut{
						PkScript: []byte{0x51},
					},
				},
			)
			require.True(t, result.IsErr())
			if reserveFailure {
				require.Zero(t, round.registerCalls)

				return
			}
			require.Equal(t, 1, round.registerCalls)
			require.Equal(t, 1, manager.forfeitReleaseCalls)
		})
	}
}

// TestLeaveWithoutActorSystemRejectsEveryInput prevents fabricated queued
// evidence when the wallet cannot register any intent.
func TestLeaveWithoutActorSystemRejectsEveryInput(t *testing.T) {
	t.Parallel()
	w := &Ark{}
	targets := []wire.OutPoint{testOutpoint(0), testOutpoint(1)}
	response, err := w.Receive(t.Context(), &LeaveVTXOsRequest{
		TargetOutpoints: targets,
	}).Unpack()
	require.NoError(t, err)
	leave, ok := response.(*LeaveVTXOsResponse)
	require.True(t, ok)
	require.Zero(t, leave.LeavingCount)
	require.Len(t, leave.Errors, len(targets))
	for _, target := range targets {
		require.Error(t, leave.Errors[target])
	}
}
