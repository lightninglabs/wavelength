package vtxo

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestStartupRetiresConsumedExpiredVTXO proves a retained confirmed consuming
// round wins over a stale expired row before an actor can emit a refresh.
func TestStartupRetiresConsumedExpiredVTXO(t *testing.T) {
	t.Parallel()
	desc := makeDescriptor(t, 2943, 1)
	desc.Status = VTXOStatusExpired
	desc.ForfeitRoundID = "consuming-round"
	store := &MockVTXOStore{}
	store.On("ListLiveVTXOs", t.Context()).Return([]*Descriptor{desc}, nil)
	store.
		On(
			"UpdateVTXOStatusReleasingReservation",
			t.Context(),
			desc.Outpoint,
			VTXOStatusForfeited,
		).
		Return(nil)
	store.
		On("ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit).
		Return([]*Descriptor{}, nil)
	mgr := NewManager(
		&ManagerConfig{
			Store: store,
			LookupForfeitSettlement: func(_ context.Context,
				id string) (fn.Option[Settlement], error) {

				require.Equal(t, desc.ForfeitRoundID, id)

				return fn.Some(
					Settlement{
						TxID:   chainhash.Hash{1},
						Height: 123,
					},
				), nil
			},
		},
	)
	require.NoError(t, mgr.Start(t.Context(), nil))
	require.Empty(t, mgr.actors)
	require.Empty(t, mgr.liveDescriptors)
	store.AssertExpectations(t)
}

// TestConsumedReconcileRequiresProof keeps incomplete or unavailable evidence
// from terminalizing an unconsumed claim, and propagates persistence failure.
func TestConsumedReconcileRequiresProof(t *testing.T) {
	const updateMethod = "UpdateVTXOStatusReleasingReservation"
	for _, tc := range []struct {
		name      string
		proof     fn.Option[Settlement]
		lookupErr error
		writeErr  error
		wantWrite bool
		wantErr   bool
	}{
		{
			name:  "absent",
			proof: fn.None[Settlement](),
		},
		{
			name:  "unconfirmed",
			proof: fn.Some(Settlement{TxID: chainhash.Hash{1}}),
		},
		{
			name:  "missing txid",
			proof: fn.Some(Settlement{Height: 123}),
		},
		{
			name:      "lookup unavailable",
			lookupErr: errors.New("unavailable"),
			wantErr:   true,
		},
		{
			name: "write fails",
			proof: fn.Some(
				Settlement{
					TxID:   chainhash.Hash{1},
					Height: 123,
				},
			),
			writeErr:  errors.New("write failed"),
			wantWrite: true,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := makeDescriptor(t, 2943, 1)
			desc.ForfeitRoundID = "consuming-round"
			store := &MockVTXOStore{}
			if tc.wantWrite {
				store.
					On(
						updateMethod,
						t.Context(),
						desc.Outpoint,
						VTXOStatusForfeited,
					).
					Return(tc.writeErr)
			}
			mgr := NewManager(
				&ManagerConfig{
					Store: store,
					LookupForfeitSettlement: func(
						context.Context, string) (
						fn.Option[Settlement], error) {

						return tc.proof, tc.lookupErr
					},
				},
			)
			consumed, err := mgr.reconcileConsumedVTXO(
				t.Context(), desc,
			)
			require.Equal(t, tc.wantErr, err != nil)
			require.False(t, consumed)
			store.AssertExpectations(t)
		})
	}
}

// TestExitRecoveryHonorsConsumingSettlement covers both a clean exit failure
// and a source-batch conflict after the local consuming round confirmed.
func TestExitRecoveryHonorsConsumingSettlement(t *testing.T) {
	for _, outcome := range []ExitOutcome{
		ExitOutcomeRecoverable,
		ExitOutcomeConflicted,
	} {
		t.Run(outcome.String(), func(t *testing.T) {
			desc := makeDescriptor(t, 2943, 1)
			desc.Status = VTXOStatusUnilateralExit
			desc.ForfeitRoundID = "consuming-round"
			mgr, store, ref := newExitTestManager(
				t, desc, &UnilateralExitState{
					VTXO: desc,
				},
			)
			store.On("GetVTXO", t.Context(), desc.Outpoint).Return(
				desc, nil,
			)
			txid := chainhash.Hash{1}
			mgr.cfg.LookupForfeitSettlement = func(context.Context,
				string) (fn.Option[Settlement], error) {

				return fn.Some(
					Settlement{
						TxID:   txid,
						Height: 123,
					},
				), nil
			}
			_, err := mgr.
				Receive(
					t.Context(),
					&ExitOutcomeNotification{
						Outpoint: desc.Outpoint,
						Outcome:  outcome,
					},
				).
				Unpack()
			require.NoError(t, err)
			state, ok := ref.state.(*ForfeitedState)
			require.True(t, ok)
			require.Equal(t, txid, state.CommitmentTxID)
			store.AssertExpectations(t)
		})
	}
}

// TestLateForfeitConfirmationAfterExitRecovery closes the race where the
// consuming round confirms after the conflict lookup, including a refresh
// reservation acquired by the expired actor before confirmation arrives.
func TestLateForfeitConfirmationAfterExitRecovery(t *testing.T) {
	desc := makeDescriptor(t, 2943, 1)
	desc.ForfeitRoundID = "consuming-round"
	for _, state := range []VTXOState{
		&UnilateralExitState{
			VTXO: desc,
		},
		&ExpiredState{
			VTXO: desc,
		},
		&PendingForfeitState{
			VTXO: desc,
		},
	} {
		txid := chainhash.Hash{1}
		transition, err := state.ProcessEvent(
			t.Context(), &ForfeitConfirmedEvent{
				CommitmentTxID: txid,
				BlockHeight:    123,
			},
			nil,
		)
		require.NoError(t, err)
		confirmed, ok := transition.NextState.(*ForfeitedState)
		require.True(t, ok)
		require.Equal(t, txid, confirmed.CommitmentTxID)
		emitted := transition.NewEvents.UnsafeFromSome()
		require.Len(t, emitted.Outbox, 2)
		update, ok := emitted.Outbox[0].(*VTXOStatusUpdate)
		require.True(t, ok)
		require.Equal(t, VTXOStatusForfeited, update.NewStatus)
		require.IsType(
			t, &VTXOTerminatedNotification{}, emitted.Outbox[1],
		)
	}
}
