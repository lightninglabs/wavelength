package txconfirm

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// mustCurrentTrackedTxState returns the current tracked-tx FSM state.
func mustCurrentTrackedTxState(t *testing.T,
	fsm *trackedTxStateMachine) trackedTxState {

	t.Helper()

	rawState, err := fsm.CurrentState()
	require.NoError(t, err)

	state, ok := rawState.(trackedTxState)
	require.True(t, ok)

	return state
}

// TestTrackedTxFSMInitialBroadcastFlow verifies that the tracked-tx protofsm
// carries immutable data and broadcast progress through its normal lifecycle.
func TestTrackedTxFSMInitialBroadcastFlow(t *testing.T) {
	tx := makeTestTx(true)
	data := trackedTxData{
		Tx:          tx,
		Txid:        tx.TxHash(),
		Label:       "test",
		HeightHint:  91,
		TargetConfs: 2,
	}

	fsm := newTrackedTxStateMachine(btclog.Disabled, data)
	fsm.Start(t.Context())
	t.Cleanup(fsm.Stop)

	_, err := fsm.AskEvent(
		t.Context(), &trackedTxBroadcastStarted{},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	broadcasting, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateBroadcasting)
	require.True(t, ok)
	require.Equal(t, data.Txid, broadcasting.Txid)
	require.Equal(t, data.TargetConfs, broadcasting.TargetConfs)

	progress := trackedTxProgress{
		LastBroadcastHeight: 100,
		CurrentFeeRate:      7,
		ChildTxid:           copyHash(&data.Txid),
	}
	_, err = fsm.AskEvent(
		t.Context(), &trackedTxBroadcastAccepted{
			Progress: progress,
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	awaiting, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateAwaitingConfirmation)
	require.True(t, ok)
	require.Equal(
		t, progress.LastBroadcastHeight, awaiting.LastBroadcastHeight,
	)
	require.Equal(t, progress.CurrentFeeRate, awaiting.CurrentFeeRate)
	require.Equal(t, 0, awaiting.BumpCount)
	require.Equal(t, progress.ChildTxid, awaiting.ChildTxid)
	require.Equal(
		t, TxStateAwaitingConfirmation,
		txStateFromTrackedState(awaiting),
	)
	require.Equal(t, int32(100), trackedTxLastBroadcastHeight(awaiting))

	_, err = fsm.AskEvent(
		t.Context(), &trackedTxConfirmed{
			BlockHeight: 102,
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	confirmed, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateConfirmed)
	require.True(t, ok)
	require.Equal(t, int32(102), confirmed.ConfirmHeight)
	require.Equal(
		t, progress.LastBroadcastHeight, confirmed.LastBroadcastHeight,
	)
	height, ok := trackedTxConfirmHeight(confirmed)
	require.True(t, ok)
	require.Equal(t, int32(102), height)
	require.Equal(t, TxStateConfirmed, txStateFromTrackedState(confirmed))
}

// TestTrackedTxFSMFeeBumpFlow verifies that fee-bump retries preserve prior
// progress and increment the bump counter on successful rebroadcast.
func TestTrackedTxFSMFeeBumpFlow(t *testing.T) {
	tx := makeTestTx(true)
	data := trackedTxData{
		Tx:          tx,
		Txid:        tx.TxHash(),
		TargetConfs: 1,
	}

	fsm := newTrackedTxStateMachine(btclog.Disabled, data)
	fsm.Start(t.Context())
	t.Cleanup(fsm.Stop)

	_, err := fsm.AskEvent(
		t.Context(), &trackedTxBroadcastStarted{},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	_, err = fsm.AskEvent(
		t.Context(), &trackedTxBroadcastAccepted{
			Progress: trackedTxProgress{
				LastBroadcastHeight: 100,
				CurrentFeeRate:      5,
			},
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	_, err = fsm.AskEvent(
		t.Context(), &trackedTxFeeBumpStarted{},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	feeBumping, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateFeeBumping)
	require.True(t, ok)
	require.Equal(t, int32(100), feeBumping.LastBroadcastHeight)
	require.Equal(t, 0, feeBumping.BumpCount)

	_, err = fsm.AskEvent(
		t.Context(), &trackedTxBroadcastAccepted{
			Progress: trackedTxProgress{
				LastBroadcastHeight: 103,
				CurrentFeeRate:      11,
			},
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	awaiting, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateAwaitingConfirmation)
	require.True(t, ok)
	require.Equal(t, int32(103), awaiting.LastBroadcastHeight)
	require.Equal(t, int64(11), awaiting.CurrentFeeRate)
	require.Equal(t, 1, awaiting.BumpCount)
}

// TestTrackedTxFSMFailureAndInvalidTransitions verifies terminal failure
// projection and unexpected-event error handling.
func TestTrackedTxFSMFailureAndInvalidTransitions(t *testing.T) {
	tx := makeTestTx(false)
	data := trackedTxData{
		Tx:          tx,
		Txid:        tx.TxHash(),
		TargetConfs: 1,
	}

	fsm := newTrackedTxStateMachine(btclog.Disabled, data)
	fsm.Start(t.Context())
	t.Cleanup(fsm.Stop)

	_, err := fsm.AskEvent(
		t.Context(), &trackedTxFailed{
			Reason: "broadcast rejected",
		},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)

	failed, ok := mustCurrentTrackedTxState(
		t, fsm,
	).(*trackedTxStateFailed)
	require.True(t, ok)
	reason, ok := trackedTxFailureReason(failed)
	require.True(t, ok)
	require.Equal(t, "broadcast rejected", reason)
	require.Equal(t, TxStateFailed, txStateFromTrackedState(failed))
	require.Zero(t, trackedTxLastBroadcastHeight(failed))

	_, err = failed.ProcessEvent(
		t.Context(), &trackedTxBroadcastStarted{},
		&trackedTxEnvironment{
			Txid: data.Txid,
		},
	)
	require.Error(t, err)

	newState := &trackedTxStateNew{trackedTxData: data}
	_, err = newState.ProcessEvent(
		t.Context(), &trackedTxConfirmed{
			BlockHeight: 1,
		}, &trackedTxEnvironment{Txid: data.Txid},
	)
	require.Error(t, err)
}
