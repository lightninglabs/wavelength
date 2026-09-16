package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestDurableColdRestore rejects signing and cancellation after session loss.
func TestDurableColdRestore(t *testing.T) {
	id := RoundID{1}
	operation := [32]byte{2}
	point := wire.OutPoint{Index: 3}
	intents := Intents{Forfeits: []types.ForfeitRequest{{
		VTXOOutpoint: &point, Amount: 1,
	}}, Service: &types.ServiceRequest{
		OperationID: operation, Mode: types.ServiceImmediate,
		ExpiresAtUnix: 1700000000,
	}}
	states := []ClientState{
		&NoncesSentState{
			RoundID: id,
			Intents: intents,
		},
		&NoncesAggregatedState{
			RoundID: id,
			Intents: intents,
		},
		&PartialSigsSentState{
			RoundID: id,
			Intents: intents,
		},
	}
	for _, state := range states {
		h := newActorTestHarness(t)
		a := h.actor
		key := RoundKeyStr(id.KeyString())
		round := &RoundFSM{
			Key: id, RoundID: id,
			env: &ClientEnvironment{
				RoundKey: key,
			},
		}
		a.installRoundState(t.Context(), round, state)
		a.rounds[key] = round
		raw, err := a.encodeActorSnapshot(t.Context())
		require.NoError(t, err)
		snapshot, err := decodeActorSnapshot(raw, a.env)
		require.NoError(t, err)
		a.installActorSnapshot(t.Context(), snapshot)
		restored := a.rounds[key]
		require.True(t, restored.coldSigning)
		t.Cleanup(restored.FSM.Stop)

		// Neither a local cancellation nor a server request can resume
		// signer work or discard ownership in the cold state.
		cancelRequest := &CancelRoundRequest{RoundKey: fn.Some(key)}
		response := a.handleCancelRound(
			t.Context(), cancelRequest,
		).UnwrapOrFail(t)
		cancelled, ok := response.(*CancelRoundResponse)
		require.True(t, ok)
		require.False(t, cancelled.Success)
		require.Same(t, restored, a.rounds[key])
		before, err := a.encodeActorSnapshot(t.Context())
		require.NoError(t, err)
		for _, event := range []ClientEvent{
			&RoundStatusReported{
				RoundID: id,
				Status:  roundStatusDead,
			},
			&RoundStatusReported{
				RoundID: id, Status: roundStatusDead,
				Operation: &roundpb.OperationStatus{
					OperationId: []byte{
						9,
					}, Phase: operationFailed,
				},
			},
			&RoundStatusReported{
				RoundID: RoundID{
					9,
				}, Status: roundStatusDead,
				Operation: &roundpb.OperationStatus{
					OperationId: operation[:],
					Phase:       operationFailed,
				},
			},
			&BoardingConfirmed{},
		} {
			require.NoError(
				t,
				a.askEventAndProcessOutbox(
					t.Context(), restored, event,
				),
			)
		}
		after, err := a.encodeActorSnapshot(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)

		// A matching terminal operation report permits the one cleanup
		// path. Reservation release is captured for durable delivery.
		h.roundStore.
			On("FailRound", mock.Anything, id).
			Return(nil).
			Once()
		var effects []ClientOutMsg
		a.queueEffect = func(_ context.Context,
			message ClientOutMsg) error {

			effects = append(effects, message)

			return nil
		}
		require.NoError(
			t,
			a.askEventAndProcessOutbox(
				t.Context(), restored, &RoundStatusReported{
					RoundID: id,
					Status:  roundStatusDead,
					Operation: &roundpb.OperationStatus{
						OperationId: operation[:],
						Phase:       operationFailed,
					},
				},
			),
		)
		require.False(t, restored.coldSigning)
		settled, err := restored.FSM.CurrentStateWithContext(
			t.Context(),
		)
		require.NoError(t, err)
		require.IsType(t, &ClientFailedState{}, settled)
		require.Len(t, effects, 1)
		require.Equal(t, &ReleaseForfeitReservation{
			Outpoints: []wire.OutPoint{point},
		}, effects[0])
		h.roundStore.AssertExpectations(t)
	}
}
