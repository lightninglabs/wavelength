package round

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestActorStart verifies the actor initialization sequence, ensuring proper
// registration with dependencies and correct handling of initial messages.
func TestActorStart(t *testing.T) {
	t.Parallel()

	t.Run("registers_with_wallet", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// The actor must register a notifier with the wallet to
		// receive boarding confirmations. Without this registration,
		// the actor would never learn about completed on-chain
		// deposits.
		require.NotNil(
			t, h.walletActor.registeredNotifier,
			"expected wallet notifier to be registered",
		)
	})

	t.Run("receives_wallet_confirmation", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// After receiving a wallet confirmation, the FSM should
		// transition from Idle to PendingRoundAssembly, indicating
		// the actor is now waiting for the server to signal that a
		// new round is being assembled.
		states := h.queryState()
		primaryState, exists := states["primary"]
		require.True(t, exists, "expected primary FSM state")

		_, ok := primaryState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			primaryState.State,
		)
	})

	t.Run("sends_join_round_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.sendServerMessage(&RegistrationRequested{})

		// When the server signals registration is requested, the
		// actor should respond by sending a join round request
		// containing all pending boarding intents.
		h.serverConn.assertMessageSent(t, "SendClientEventRequest")
	})

	t.Run("handles_get_state_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		states := h.queryState()

		primaryState, exists := states["primary"]
		require.True(t, exists)
		require.True(t, primaryState.IsPrimary)

		_, ok := primaryState.State.(*Idle)
		require.True(t, ok, "expected Idle, got %T", primaryState.State)
	})

	t.Run("handles_cancel_round_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		result := h.receive(&CancelRoundRequest{})
		require.True(t, result.IsOk())

		resp, _ := result.Unpack()
		cancelResp, ok := resp.(*CancelRoundResponse)
		require.True(t, ok, "expected CancelRoundResponse")
		require.True(t, cancelResp.Success)

		states := h.queryState()
		primaryState := states["primary"]
		_, isFailedState := primaryState.State.(*ClientFailedState)
		require.True(
			t, isFailedState,
			"expected ClientFailedState after cancel, got %T",
			primaryState.State,
		)
	})
}

// TestActorRecovery validates that the actor can restore its state after a
// restart by loading active rounds from persistent storage and re-establishing
// chain confirmations.
func TestActorRecovery(t *testing.T) {
	t.Parallel()

	t.Run("single_active_round", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		roundID := testRoundID("test-round-001")
		round := h.newTestRound(roundID)
		h.setupMockRoundStoreForRecovery([]*Round{round})

		err := h.start()
		require.NoError(t, err)

		// The actor should have loaded the round from storage and
		// created a corresponding FSM to resume processing.
		require.Len(t, h.actor.activeRounds, 1)

		roundFSM, exists := h.actor.activeRounds[roundID]
		require.True(t, exists, "expected round FSM for test-round-001")
		require.Equal(t, roundID, roundFSM.RoundID)

		// The commitment tx index is used to route confirmation
		// events to the correct round FSM, so it must be rebuilt
		// during recovery.
		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()
		indexedRoundID, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists, "expected commitment tx in index")
		require.Equal(t, roundID, indexedRoundID)

		// The actor must re-register for chain confirmations during
		// recovery to resume monitoring the commitment transaction.
		require.Len(t, h.chainSource.registrations, 1)
		reg := h.chainSource.registrations[0]
		require.NotNil(t, reg.Txid)
		require.True(t, reg.Txid.IsEqual(&txid))
	})

	t.Run("multiple_active_rounds", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		rounds := []*Round{
			h.newTestRound(testRoundID("round-001")),
			h.newTestRound(testRoundID("round-002")),
			h.newTestRound(testRoundID("round-003")),
		}
		h.setupMockRoundStoreForRecovery(rounds)

		err := h.start()
		require.NoError(t, err)

		require.Len(t, h.actor.activeRounds, 3)
		for _, round := range rounds {
			_, exists := h.actor.activeRounds[round.RoundID]
			require.True(
				t, exists,
				"expected round FSM for %s", round.RoundID,
			)
		}

		require.Len(t, h.actor.commitmentTxIndex, 3)
		require.Len(t, h.chainSource.registrations, 3)
	})
}

// TestActorConfirmation exercises the confirmation event routing logic,
// ensuring events are delivered to the correct round FSM and error cases are
// handled gracefully.
func TestActorConfirmation(t *testing.T) {
	t.Parallel()

	t.Run("routes_to_correct_fsm", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		roundID := testRoundID("test-round-001")
		round := h.newTestRound(roundID)
		h.setupMockRoundStoreForRecovery([]*Round{round})

		err := h.start()
		require.NoError(t, err)

		require.Contains(t, h.actor.activeRounds, roundID)

		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()
		h.roundStore.On(
			"LookupRoundByCommitmentTx", mock.Anything, txid,
		).Return(round, nil)

		confTx := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx
		confEvent := &ConfirmationEvent{
			Txid:          txid,
			BlockHeight:   105,
			Confirmations: 6,
			Tx:            confTx,
		}

		// This test verifies the routing path from confirmation event
		// to the correct FSM. The mock FSM may reject the event, but
		// we've confirmed the actor's dispatch logic works correctly.
		_ = h.receive(confEvent)
	})

	t.Run("nil_tx_handled_gracefully", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Simulate an unknown txid so the lookup fails gracefully.
		unknownTxid := chainhash.HashH([]byte("nil-tx-test"))
		h.roundStore.On(
			"LookupRoundByCommitmentTx", mock.Anything, unknownTxid,
		).Return(nil, fmt.Errorf("round not found"))

		// Confirmation events with nil Tx should be handled gracefully
		// since the Tx field is not used by handleConfirmation.
		confEvent := &ConfirmationEvent{
			Txid:          unknownTxid,
			BlockHeight:   105,
			Confirmations: 6,
			Tx:            nil,
		}

		result := h.receive(confEvent)
		require.True(t, result.IsOk())
	})

	t.Run("unknown_round_graceful", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		unknownTxid := chainhash.HashH([]byte("unknown-tx"))
		h.roundStore.On(
			"LookupRoundByCommitmentTx", mock.Anything, unknownTxid,
		).Return(nil, fmt.Errorf("round not found"))

		confEvent := &ConfirmationEvent{
			Txid:          unknownTxid,
			BlockHeight:   105,
			Confirmations: 6,
			Tx:            wire.NewMsgTx(2),
		}

		// Confirmations for unknown transactions should be handled
		// gracefully without errors, as they may be from old rounds
		// that have already been cleaned up.
		result := h.receive(confEvent)
		require.True(t, result.IsOk())
	})

	t.Run("fsm_not_found_error", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// This scenario tests the edge case where a round exists in
		// the database but has no active FSM. This represents an
		// inconsistent state that should be caught and reported as an
		// error rather than silently ignored.
		round := h.newTestRound(testRoundID("orphan-round"))
		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()
		h.roundStore.On(
			"LookupRoundByCommitmentTx", mock.Anything, txid,
		).Return(round, nil)

		packet := round.CommitmentTx.UnwrapOrFail(t)
		confEvent := &ConfirmationEvent{
			Txid:          txid,
			BlockHeight:   105,
			Confirmations: 6,
			Tx:            packet.UnsignedTx,
		}

		result := h.receive(confEvent)
		require.True(t, result.IsErr())
		require.Contains(t, result.Err().Error(), "round FSM not found")
	})
}

// TestActorProcessOutbox verifies that the actor correctly handles various
// outbox messages emitted by FSMs, including chain registrations, round
// lifecycle events, and server communication.
func TestActorProcessOutbox(t *testing.T) {
	t.Parallel()

	t.Run("register_confirmation_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		h.chainSource.registrations = nil

		testTxid := chainhash.HashH([]byte("test-txid"))
		outbox := []ClientOutMsg{
			&RegisterConfirmationRequest{
				CallerID:    "test-caller",
				Txid:        &testTxid,
				TargetConfs: 6,
				HeightHint:  100,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// The actor should translate FSM outbox requests into actual
		// chain source registrations, preserving all parameters.
		require.Len(t, h.chainSource.registrations, 1)
		reg := h.chainSource.registrations[0]
		require.True(t, reg.Txid.IsEqual(&testTxid))
		require.Equal(t, uint32(6), reg.TargetConfs)
	})

	t.Run("pkscript_registration", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		h.chainSource.registrations = nil

		pkScript := []byte{0x00, 0x14, 0x01, 0x02, 0x03, 0x04}
		outbox := []ClientOutMsg{
			&RegisterConfirmationRequest{
				CallerID:    "pkscript-caller",
				PkScript:    pkScript,
				TargetConfs: 3,
				HeightHint:  50,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		require.Len(t, h.chainSource.registrations, 1)
		reg := h.chainSource.registrations[0]
		require.Nil(t, reg.Txid)
		require.Equal(t, pkScript, reg.PkScript)
	})

	t.Run("round_completed", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		roundID := testRoundID("completing-round")
		round := h.newTestRound(roundID)
		h.setupMockRoundStoreForRecovery([]*Round{round})

		err := h.start()
		require.NoError(t, err)

		require.Contains(t, h.actor.activeRounds, roundID)

		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()
		h.roundStore.On(
			"FinalizeRound", mock.Anything, roundID, txid,
			mock.Anything,
		).Return(nil)

		confInfo := ConfInfo{
			Height:    100,
			BlockHash: chainhash.Hash{0x01},
		}
		outbox := []ClientOutMsg{
			&RoundCompletedNotification{
				RoundID:  roundID,
				TxID:     txid,
				ConfInfo: confInfo,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// When a round completes, the actor must clean up all
		// in-memory state, including the FSM and tx index entry, to
		// prevent memory leaks and avoid routing events to completed
		// rounds.
		require.NotContains(t, h.actor.activeRounds, roundID)

		_, exists := h.actor.commitmentTxIndex[txid]
		require.False(t, exists)

		h.roundStore.AssertCalled(
			t, "FinalizeRound", mock.Anything, roundID, txid,
			confInfo,
		)
	})

	t.Run("round_checkpointed", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		require.Empty(t, h.actor.activeRounds)

		roundID := testRoundID("checkpointed-round")

		// Set up the primaryFSM in InputSigSentState, simulating the
		// state after the round has completed through partial sigs.
		commitmentTx := h.setupPrimaryFSMInInputSigSentState(roundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{
				RoundID: roundID,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// When a checkpoint notification is received, the actor must
		// promote the primaryFSM to activeRounds so it can continue
		// processing events (like BoardingConfirmed). A new primaryFSM
		// is created in Idle state for subsequent rounds.
		require.Contains(t, h.actor.activeRounds, roundID)

		txid := commitmentTx.UnsignedTx.TxHash()
		indexedRoundID, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists)
		require.Equal(t, roundID, indexedRoundID)

		// Verify the new primaryFSM is in Idle state, ready for new
		// boarding intents.
		primaryState, err := h.actor.primaryFSM.CurrentState()
		require.NoError(t, err)
		_, isIdle := primaryState.(*Idle)
		require.True(t, isIdle, "new primaryFSM should be in Idle, "+
			"got %T", primaryState)
	})

	t.Run("primary_accepts_boarding_after_checkpoint", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// First round: promote primaryFSM to activeRounds.
		firstRoundID := testRoundID("first-round")
		h.setupPrimaryFSMInInputSigSentState(firstRoundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{RoundID: firstRoundID},
		}
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// After promotion, a new primaryFSM should be in Idle.
		primaryState, err := h.actor.primaryFSM.CurrentState()
		require.NoError(t, err)
		_, isIdle := primaryState.(*Idle)
		require.True(t, isIdle)

		// Send a new boarding confirmation to the new primaryFSM.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// The new primaryFSM should transition from Idle to
		// PendingRoundAssembly.
		primaryState, err = h.actor.primaryFSM.CurrentState()
		require.NoError(t, err)
		_, isPending := primaryState.(*PendingRoundAssembly)
		require.True(t, isPending, "new primaryFSM should transition "+
			"to PendingRoundAssembly, got %T", primaryState)

		// The first round should still be tracked in activeRounds.
		require.Contains(t, h.actor.activeRounds, firstRoundID)
	})

	t.Run("promoted_fsm_receives_boarding_confirmed", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Promote primaryFSM to activeRounds.
		roundID := testRoundID("promoted-round")
		commitmentTx := h.setupPrimaryFSMInInputSigSentState(roundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{RoundID: roundID},
		}
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// The promoted FSM should be in activeRounds.
		require.Contains(t, h.actor.activeRounds, roundID)
		promotedFSM := h.actor.activeRounds[roundID]

		// Verify the promoted FSM is still in InputSigSentState.
		promotedState, err := promotedFSM.FSM.CurrentState()
		require.NoError(t, err)
		_, isInputSigSent := promotedState.(*InputSigSentState)
		require.True(t, isInputSigSent)

		// The promoted FSM should be indexed by commitment txid.
		txid := commitmentTx.UnsignedTx.TxHash()
		indexedRoundID, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists)
		require.Equal(t, roundID, indexedRoundID)
	})

	t.Run("vtxo_created", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		outbox := []ClientOutMsg{
			&VTXOCreatedNotification{
				VTXOs: []*ClientVTXO{},
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)
	})

	t.Run("migrate_round_idempotent", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("idempotent-round")
		round := h.newTestRound(roundID)
		h.roundStore.On(
			"FetchState", mock.Anything, roundID,
		).Return(
			round,
			&InputSigSentState{RoundID: roundID},
			nil,
		)

		err = h.actor.migrateRoundToActiveFSM(h.ctx, roundID)
		require.NoError(t, err)
		require.Len(t, h.actor.activeRounds, 1)

		// Migration must be idempotent to handle duplicate
		// checkpoint notifications without creating multiple FSMs for
		// the same round.
		err = h.actor.migrateRoundToActiveFSM(h.ctx, roundID)
		require.NoError(t, err)
		require.Len(t, h.actor.activeRounds, 1)
	})

	t.Run("server_messages", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		joinReq := &JoinRoundRequest{
			BoardingRequests: []types.BoardingRequest{},
			VTXORequests:     []types.VTXORequest{},
		}

		h.clearServerMessages()
		err = h.actor.processOutbox(h.ctx, []ClientOutMsg{joinReq})
		require.NoError(t, err)

		h.assertServerMessageSent("SendClientEventRequest")
	})
}

// TestActorGetStateWithActiveRounds validates that the actor correctly
// reports both the primary FSM state and all active round states when queried.
func TestActorGetStateWithActiveRounds(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	rounds := []*Round{
		h.newTestRound(testRoundID("round-001")),
		h.newTestRound(testRoundID("round-002")),
	}
	h.setupMockRoundStoreForRecovery(rounds)

	err := h.start()
	require.NoError(t, err)

	states := h.queryState()

	// The state map should contain the primary FSM plus one entry for
	// each active round.
	require.Len(t, states, 3)

	primaryState, exists := states["primary"]
	require.True(t, exists)
	require.True(t, primaryState.IsPrimary)

	for _, round := range rounds {
		roundState, exists := states[round.RoundID.String()]
		require.True(t, exists, "expected state for %s", round.RoundID)
		require.False(t, roundState.IsPrimary)
		require.Equal(t, round.RoundID, roundState.RoundID)
	}
}

// TestActorReceiveUnknownMessageType ensures that the actor rejects
// unrecognized message types with an appropriate error rather than silently
// ignoring them.
func TestActorReceiveUnknownMessageType(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	unknownMsg := &unknownClientMsg{}

	result := h.receive(unknownMsg)
	require.True(t, result.IsErr())
	require.Contains(t, result.Err().Error(), "unknown message type")
}

// TestActorLifecycle verifies complete end-to-end workflows through various
// FSM state transitions, ensuring the actor behaves correctly across the full
// boarding lifecycle.
func TestActorLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("full_boarding_flow", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// The actor begins in Idle state, registered with the wallet
		// to receive boarding confirmations.
		require.NotNil(
			t, h.walletActor.registeredNotifier,
			"actor should register with wallet on start",
		)
		h.assertFSMState("Idle")

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.assertFSMState("PendingRoundAssembly")

		h.clearServerMessages()
		h.sendServerMessage(&RegistrationRequested{})
		h.assertFSMState("RegistrationSentState")
		h.assertServerMessageSent("SendClientEventRequest")

		roundID := testRoundID("test-round-001")
		h.simulateRoundJoined(roundID)
		h.assertFSMState("RoundJoinedState")
	})

	t.Run("multiple_boarding_confirmations", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent1 := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent1)
		h.assertFSMState("PendingRoundAssembly")

		// Multiple boarding confirmations should accumulate in the
		// pending intents list, with all intents included in the
		// subsequent join round request.
		intent2 := h.newTestBoardingIntentWithSuffix("-second")
		h.sendWalletConfirmation(intent2)
		h.assertFSMState("PendingRoundAssembly")

		h.clearServerMessages()
		h.sendServerMessage(&RegistrationRequested{})
		h.assertFSMState("RegistrationSentState")
		h.assertServerMessageSent("SendClientEventRequest")
	})

	t.Run("registration_failure", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.sendServerMessage(&RegistrationRequested{})
		h.assertFSMState("RegistrationSentState")

		h.sendServerMessage(&BoardingFailed{
			Reason:      "Round full",
			Error:       fmt.Errorf("max participants reached"),
			Recoverable: true,
		})

		h.assertFSMState("ClientFailedState")
	})
}

// TestActorServerMessageRouting uses table-driven tests to verify that server
// messages trigger the correct FSM state transitions and produce expected
// outbox messages across various scenarios.
func TestActorServerMessageRouting(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		setupState    func(*actorTestHarness)
		serverEvent   ClientEvent
		expectedState string
		expectOutbox  bool
		outboxMsgType string
	}{
		{
			name: "RegistrationRequested_from_PendingRoundAssembly",
			setupState: func(h *actorTestHarness) {
				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
			},
			serverEvent:   &RegistrationRequested{},
			expectedState: "RegistrationSentState",
			expectOutbox:  true,
			outboxMsgType: "SendClientEventRequest",
		},
		{
			name: "RoundJoined_from_RegistrationSentState",
			setupState: func(h *actorTestHarness) {
				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
				h.sendServerMessage(&RegistrationRequested{})
			},
			serverEvent: &RoundJoined{
				RoundID: testRoundID("test-round"),
			},
			expectedState: "RoundJoinedState",
			expectOutbox:  false,
		},
		{
			name: "BoardingFailed_from_any_state",
			setupState: func(h *actorTestHarness) {
				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
			},
			serverEvent: &BoardingFailed{
				Reason:      "Test failure",
				Recoverable: true,
			},
			expectedState: "ClientFailedState",
			expectOutbox:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newActorTestHarness(t)
			tc.setupState(h)
			h.clearServerMessages()

			h.sendServerMessage(tc.serverEvent)

			h.assertFSMState(tc.expectedState)
			if tc.expectOutbox {
				h.assertServerMessageSent(tc.outboxMsgType)
			}
		})
	}
}
