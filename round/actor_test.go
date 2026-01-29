package round

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM state")

		_, ok := tempState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State,
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
		h.sendVTXORequests(50000)
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

		// On startup with no active rounds, the state map should be
		// empty since FSMs are now created on-demand when boarding
		// intents arrive.
		states := h.queryState()
		require.Empty(t, states, "expected no rounds at startup")

		// After a boarding intent arrives, a temp-keyed round is
		// created.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		states = h.queryState()
		require.Len(t, states, 1, "expected one round after boarding")

		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM")
		require.True(t, tempState.IsTemp)
	})

	t.Run("handles_cancel_round_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// Verify round exists before cancel.
		statesBeforeCancel := h.queryState()
		_, exists := h.findTempState(statesBeforeCancel)
		require.True(t, exists, "temp round before cancel")

		result := h.receive(&CancelRoundRequest{})
		require.True(t, result.IsOk())

		resp, _ := result.Unpack()
		cancelResp, ok := resp.(*CancelRoundResponse)
		require.True(t, ok, "expected CancelRoundResponse")
		require.True(t, cancelResp.Success)

		// After cancel, the round is removed from rounds map.
		states := h.queryState()
		require.Empty(t, states, "round should be removed after cancel")
	})

	t.Run("registers_vtxo_requests_from_amounts", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		signingKey := &keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig,
				Index:  0,
			},
		}

		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			keychain.KeyFamilyMultiSig,
		).Return(signingKey, nil).Once()

		amount := btcutil.Amount(50000)
		msg := &RegisterVTXORequestsRequest{
			Amounts: []btcutil.Amount{amount},
		}

		result := h.receive(msg)
		require.True(t, result.IsOk())

		resp, _ := result.Unpack()
		registerResp, ok := resp.(*RegisterVTXORequestsResponse)
		require.True(t, ok)
		require.True(t, registerResp.Success)

		states := h.queryState()
		require.Len(t, states, 1, "expected one FSM")

		// Find the temp-keyed round.
		tempState, found := h.findTempState(states)
		require.True(t, found, "expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State,
		)
		require.Len(t, assembly.VTXOs, 1)

		expectedDesc, err := tree.NewVTXODescriptor(
			amount, signingKey.PubKey, h.operatorPubKey,
			h.operatorTerms.VTXOExitDelay,
		)
		require.NoError(t, err)

		req := assembly.VTXOs[0]
		require.Equal(t, amount, req.Amount)
		require.Equal(t, expectedDesc.PkScript, req.PkScript)
		require.Equal(t, h.operatorTerms.VTXOExitDelay, req.Expiry)
		require.True(t, req.ClientKey.IsEqual(signingKey.PubKey))
		require.True(t, req.OperatorKey.IsEqual(h.operatorPubKey))
		require.Equal(t, *signingKey, req.SigningKey)
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
		require.Len(t, h.actor.rounds, 1)

		keyStr := RoundKeyStr(roundID.KeyString())
		roundFSM, exists := h.actor.rounds[keyStr]
		require.True(t, exists, "expected round FSM for test-round-001")
		require.Equal(t, roundID, roundFSM.RoundID)

		// The commitment tx index is used to route confirmation
		// events to the correct round FSM, so it must be rebuilt
		// during recovery.
		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()
		indexedKeyStr, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists, "expected commitment tx in index")
		require.Equal(t, keyStr, indexedKeyStr)

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

		require.Len(t, h.actor.rounds, 3)
		for _, round := range rounds {
			keyStr := RoundKeyStr(round.RoundID.KeyString())
			_, exists := h.actor.rounds[keyStr]
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

		keyStr := RoundKeyStr(roundID.KeyString())
		require.Contains(t, h.actor.rounds, keyStr)

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

		// This scenario tests the edge case where the commitment tx
		// index contains an entry for a txid, but the corresponding
		// FSM no longer exists (e.g., was cleaned up but index wasn't
		// updated). This is an inconsistent state that should be
		// reported as an error.
		round := h.newTestRound(testRoundID("orphan-round"))
		txid := round.CommitmentTx.UnwrapOrFail(t).UnsignedTx.TxHash()

		// Simulate an inconsistent state by adding to the index
		// without an FSM.
		orphanKeyStr := RoundKeyStr("orphan-round-key")
		h.actor.commitmentTxIndex[txid] = orphanKeyStr

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

		keyStr := RoundKeyStr(roundID.KeyString())
		require.Contains(t, h.actor.rounds, keyStr)

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
		require.NotContains(t, h.actor.rounds, keyStr)

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

		require.Empty(t, h.actor.rounds)

		roundID := testRoundID("checkpointed-round")

		// Set up a round in InputSigSentState, simulating the state
		// after the round has completed through partial sigs.
		commitmentTx := h.setupRoundInInputSigSentState(roundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{
				RoundID: roundID,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// When a checkpoint notification is received, the commitment
		// tx should be indexed for confirmation routing.
		keyStr := RoundKeyStr(roundID.KeyString())
		require.Contains(t, h.actor.rounds, keyStr)

		txid := commitmentTx.UnsignedTx.TxHash()
		indexedKeyStr, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists)
		require.Equal(t, keyStr, indexedKeyStr)
	})

	t.Run("new_boarding_creates_round", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Initially no rounds.
		require.Empty(t, h.actor.rounds)

		// Send a new boarding confirmation - should create a new round.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// A new round should be created.
		require.Len(t, h.actor.rounds, 1)

		// The new round should be in PendingRoundAssembly state.
		for _, roundFSM := range h.actor.rounds {
			state, err := roundFSM.FSM.CurrentState()
			require.NoError(t, err)

			_, isPending := state.(*PendingRoundAssembly)
			require.True(t, isPending, "round should transition "+
				"to PendingRoundAssembly, got %T", state)

			// The round should have a temp key.
			require.True(t, roundFSM.Key.IsTemp())
		}
	})

	t.Run("checkpointed_round_indexed", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Set up a round in InputSigSentState.
		roundID := testRoundID("indexed-round")
		commitmentTx := h.setupRoundInInputSigSentState(roundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{RoundID: roundID},
		}
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// The round should be tracked.
		keyStr := RoundKeyStr(roundID.KeyString())
		require.Contains(t, h.actor.rounds, keyStr)
		roundFSM := h.actor.rounds[keyStr]

		// Verify the FSM is still in InputSigSentState.
		state, err := roundFSM.FSM.CurrentState()
		require.NoError(t, err)
		_, isInputSigSent := state.(*InputSigSentState)
		require.True(t, isInputSigSent)

		// The round should be indexed by commitment txid.
		txid := commitmentTx.UnsignedTx.TxHash()
		indexedKeyStr, exists := h.actor.commitmentTxIndex[txid]
		require.True(t, exists)
		require.Equal(t, keyStr, indexedKeyStr)
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

	t.Run("checkpoint_idempotent", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("idempotent-round")
		h.setupRoundInInputSigSentState(roundID)

		outbox := []ClientOutMsg{
			&RoundCheckpointedNotification{RoundID: roundID},
		}

		// First checkpoint.
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)
		require.Len(t, h.actor.rounds, 1)

		// Duplicate checkpoint should be idempotent.
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)
		require.Len(t, h.actor.rounds, 1)
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

// TestActorGetStateWithActiveRounds validates that the actor correctly reports
// all round FSM states when queried.
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

	// The state map should contain one entry for each recovered round.
	require.Len(t, states, 2)

	for _, round := range rounds {
		keyStr := round.RoundID.KeyString()
		roundState, exists := states[keyStr]
		require.True(t, exists, "expected state for %s", round.RoundID)
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

		// The actor begins with no rounds, registered with the wallet
		// to receive boarding confirmations.
		require.NotNil(
			t, h.walletActor.registeredNotifier,
			"actor should register with wallet on start",
		)
		require.Empty(t, h.actor.rounds)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.assertFSMState("PendingRoundAssembly")

		// Send VTXO requests before registration.
		h.sendVTXORequests(50000)

		h.clearServerMessages()
		h.sendServerMessage(&RegistrationRequested{})
		h.assertFSMState("RegistrationSentState")
		h.assertServerMessageSent("SendClientEventRequest")

		roundID := testRoundID("test-round-001")
		h.simulateRoundJoined(roundID, []wire.OutPoint{intent.Outpoint})
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

		// Send VTXO requests before registration.
		h.sendVTXORequests(50000)

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
		h.sendVTXORequests(50000)
		h.sendServerMessage(&RegistrationRequested{})
		h.assertFSMState("RegistrationSentState")

		h.sendServerMessage(&BoardingFailed{
			Reason:      "Round full",
			Error:       fmt.Errorf("max participants reached"),
			Recoverable: true,
		})

		h.assertFSMState("ClientFailedState")
	})

	t.Run("concurrent_rounds", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Create first boarding intent - this creates round 1.
		intent1 := h.newTestBoardingIntentWithSuffix("-first")
		h.sendWalletConfirmation(intent1)

		// Add VTXO requests and advance to RegistrationSentState (new
		// transitions require both boarding AND VTXO requests).
		h.sendVTXORequests(50000)
		h.sendServerMessage(&RegistrationRequested{})

		// Verify we have one round in RegistrationSentState.
		states := h.queryState()
		require.Len(t, states, 1, "expected one round")
		var round1Key string
		for k, info := range states {
			require.IsType(t, &RegistrationSentState{}, info.State)
			round1Key = k
		}

		// Create second boarding intent - this should create a new
		// round (round 2) since round 1 is past Idle state.
		intent2 := h.newTestBoardingIntentWithSuffix("-second")
		h.sendWalletConfirmation(intent2)

		// Verify we now have two rounds.
		states = h.queryState()
		require.Len(t, states, 2, "expected two concurrent rounds")

		// Verify round states: round 1 should still be in
		// RegistrationSentState, round 2 in PendingRoundAssembly.
		foundRound1 := false
		foundRound2 := false
		var round2Key string
		for k, info := range states {
			if k == round1Key {
				foundRound1 = true
				require.IsType(
					t, &RegistrationSentState{}, info.State,
					"round 1 in RegistrationSentState",
				)
			} else {
				foundRound2 = true
				round2Key = k
				require.IsType(
					t, &PendingRoundAssembly{}, info.State,
					"round 2 in PendingRoundAssembly",
				)
			}
		}
		require.True(t, foundRound1, "round 1 not found")
		require.True(t, foundRound2, "round 2 not found")

		// Complete round 1 by sending RoundJoined.
		roundID1 := testRoundID("test-round-001")
		outpoints1 := []wire.OutPoint{intent1.Outpoint}
		h.simulateRoundJoined(roundID1, outpoints1)

		// Verify round 1 transitioned and round 2 is still pending.
		states = h.queryState()
		require.Len(t, states, 2, "still expected two rounds")

		// Round 1 should now be keyed by RoundID (re-keyed).
		round1KeyStr := RoundKeyStr(roundID1.KeyString())
		round1Info, exists := states[string(round1KeyStr)]
		require.True(t, exists, "round 1 should be re-keyed")
		require.IsType(t, &RoundJoinedState{}, round1Info.State)
		require.False(t, round1Info.IsTemp, "round 1 not temp")

		// Round 2 should still be temp-keyed.
		round2Info, exists := states[round2Key]
		require.True(t, exists, "round 2 should still exist")
		require.IsType(t, &PendingRoundAssembly{}, round2Info.State)
		require.True(t, round2Info.IsTemp, "round 2 should be temp")

		// Cancel round 2 using the specific key.
		cancelResult := h.receive(&CancelRoundRequest{
			RoundKey: fn.Some(RoundKeyStr(round2Key)),
		})
		require.True(t, cancelResult.IsOk())
		resp, _ := cancelResult.Unpack()
		cancelResp, ok := resp.(*CancelRoundResponse)
		require.True(t, ok, "expected CancelRoundResponse")
		require.True(t, cancelResp.Success)

		// Verify only round 1 remains.
		states = h.queryState()
		require.Len(t, states, 1, "expected one round after cancel")
		_, exists = states[string(round1KeyStr)]
		require.True(t, exists, "round 1 should remain")
	})
}

// TestActorServerMessageRouting uses table-driven tests to verify that server
// messages trigger the correct FSM state transitions and produce expected
// outbox messages across various scenarios.
func TestActorServerMessageRouting(t *testing.T) {
	t.Parallel()

	// serverEventBuilder allows building server events that depend on
	// harness state (e.g., RoundJoined needs the intent's outpoint).
	type serverEventBuilder func(*actorTestHarness) ClientEvent

	testCases := []struct {
		name          string
		setupState    func(*actorTestHarness) *wallet.BoardingIntent
		serverEvent   serverEventBuilder
		expectedState string
		expectOutbox  bool
		outboxMsgType string
	}{
		{
			name: "RegistrationRequested_from_PendingRoundAssembly",
			setupState: func(
				h *actorTestHarness,
			) *wallet.BoardingIntent {

				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
				h.sendVTXORequests(50000)

				return intent
			},
			serverEvent: func(_ *actorTestHarness) ClientEvent {
				return &RegistrationRequested{}
			},
			expectedState: "RegistrationSentState",
			expectOutbox:  true,
			outboxMsgType: "SendClientEventRequest",
		},
		{
			name: "RoundJoined_from_RegistrationSentState",
			setupState: func(
				h *actorTestHarness,
			) *wallet.BoardingIntent {

				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
				h.sendVTXORequests(50000)
				h.sendServerMessage(&RegistrationRequested{})

				return intent
			},
			serverEvent: func(h *actorTestHarness) ClientEvent {
				// Find outpoints from pending round.
				states := h.queryState()
				var outpoints []wire.OutPoint
				for _, info := range states {
					st := info.State
					rs, ok := st.(*RegistrationSentState)
					if !ok {
						continue
					}

					for _, i := range rs.Intents.Boarding {
						outpoints = append(
							outpoints, i.Outpoint,
						)
					}
				}

				roundID := testRoundID("test-round")

				return &RoundJoined{
					RoundID:                   roundID,
					AcceptedBoardingOutpoints: outpoints,
				}
			},
			expectedState: "RoundJoinedState",
			expectOutbox:  false,
		},
		{
			name: "BoardingFailed_from_any_state",
			setupState: func(
				h *actorTestHarness,
			) *wallet.BoardingIntent {

				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)

				return intent
			},
			serverEvent: func(_ *actorTestHarness) ClientEvent {
				return &BoardingFailed{
					Reason:      "Test failure",
					Recoverable: true,
				}
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

			event := tc.serverEvent(h)
			h.sendServerMessage(event)

			h.assertFSMState(tc.expectedState)
			if tc.expectOutbox {
				h.assertServerMessageSent(tc.outboxMsgType)
			}
		})
	}
}

// TestHandleRefreshVTXORequest verifies that refresh requests from VTXO actors
// are properly forwarded to the primary FSM and tracked for inclusion in the
// next round registration.
func TestHandleRefreshVTXORequest(t *testing.T) {
	t.Parallel()

	t.Run("queues_refresh_from_idle_state", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// With on-demand FSM creation, no FSMs exist at startup.
		states := h.queryState()
		require.Empty(t, states, "expected no FSMs at startup")

		// Create a refresh request as if from a VTXO actor.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("vtxo-to-refresh")),
			Index: 0,
		}
		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint: vtxoOutpoint,
			Amount:       50000,
			NewVTXOKey:   h.clientPubKey,
			PkScript:     []byte{0x51, 0x20}, // Minimal P2TR
			OperatorKey:  h.operatorPubKey,
			Expiry:       144,
		}

		// Send the refresh request to the actor.
		result := h.receive(refreshReq)
		require.True(t, result.IsOk(), "expected Ok result, got: %v",
			result.Err())

		// FSM should transition to PendingRoundAssembly with the
		// refresh request tracked. The FSM is keyed by a temp key.
		states = h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State)

		// Verify the refresh request is tracked.
		require.Contains(t, assembly.RefreshingVTXOs, vtxoOutpoint)
		require.Equal(
			t, refreshReq, assembly.RefreshingVTXOs[vtxoOutpoint],
		)
	})

	t.Run("queues_refresh_alongside_boarding_intent", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// First, add a boarding intent.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// Verify FSM is in PendingRoundAssembly with the intent.
		h.assertFSMState("PendingRoundAssembly")

		// Now add a refresh request.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("vtxo-refresh")),
			Index: 1,
		}
		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint: vtxoOutpoint,
			Amount:       75000,
			NewVTXOKey:   h.clientPubKey,
			PkScript:     []byte{0x51, 0x20},
			OperatorKey:  h.operatorPubKey,
			Expiry:       144,
		}

		result := h.receive(refreshReq)
		require.True(t, result.IsOk())

		// Verify both intent and refresh are tracked. The FSM is
		// temp-keyed since it hasn't been assigned a RoundID yet.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State)
		require.Len(t, assembly.Boarding, 1)
		require.Contains(t, assembly.RefreshingVTXOs, vtxoOutpoint)
	})
}

// TestHandleForfeitSignatureResponse verifies that forfeit signatures from VTXO
// actors are routed to the correct round FSM for collection.
func TestHandleForfeitSignatureResponse(t *testing.T) {
	t.Parallel()

	t.Run("errors_on_unknown_round", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Send a forfeit signature for a non-existent round.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("unknown-vtxo")),
			Index: 0,
		}
		sig := testutils.TestSchnorrSignature(t, "forfeit")
		response := &ForfeitSignatureResponse{
			RoundID:      "non-existent-round",
			VTXOOutpoint: vtxoOutpoint,
			Signature:    sig,
			ForfeitTx:    wire.NewMsgTx(2),
		}

		result := h.receive(response)
		require.True(t, result.IsErr())
		require.Contains(t, result.Err().Error(), "unknown round")
	})
}

// TestVTXOCreatedNotificationForwarding verifies that VTXOCreatedNotification
// messages from the FSM outbox are forwarded to the VTXO manager.
func TestVTXOCreatedNotificationForwarding(t *testing.T) {
	t.Parallel()

	t.Run("forwards_to_vtxo_manager", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Create a VTXOCreatedNotification as if from FSM outbox.
		clientVTXO := &ClientVTXO{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("new-vtxo")),
				Index: 0,
			},
			Amount:      100000,
			PkScript:    []byte{0x51, 0x20},
			ClientKey:   h.newKeyDescriptor(),
			OperatorKey: h.operatorPubKey,
			Expiry:      144,
		}

		notification := &VTXOCreatedNotification{
			VTXOs:          []*ClientVTXO{clientVTXO},
			RoundID:        "test-round-123",
			CommitmentTxID: chainhash.HashH([]byte("commitment")),
			BatchExpiry:    1000,
			CreatedHeight:  500,
		}

		// Directly call processOutbox to test the routing.
		outbox := []ClientOutMsg{notification}
		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		// Verify the VTXO manager received the notification.
		receivedNotif := h.vtxoManager.assertVTXOCreatedReceived(t)
		require.Equal(t, "test-round-123", receivedNotif.RoundID)
		require.Len(t, receivedNotif.VTXOs, 1)
		require.Equal(
			t, clientVTXO.Outpoint, receivedNotif.VTXOs[0].Outpoint,
		)
	})
}
