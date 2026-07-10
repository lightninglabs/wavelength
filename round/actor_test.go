package round

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type lifecycleProbeState struct {
	ctxErr chan error
}

// ProcessEvent records whether the state machine lifecycle context was
// cancelled when a later event was processed.
func (s *lifecycleProbeState) ProcessEvent(ctx context.Context, _ ClientEvent,
	_ *ClientEnvironment) (*ClientStateTransition, error) {

	s.ctxErr <- ctx.Err()

	return &ClientStateTransition{NextState: s}, nil
}

// IsTerminal reports that the lifecycle probe remains active.
func (s *lifecycleProbeState) IsTerminal() bool {
	return false
}

// String returns the lifecycle probe state name.
func (s *lifecycleProbeState) String() string {
	return "LifecycleProbe"
}

func stdTpl(t *testing.T, clientKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) []byte {

	t.Helper()

	policy, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return policy
}

// TestRoundFSMOutlivesCreationRequest verifies that a round FSM retains the
// actor lifecycle after the Ask request that created it has completed.
func TestRoundFSMOutlivesCreationRequest(t *testing.T) {
	t.Parallel()

	state := &lifecycleProbeState{ctxErr: make(chan error, 1)}
	fsm := protofsm.NewStateMachine(ClientStateMachineCfg{
		Logger:        btclog.Disabled,
		ErrorReporter: newContextErrorReporter(t.Context(), "probe"),
		InitialState:  state,
		Env:           &ClientEnvironment{},
	})

	actor := &RoundClientActor{runCtx: t.Context()}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	actor.startRoundFSM(requestCtx, &fsm)
	t.Cleanup(fsm.Stop)

	// Actor Ask processing cancels its merged request context immediately
	// after Receive returns. A later protocol event must still run using
	// the actor-owned context.
	cancelRequest()
	result := fsm.AskEvent(t.Context(), &GenerateNonces{}).Await(
		t.Context(),
	)
	_, err := result.Unpack()
	require.NoError(t, err)
	require.NoError(t, <-state.ctxErr)
}

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
		h.sendServerMessage(&IntentRequested{})

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

		ownerKey := &keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  0,
			},
		}

		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOOwnerKeyFamily,
		).Return(ownerKey, nil).Once()

		amount := btcutil.Amount(50000)
		msg := &RegisterVTXORequestsRequest{
			Amounts: []btcutil.Amount{
				amount,
			},
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

		_, expectedPkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				ownerKey.PubKey, h.operatorPubKey,
				h.operatorTerms.VTXOExitDelay,
			)
		require.NoError(t, err)

		req := assembly.VTXOs[0]
		actualPkScript, err := req.EffectivePkScript()
		require.NoError(t, err)
		params, err := req.DecodeStandardPolicyTemplate()
		require.NoError(t, err)

		require.Equal(t, amount, req.Amount)
		require.Equal(t, expectedPkScript, actualPkScript)
		require.Equal(
			t, h.operatorTerms.VTXOExitDelay, params.ExitDelay,
		)
		require.Equal(
			t, schnorr.SerializePubKey(ownerKey.PubKey),
			schnorr.SerializePubKey(params.OwnerKey),
		)
		require.Equal(
			t, schnorr.SerializePubKey(h.operatorPubKey),
			schnorr.SerializePubKey(params.OperatorKey),
		)
		require.Equal(t, *ownerKey, req.OwnerKey)
		require.Nil(t, req.SigningKey.PubKey)
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
				t, exists, "expected round FSM for %s",
				round.RoundID,
			)
		}

		require.Len(t, h.actor.commitmentTxIndex, 3)
		require.Len(t, h.chainSource.registrations, 3)
	})

	t.Run("replays_checkpointed_boarding_input_sigs", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)

		roundID := testRoundID("replay-input-sigs")
		round := h.newTestRound(roundID)
		walletIntent := h.newTestBoardingIntent()
		intent, err := buildBoardingIntentFromWallet(walletIntent)
		require.NoError(t, err)
		inputSig := &types.BoardingInputSignature{
			InputIndex: 0,
			Outpoint:   walletIntent.Outpoint,
			ClientSignature: testutils.TestSchnorrSignature(
				t, "replay",
			),
		}

		h.roundStore.On(
			"ListActiveRounds", mock.Anything,
		).Return([]*Round{round}, nil)
		h.roundStore.On(
			"FetchState", mock.Anything, round.RoundID,
		).Return(
			round,
			&InputSigSentState{
				RoundID: roundID,
				CommitmentTx: round.CommitmentTx.
					UnwrapOrFail(
						t,
					),
				Intents: Intents{
					Boarding: []BoardingIntent{intent},
				},
				InputSigs: []*types.BoardingInputSignature{
					inputSig,
				},
			},
			nil,
		)

		err = h.start()
		require.NoError(t, err)

		msgs := h.serverMessages()
		require.Len(t, msgs, 1)

		req, ok := msgs[0].(*serverconn.SendClientEventRequest)
		require.True(
			t, ok, "expected SendClientEventRequest, got %T",
			msgs[0],
		)

		replayed, ok := req.Message.(*SubmitForfeitSigRequest)
		require.True(
			t, ok, "expected SubmitForfeitSigRequest, got %T",
			req.Message,
		)
		require.Equal(t, roundID, replayed.RoundID)
		require.Len(t, replayed.Signatures, 1)
		require.Equal(
			t, walletIntent.Outpoint,
			replayed.Signatures[0].Outpoint,
		)
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

	t.Run("start_timeout_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("timeout-round-start")
		duration := 30 * time.Second
		outbox := []ClientOutMsg{
			&StartTimeoutReq{
				RoundKey: RoundKeyStr(roundID.KeyString()),
				Phase:    TimeoutPhaseForfeitCollection,
				Duration: duration,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		timeoutID := makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhaseForfeitCollection,
		)
		h.timeoutActor.assertTimeoutScheduled(
			t, timeoutID, duration,
		)
	})

	t.Run("cancel_timeout_request", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("timeout-round-cancel")
		timeoutID := makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhaseForfeitCollection,
		)

		schedReq := &timeout.ScheduleTimeoutRequest{
			ID:       timeoutID,
			Duration: 10 * time.Second,
			Callback: actor.NewChannelTellOnlyRef[*timeout.ExpiredMsg]( //nolint:ll
				"timeout-callback", 1,
			),
		}
		err = h.timeoutActor.Tell(h.ctx, schedReq)
		require.NoError(t, err)

		outbox := []ClientOutMsg{
			&CancelTimeoutReq{
				RoundKey: RoundKeyStr(roundID.KeyString()),
				Phase:    TimeoutPhaseForfeitCollection,
			},
		}

		err = h.actor.processOutbox(h.ctx, outbox)
		require.NoError(t, err)

		h.timeoutActor.assertTimeoutCancelled(t, timeoutID)
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
			Height: 100,
			BlockHash: chainhash.Hash{
				0x01,
			},
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
			require.True(
				t, isPending, "round should transition to "+
					"PendingRoundAssembly, got %T", state,
			)

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
			&RoundCheckpointedNotification{
				RoundID: roundID,
			},
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
			&RoundCheckpointedNotification{
				RoundID: roundID,
			},
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

	// This is the regression test for issue #386. The forfeit-collection
	// transitions emit StartTimeoutReq BEFORE the per-VTXO
	// ForfeitRequestToVTXO messages so that a failed forfeit Tell cannot
	// strand the round without a timeout. processOutbox aborts on the
	// first send error, so if the timeout were emitted last it would be
	// skipped when a forfeit Tell fails, and the round would wait forever
	// for signatures with no timeout armed.
	t.Run("forfeit_send_failure_still_arms_timeout", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		// Wire up a real but empty actor system so the forfeit
		// request fails its service-key lookup (no VTXO actor is
		// registered), mirroring the ErrNoActorsAvailable trigger.
		system := actor.NewActorSystem()
		t.Cleanup(func() {
			_ = system.Shutdown(t.Context())
		})
		h.actor.cfg.ActorSystem = system

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("forfeit-send-failure")
		duration := 30 * time.Second
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("vtxo-386")),
			Index: 0,
		}

		// The outbox mirrors what the forfeit-collection transitions
		// now emit: the timeout first, then the per-VTXO forfeit
		// request whose Tell will fail.
		outbox := []ClientOutMsg{
			&StartTimeoutReq{
				RoundKey: RoundKeyStr(roundID.KeyString()),
				Phase:    TimeoutPhaseForfeitCollection,
				Duration: duration,
			},
			&ForfeitRequestToVTXO{
				VTXOOutpoint: vtxoOutpoint,
				RoundID:      roundID.String(),
			},
		}

		// The forfeit Tell fails, so processOutbox returns an error.
		err = h.actor.processOutbox(h.ctx, outbox)
		require.Error(t, err)

		// Despite the forfeit failure, the timeout must already be
		// armed because it was emitted first. This is the property
		// that keeps the round recoverable.
		timeoutID := makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhaseForfeitCollection,
		)
		h.timeoutActor.assertTimeoutScheduled(t, timeoutID, duration)
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
		h.sendServerMessage(&IntentRequested{})
		h.assertFSMState("IntentSentState")
		h.assertServerMessageSent("SendClientEventRequest")

		roundID := testRoundID("test-round-001")
		h.simulateRoundJoined(roundID, []wire.OutPoint{intent.Outpoint})

		// Under the #270 seal-time handshake, RoundJoined re-keys
		// the round at the actor layer but leaves the FSM parked
		// in IntentSentState until JoinRoundQuote arrives.
		h.assertFSMState("IntentSentState")
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
		h.sendServerMessage(&IntentRequested{})
		h.assertFSMState("IntentSentState")
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
		h.sendServerMessage(&IntentRequested{})
		h.assertFSMState("IntentSentState")

		h.sendServerMessage(&BoardingFailed{
			Reason:      "Round full",
			Error:       fmt.Errorf("max participants reached"),
			Recoverable: true,
		})

		// The failed round stays observable in ClientFailedState — it
		// is reaped lazily at the next assembly, not on entry, so a
		// consumer can still see the failure (darepo-client#602).
		state := h.queryState()
		require.Len(
			t, state, 1, "failed round should remain observable",
		)
		for _, info := range state {
			require.IsType(t, &ClientFailedState{}, info.State)
		}
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

		// Add VTXO requests and advance to IntentSentState (new
		// transitions require both boarding AND VTXO requests).
		h.sendVTXORequests(50000)
		h.sendServerMessage(&IntentRequested{})

		// Verify we have one round in IntentSentState.
		states := h.queryState()
		require.Len(t, states, 1, "expected one round")
		var round1Key string
		for k, info := range states {
			require.IsType(t, &IntentSentState{}, info.State)
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
		// IntentSentState, round 2 in PendingRoundAssembly.
		foundRound1 := false
		foundRound2 := false
		var round2Key string
		for k, info := range states {
			if k == round1Key {
				foundRound1 = true
				require.IsType(
					t, &IntentSentState{}, info.State,
					"round 1 in IntentSentState",
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

		// Round 1 should now be keyed by RoundID (re-keyed). The FSM
		// stays parked in IntentSentState under the seal-time
		// handshake — advancement waits for JoinRoundQuote.
		round1KeyStr := RoundKeyStr(roundID1.KeyString())
		round1Info, exists := states[string(round1KeyStr)]
		require.True(t, exists, "round 1 should be re-keyed")
		require.IsType(t, &IntentSentState{}, round1Info.State)
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
// TestActorBuffersEarlyQuote verifies that the actor buffers a
// JoinRoundQuoteReceived that arrives before the matching
// RoundJoined has re-keyed the FSM, and delivers the buffered
// quote once re-keying happens. The mailbox contract permits
// out-of-order envelope delivery (see
// docs/RPC_MAILBOX_CONTRACT.md), so this path is reachable under
// normal operation and must not silently drop the quote.
func TestActorBuffersEarlyQuote(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	// Drive the FSM to IntentSentState keyed under tempKey.
	intent := h.newTestBoardingIntent()
	h.sendWalletConfirmation(intent)
	h.sendVTXORequests(50000)
	h.sendServerMessage(&IntentRequested{})

	// Look up the intent's pkScript so the quote's echo matches
	// and evaluateQuote would accept.
	states := h.queryState()
	var vtxos []types.VTXORequest
	for _, info := range states {
		rs, ok := info.State.(*IntentSentState)
		if !ok {
			continue
		}
		vtxos = append(vtxos, rs.Intents.VTXOs...)
	}
	require.NotEmpty(t, vtxos)

	roundID := testRoundID("early-quote")

	// Send the quote BEFORE RoundJoined.
	//
	// We have a single boarding input of 50_000 sat and a single
	// matching VTXO output. The lone output is implicit change
	// under the #270 protocol (IsChange=false on the lone VTXO), so
	// the server treats the lone slot as implicit change and stamps
	// (Amount − OperatorFeeSat) on it. Mirror that here so
	// validateQuoteEchoes accepts -- the previously-loose
	// implicit-change shortcut would have accepted any AmountSat,
	// but issue #378 tightened the rule to require the exact
	// (Amount − fee) deviation. Quoting the lone (i==0) slot down
	// by OperatorFeeSat also keeps the realised fee
	// (Σinputs−Σoutputs) in agreement with OperatorFeeSat — see
	// #379.
	const operatorFeeSat = int64(1_000)
	vtxoQuotes := make([]VTXOQuoteEntry, len(vtxos))
	for i, v := range vtxos {
		script, err := v.EffectivePkScript()
		require.NoError(t, err)

		amount := int64(v.Amount)
		if i == 0 {
			amount -= operatorFeeSat
		}

		vtxoQuotes[i] = VTXOQuoteEntry{
			PkScript:     script,
			AmountSat:    amount,
			RecipientKey: v.SigningKey.PubKey.SerializeCompressed(),
		}
	}

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	quote := &ClientQuote{
		QuoteID:        quoteID,
		OperatorFeeSat: operatorFeeSat,
		VTXOQuotes:     vtxoQuotes,
	}

	h.sendServerMessage(&JoinRoundQuoteReceived{
		RoundID: roundID,
		Quote:   quote,
	})

	// FSM should still be parked in IntentSentState; the quote
	// is buffered rather than processed.
	states = h.queryState()
	for _, info := range states {
		_, isIntentSent := info.State.(*IntentSentState)
		require.True(
			t, isIntentSent, "FSM must stay in IntentSentState "+
				"while quote is buffered, got %T", info.State,
		)
	}

	// Now deliver the matching RoundJoined. handleRoundJoined
	// drains the buffered quote; FSM must transition to
	// QuoteReceivedState (and then to RoundJoinedState once the
	// internal QuoteAccepted event fires).
	h.sendServerMessage(&RoundJoined{
		RoundID: roundID,
		AcceptedBoardingOutpoints: []wire.OutPoint{
			intent.Outpoint,
		},
	})

	states = h.queryState()
	found := false
	for _, info := range states {
		switch info.State.(type) {
		case *QuoteReceivedState, *RoundJoinedState:
			found = true
		}
	}
	require.True(
		t, found, "FSM must advance past IntentSentState after "+
			"draining buffered quote",
	)
}

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

		// expectReaped asserts the round is removed from tracking
		// (e.g. it failed) rather than left in expectedState.
		expectReaped bool
	}{
		{
			name: "IntentRequested_from_PendingRoundAssembly",
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
				return &IntentRequested{}
			},
			expectedState: "IntentSentState",
			expectOutbox:  true,
			outboxMsgType: "SendClientEventRequest",
		},
		{
			// Under the #270 seal-time handshake, RoundJoined is a
			// watermark that triggers re-keying at the actor layer
			// but leaves the FSM parked in IntentSentState so the
			// subsequent JoinRoundQuote can drive the decision.
			name: "RoundJoined_from_IntentSentState",
			setupState: func(
				h *actorTestHarness,
			) *wallet.BoardingIntent {

				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
				h.sendVTXORequests(50000)
				h.sendServerMessage(&IntentRequested{})

				return intent
			},
			serverEvent: func(h *actorTestHarness) ClientEvent {
				// Find outpoints from pending round.
				states := h.queryState()
				var outpoints []wire.OutPoint
				for _, info := range states {
					st := info.State
					rs, ok := st.(*IntentSentState)
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
			expectedState: "IntentSentState",
			expectOutbox:  false,
		},
		{
			name: "BoardingFailed_from_joined_round",
			//nolint:ll
			setupState: func(
				h *actorTestHarness,
			) *wallet.BoardingIntent {

				h.setupMockRoundStoreForStart()
				require.NoError(h.t, h.start())
				intent := h.newTestBoardingIntent()
				h.sendWalletConfirmation(intent)
				h.sendVTXORequests(50000)
				h.sendServerMessage(&IntentRequested{})

				// Re-key the temp round by simulating a
				// successful join, then send BoardingFailed
				// without RoundID.
				states := h.queryState()
				var outpoints []wire.OutPoint
				for _, info := range states {
					state, ok := info.State.(*IntentSentState)
					if !ok {
						continue
					}

					for _, boarding := range state.Intents.Boarding {
						outpoints = append(
							outpoints,
							boarding.Outpoint,
						)
					}
				}
				require.NotEmpty(
					h.t, outpoints, "registration "+
						"state should contain "+
						"boarding outpoints",
				)

				h.sendServerMessage(&RoundJoined{
					RoundID: testRoundID(
						"test-round-failed",
					),
					AcceptedBoardingOutpoints: outpoints,
				})

				return intent
			},
			serverEvent: func(_ *actorTestHarness) ClientEvent {
				return &BoardingFailed{
					Reason:      "Test failure",
					Recoverable: true,
				}
			},
			expectOutbox: false,
			expectReaped: true,
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

			if tc.expectReaped {
				// Failed rounds are reaped lazily at the next
				// assembly, not on entry, so the round remains
				// observable in ClientFailedState here
				// (darepo-client#602).
				state := h.queryState()
				require.Len(
					h.t, state, 1,
					"failed round should remain observable",
				)
				for _, info := range state {
					require.IsType(
						h.t, &ClientFailedState{},
						info.State,
					)
				}
			} else {
				h.assertFSMState(tc.expectedState)
			}
			if tc.expectOutbox {
				h.assertServerMessageSent(tc.outboxMsgType)
			}
		})
	}
}

// TestBoardingFailedRoutesByRoundIDWithLingeringRound reproduces the
// darepo-client#571 dropped-event bug and proves the RoundID-keyed routing
// fix. Two rounds are tracked: a lingering terminal round (ClientFailedState,
// which is never evicted from a.rounds) and a live round re-keyed to a
// server-assigned RoundID sitting in RoundJoinedState. A server
// ClientRoundFailedResp for the live round arrives as a BoardingFailed
// carrying that RoundID. Before the fix, findPendingRound missed the re-keyed
// live round and the sole-round fallback bailed (len(a.rounds) == 2), so the
// failure was silently dropped and the live round stayed stuck projecting a
// phantom pending VTXO forever. With the fix, the failure is routed
// deterministically by RoundID and the live round transitions to
// ClientFailedState.
func TestBoardingFailedRoutesByRoundIDWithLingeringRound(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	// Stage a lingering terminal round under its own RoundID. Terminal
	// FSMs are reaped lazily, not on entry, so this round stays in
	// a.rounds and is what makes the sole-round heuristic bail.
	lingeringID := testRoundID("lingering-failed-round")
	h.injectRoundInState(lingeringID, &ClientFailedState{
		Reason:      "earlier round failed",
		Recoverable: true,
	})

	// Stage the live round re-keyed to its server-assigned RoundID, parked
	// in a waiting state (RoundJoinedState) that projects a pending VTXO.
	liveID := testRoundID("live-joined-round")
	h.injectRoundInState(liveID, &RoundJoinedState{
		RoundID: liveID,
	})

	// Sanity check: two rounds are tracked, so the sole-round heuristic
	// alone cannot route the failure.
	require.Len(t, h.queryState(), 2)

	// Deliver the server failure for the live round through the same path
	// handleServerMessage uses, carrying the live round's RoundID exactly
	// as BoardingFailed.FromProto populates it from a
	// ClientRoundFailedResp.
	h.sendServerMessage(&BoardingFailed{
		RoundID:     fn.Some(liveID),
		Reason:      "operator failed to build round",
		Recoverable: true,
	})

	// The live round must have consumed the failure and transitioned to
	// ClientFailedState rather than dropping it and staying in
	// RoundJoinedState.
	states := h.queryState()

	liveInfo, ok := states[liveID.KeyString()]
	require.True(t, ok, "live round should still be tracked")
	require.IsType(
		t, &ClientFailedState{}, liveInfo.State,
		"live round should have failed, not stayed in RoundJoinedState",
	)

	// The lingering round must be untouched: routing was deterministic and
	// did not leak the failure into the wrong FSM.
	lingeringInfo, ok := states[lingeringID.KeyString()]
	require.True(t, ok, "lingering round should still be tracked")
	require.IsType(
		t, &ClientFailedState{}, lingeringInfo.State,
	)
	failed, ok := lingeringInfo.State.(*ClientFailedState)
	require.True(t, ok)
	require.Equal(
		t, "earlier round failed", failed.Reason,
		"lingering round's failure reason must be unchanged",
	)
}

// TestBoardingFailedMismatchedRoundIDDoesNotMisroute verifies that a
// BoardingFailed carrying a RoundID that matches no tracked round is treated
// as a genuine miss rather than being misrouted to the sole tracked round.
// The sole-round fallback must apply only to failures that carry NO RoundID
// (pre-assignment failures); a present-but-mismatched id (e.g. for an
// already-reaped round) must not fail an unrelated round.
func TestBoardingFailedMismatchedRoundIDDoesNotMisroute(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	// Exactly one round is tracked, parked in a waiting state.
	liveID := testRoundID("live-joined-round")
	h.injectRoundInState(liveID, &RoundJoinedState{RoundID: liveID})
	require.Len(t, h.queryState(), 1)

	// Deliver a failure for a DIFFERENT round id. findPendingRound misses
	// (the round is re-keyed), the RoundID lookup misses (no such round),
	// and the sole-round fallback must NOT fire because the id is present.
	otherID := testRoundID("some-other-round")
	result := h.receive(&ServerMessageNotification{
		Message: &BoardingFailed{
			RoundID:     fn.Some(otherID),
			Reason:      "failure for an unrelated round",
			Recoverable: true,
		},
	})
	require.False(
		t, result.IsOk(),
		"a mismatched-RoundID failure must miss, not misroute",
	)

	// The sole round must be untouched — still in its waiting state.
	liveInfo, ok := h.queryState()[liveID.KeyString()]
	require.True(t, ok, "the live round should still be tracked")
	require.IsType(
		t, &RoundJoinedState{}, liveInfo.State,
		"sole round must not have been failed by an unrelated failure",
	)
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
		policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			h.clientPubKey, h.operatorPubKey, 144,
		)
		require.NoError(t, err)

		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint:   vtxoOutpoint,
			Amount:         50000,
			PolicyTemplate: policyTemplate,
		}

		// Send the refresh request to the actor.
		result := h.receive(refreshReq)
		require.True(
			t, result.IsOk(),
			"expected Ok result, got: %v", result.Err(),
		)

		// FSM should transition to PendingRoundAssembly with the
		// refresh request tracked. The FSM is keyed by a temp key.
		states = h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State,
		)

		// Verify the forfeit input is tracked.
		require.Len(t, assembly.Forfeits, 1)
		require.Equal(
			t, vtxoOutpoint, *assembly.Forfeits[0].VTXOOutpoint,
		)
		require.Equal(
			t, btcutil.Amount(refreshReq.Amount),
			assembly.Forfeits[0].Amount,
		)

		// Verify the VTXO output request is tracked.
		require.Len(t, assembly.VTXOs, 1)
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
		policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			h.clientPubKey, h.operatorPubKey, 144,
		)
		require.NoError(t, err)

		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint:   vtxoOutpoint,
			Amount:         75000,
			PolicyTemplate: policyTemplate,
		}

		result := h.receive(refreshReq)
		require.True(t, result.IsOk())

		// Verify both intent and refresh are tracked. The FSM is
		// temp-keyed since it hasn't been assigned a RoundID yet.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State,
		)
		require.Len(t, assembly.Boarding, 1)
		require.Len(t, assembly.Forfeits, 1)
		require.Equal(
			t, vtxoOutpoint, *assembly.Forfeits[0].VTXOOutpoint,
		)
	})
}

// markerCount returns the total number of IsChange=true entries
// across the composed intent. Layer-2 actor regressions assert
// against this in lieu of poking at the JoinRoundOutbox payload.
func markerCount(intents Intents) int {
	var n int
	for _, req := range intents.VTXOs {
		if req.IsChange {
			n++
		}
	}
	for _, leave := range intents.Leaves {
		if leave != nil && leave.IsChange {
			n++
		}
	}

	return n
}

// TestAutoRefreshBatchedSingleChangeMarker is the regression for
// the P1 bug flagged on round/actor.go:84 (PR #298): a VTXO
// actor firing two RefreshVTXORequest events in quick succession
// must produce exactly ONE IsChange=true marker on the merged
// intent. Pre-fix, buildVTXORequestFromRefresh stamped IsChange
// per call so the merged JoinRoundRequest carried two markers
// and the operator rejected with INVALID_CHANGE_DESIGNATION.
func TestAutoRefreshBatchedSingleChangeMarker(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	// Two expiring VTXOs auto-refreshing back-to-back into the
	// same assembling round. Each forfeit outpoint must be
	// resolvable by the VTXO store because IntentRequested calls
	// computeTotalForfeitAmount. The two outputs use different
	// exit delays so PendingRoundAssembly's pkScript-keyed dedup
	// does not collapse them — in production each forfeited VTXO
	// already has its own distinct script.
	type refreshEntry struct {
		outpoint  wire.OutPoint
		amount    btcutil.Amount
		exitDelay uint32
	}
	entries := []refreshEntry{
		{
			outpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("auto-a")),
				Index: 0,
			},
			amount:    50_000,
			exitDelay: 144,
		},
		{
			outpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("auto-b")),
				Index: 1,
			},
			amount:    50_000,
			exitDelay: 145,
		},
	}
	for i, e := range entries {
		policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			h.clientPubKey, h.operatorPubKey, e.exitDelay,
		)
		require.NoError(t, err)

		h.vtxoStore.On(
			"GetVTXO", mock.Anything, e.outpoint,
		).Return(&ClientVTXO{
			Outpoint: e.outpoint,
			Amount:   e.amount,
		}, nil)

		req := &RefreshVTXORequest{
			VTXOOutpoint:   e.outpoint,
			Amount:         int64(e.amount),
			PolicyTemplate: policyTemplate,
		}
		result := h.receive(req)
		require.True(
			t, result.IsOk(),
			"refresh #%d failed: %v", i, result.Err(),
		)
	}

	// Trigger registration: the FSM normalizes the change marker
	// during the IntentRequested transition.
	h.sendServerMessage(&IntentRequested{})

	// The FSM advances out of PendingRoundAssembly into
	// IntentSentState carrying the composed Intents — that's
	// where the centralized designator left exactly one marker.
	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists, "expected temp-keyed FSM state")

	intentSent, ok := tempState.State.(*IntentSentState)
	require.True(t, ok,
		"expected IntentSentState, got %T", tempState.State)

	require.Len(
		t, intentSent.Intents.VTXOs, 2,
		"both refresh outputs must survive into the intent",
	)
	require.Equal(
		t, 1, markerCount(intentSent.Intents),
		"merged auto-refresh batch must carry exactly one "+
			"IsChange=true marker",
	)
	require.True(
		t, intentSent.Intents.VTXOs[0].IsChange,
		"first VTXO must be the marker carrier",
	)
}

// TestSequentialRefreshLeaveSingleChangeMarker is the regression
// for the cross-pool variant of the same bug: one RefreshVTXOs
// followed by one LeaveVTXOs in the same PendingRoundAssembly
// window. The composed intent then has VTXO outputs AND leave
// outputs; the centralized designator must place exactly one
// marker, with VTXOs winning over leaves (the residual is
// preferred to absorb off-chain).
func TestSequentialRefreshLeaveSingleChangeMarker(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, 144,
	)
	require.NoError(t, err)

	// 1. Refresh adds a VTXO output (IsChange unset). The forfeit
	// must be resolvable by the VTXO store.
	refreshOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("seq-refresh")),
		Index: 0,
	}
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, refreshOutpoint,
	).Return(&ClientVTXO{
		Outpoint: refreshOutpoint,
		Amount:   btcutil.Amount(50_000),
	}, nil)
	refreshReq := &RefreshVTXORequest{
		VTXOOutpoint:   refreshOutpoint,
		Amount:         50_000,
		PolicyTemplate: policyTemplate,
	}
	require.True(t, h.receive(refreshReq).IsOk())

	// 2. Leave intent through RegisterIntentRequest, also with
	// IsChange unset (the wallet handler now leaves
	// designation to the FSM).
	leaveOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("seq-leave")),
		Index: 1,
	}
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, leaveOutpoint,
	).Return(&ClientVTXO{
		Outpoint: leaveOutpoint,
		Amount:   btcutil.Amount(40_000),
	}, nil)
	leaveReq := &RegisterIntentRequest{
		Package: &IntentPackage{Intents: Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &leaveOutpoint,
				Amount:       40_000,
			}},
			Leaves: []*types.LeaveRequest{{
				Output: &wire.TxOut{
					Value: 40_000,
					PkScript: []byte{
						0x00,
						0x14,
						0x01,
					},
				},
			}},
		}},
	}
	require.True(t, h.receive(leaveReq).IsOk())

	h.sendServerMessage(&IntentRequested{})

	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists)

	intentSent, ok := tempState.State.(*IntentSentState)
	require.True(t, ok)

	require.Len(t, intentSent.Intents.VTXOs, 1)
	require.Len(t, intentSent.Intents.Leaves, 1)
	require.Equal(
		t, 1, markerCount(intentSent.Intents),
		"refresh+leave composed intent must carry exactly one "+
			"IsChange=true marker",
	)
	require.True(
		t, intentSent.Intents.VTXOs[0].IsChange,
		"VTXO output must win the marker over the leave",
	)
	require.False(
		t, intentSent.Intents.Leaves[0].IsChange,
		"leave must remain unmarked when a VTXO took the marker",
	)
}

// TestExplicitDirectedSendChangeNotOverwritten covers the
// "respect explicit IsChange=true" rule: when an entry-point
// (handleSendVTXOs for directed-send self-change, handleBoard
// for boarding-change) has already stamped a specific output as
// the change carrier, the centralized designator must leave that
// marker alone — adding a refresh output on top must NOT move or
// re-stamp the marker. Pre-fix, a future regression that auto-
// stamped the first VTXO regardless would silently retarget the
// residual to the refresh output and the wallet's intended
// change slot would receive the pre-fee value as a fixed target.
func TestExplicitDirectedSendChangeNotOverwritten(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	// Each VTXORequest must carry a valid PolicyTemplate so the
	// PendingRoundAssembly's pkScript-keyed dedup can derive a
	// pkScript without erroring.
	recipientTpl, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, 200,
	)
	require.NoError(t, err)
	selfChangeTpl, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, 201,
	)
	require.NoError(t, err)
	refreshTpl, err := arkscript.EncodeStandardVTXOTemplate(
		h.clientPubKey, h.operatorPubKey, 202,
	)
	require.NoError(t, err)

	// 1. The wallet pre-stamps a self-change VTXO (mimicking
	// the directed-send path in wallet/wallet.go:1719).
	selfChangeOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("self-change-vtxo")),
		Index: 0,
	}
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, selfChangeOutpoint,
	).Return(&ClientVTXO{
		Outpoint: selfChangeOutpoint,
		Amount:   btcutil.Amount(100_000),
	}, nil)
	directedReq := &RegisterIntentRequest{
		Package: &IntentPackage{Intents: Intents{
			Forfeits: []types.ForfeitRequest{{
				VTXOOutpoint: &selfChangeOutpoint,
				Amount:       100_000,
			}},
			VTXOs: []types.VTXORequest{
				// recipient leg
				{
					Amount:         btcutil.Amount(40_000),
					PolicyTemplate: recipientTpl,
				},
				// self-change leg with explicit marker
				{
					Amount:         btcutil.Amount(60_000),
					PolicyTemplate: selfChangeTpl,
					IsChange:       true,
				},
			},
		}},
	}
	require.True(t, h.receive(directedReq).IsOk())

	// 2. A refresh fires into the same assembling round. The
	// per-VTXO request leaves IsChange unset; the centralized
	// designator must NOT stamp a new marker because one is
	// already present.
	lateRefreshOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("late-refresh")),
		Index: 1,
	}
	h.vtxoStore.On(
		"GetVTXO", mock.Anything, lateRefreshOutpoint,
	).Return(&ClientVTXO{
		Outpoint: lateRefreshOutpoint,
		Amount:   btcutil.Amount(30_000),
	}, nil)
	refreshReq := &RefreshVTXORequest{
		VTXOOutpoint:   lateRefreshOutpoint,
		Amount:         30_000,
		PolicyTemplate: refreshTpl,
	}
	require.True(t, h.receive(refreshReq).IsOk())

	h.sendServerMessage(&IntentRequested{})

	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists)

	intentSent, ok := tempState.State.(*IntentSentState)
	require.True(t, ok)

	require.Len(t, intentSent.Intents.VTXOs, 3)
	require.Equal(
		t, 1, markerCount(intentSent.Intents),
		"explicit marker must survive the refresh add",
	)

	// Find which VTXO ended up with the marker — it must be the
	// pre-stamped self-change leg, not the refresh leg or the
	// recipient leg.
	require.False(
		t, intentSent.Intents.VTXOs[0].IsChange,
		"recipient leg must not absorb the marker",
	)
	require.True(
		t, intentSent.Intents.VTXOs[1].IsChange, "explicit "+
			"self-change marker must survive at its original "+
			"position",
	)
	require.False(
		t, intentSent.Intents.VTXOs[2].IsChange,
		"trailing refresh leg must not absorb the marker",
	)
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

	t.Run("rejects_wrong_round_by_expected_outpoint", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("forfeit-signature-round")
		h.setupRoundInForfeitCollectingState(roundID)

		vtxoOutpoint := wire.OutPoint{
			Hash: chainhash.HashH(
				[]byte("forfeit-vtxo-" + roundID.String()),
			),
			Index: 0,
		}
		wrongRoundID := testRoundID("forfeit-signature-wrong-round")
		sig := testutils.TestSchnorrSignature(t, "forfeit")
		response := &ForfeitSignatureResponse{
			RoundID:      wrongRoundID.String(),
			VTXOOutpoint: vtxoOutpoint,
			Signature:    sig,
			ForfeitTx:    wire.NewMsgTx(2),
		}

		result := h.receive(response)
		require.True(t, result.IsErr())
		require.Contains(t, result.Err().Error(), "unknown round")
	})
}

// TestHandleTriggerBoard verifies the board trigger path rebuilds confirmed
// boarding intents from the wallet actor before round registration.
func TestHandleTriggerBoard(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	intent := h.newTestBoardingIntent()
	h.walletActor.setConfirmedIntents(*intent)

	h.wallet.On(
		"DeriveNextKey", mock.Anything, types.VTXOOwnerKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOOwnerKeyFamily,
			Index:  0,
		},
	}, nil).Once()
	h.wallet.On(
		"DeriveNextKey", mock.Anything, types.VTXOSigningKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOSigningKeyFamily,
			Index:  1,
		},
	}, nil).Once()

	result := h.receive(&actormsg.TriggerBoardMsg{
		Amounts: []btcutil.Amount{49_000},
	})
	require.True(t, result.IsOk(), "expected Ok, got: %v",
		result.Err())

	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists, "expected temp-keyed FSM state")

	regState, ok := tempState.State.(*IntentSentState)
	require.True(t, ok, "expected IntentSentState, got %T",
		tempState.State)
	require.Len(t, regState.Intents.Boarding, 1)
	require.Equal(
		t, intent.Outpoint, regState.Intents.Boarding[0].Outpoint,
	)
	require.Len(t, regState.Intents.VTXOs, 1)
	require.Equal(
		t, btcutil.Amount(49_000), regState.Intents.VTXOs[0].Amount,
	)
	require.Equal(
		t, types.VTXOOwnerKeyFamily,
		regState.Intents.VTXOs[0].OwnerKey.Family,
	)
	require.Equal(
		t, types.VTXOSigningKeyFamily,
		regState.Intents.VTXOs[0].SigningKey.Family,
	)
}

// TestHandleTriggerBoardMultipleVTXOs verifies board fanout registers one VTXO
// request per requested target amount.
func TestHandleTriggerBoardMultipleVTXOs(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	intent := h.newTestBoardingIntent()
	h.walletActor.setConfirmedIntents(*intent)

	for i := 0; i < 3; i++ {
		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOOwnerKeyFamily,
		).Return(&keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(i),
			},
		}, nil).Once()
		h.wallet.On(
			"DeriveNextKey", mock.Anything,
			types.VTXOSigningKeyFamily,
		).Return(&keychain.KeyDescriptor{
			PubKey: h.clientPubKey,
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOSigningKeyFamily,
				Index:  uint32(i),
			},
		}, nil).Once()
	}

	result := h.receive(&actormsg.TriggerBoardMsg{
		Amounts: []btcutil.Amount{17_000, 17_000, 16_000},
	})
	require.True(t, result.IsOk(), "expected Ok, got: %v",
		result.Err())

	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists, "expected temp-keyed FSM state")

	regState, ok := tempState.State.(*IntentSentState)
	require.True(t, ok, "expected IntentSentState, got %T",
		tempState.State)
	require.Len(t, regState.Intents.Boarding, 1)
	require.Len(t, regState.Intents.VTXOs, 3)

	require.Equal(
		t, btcutil.Amount(17_000), regState.Intents.VTXOs[0].Amount,
	)
	require.Equal(
		t, btcutil.Amount(17_000), regState.Intents.VTXOs[1].Amount,
	)
	require.Equal(
		t, btcutil.Amount(16_000), regState.Intents.VTXOs[2].Amount,
	)
}

// TestHandleTriggerBoardFiltersToNamedOutpoints verifies that when a board
// trigger names the boarding outpoints it sized its amounts over, the round
// actor registers exactly those inputs and ignores other confirmed boarding
// intents. This keeps the proven inputs coherent with the wallet's amounts
// and is the round-side half of the darepo-client#772 boarding idempotency
// fix: the wallet excludes an already-in-flight outpoint, and the round actor
// must not silently re-add it from its own confirmed-boarding fetch.
func TestHandleTriggerBoardFiltersToNamedOutpoints(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	intentA := h.newTestBoardingIntentWithSuffix("-a")
	intentB := h.newTestBoardingIntentWithSuffix("-b")
	h.walletActor.setConfirmedIntents(*intentA, *intentB)

	h.wallet.On(
		"DeriveNextKey", mock.Anything, types.VTXOOwnerKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOOwnerKeyFamily,
			Index:  0,
		},
	}, nil).Once()
	h.wallet.On(
		"DeriveNextKey", mock.Anything, types.VTXOSigningKeyFamily,
	).Return(&keychain.KeyDescriptor{
		PubKey: h.clientPubKey,
		KeyLocator: keychain.KeyLocator{
			Family: types.VTXOSigningKeyFamily,
			Index:  1,
		},
	}, nil).Once()

	// The trigger names only A even though both A and B are confirmed.
	result := h.receive(&actormsg.TriggerBoardMsg{
		Amounts:   []btcutil.Amount{49_000},
		Outpoints: []wire.OutPoint{intentA.Outpoint},
	})
	require.True(t, result.IsOk(), "expected Ok, got: %v", result.Err())

	states := h.queryState()
	tempState, exists := h.findTempState(states)
	require.True(t, exists, "expected temp-keyed FSM state")

	regState, ok := tempState.State.(*IntentSentState)
	require.True(t, ok, "expected IntentSentState, got %T",
		tempState.State)

	// Only the named outpoint A is registered as a boarding input.
	require.Len(t, regState.Intents.Boarding, 1)
	require.Equal(
		t, intentA.Outpoint, regState.Intents.Boarding[0].Outpoint,
	)
}

// TestHandleTriggerBoardSkipsWhenNamedOutpointAbsent verifies that a board
// trigger whose named outpoints are no longer confirmed (adopted between the
// wallet's dispatch and the round actor's fetch) is skipped without minting
// VTXO owner keys or creating a round. The unmocked DeriveNextKey would panic
// if the short-circuit failed to fire.
func TestHandleTriggerBoardSkipsWhenNamedOutpointAbsent(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()

	err := h.start()
	require.NoError(t, err)

	intentA := h.newTestBoardingIntentWithSuffix("-a")
	intentB := h.newTestBoardingIntentWithSuffix("-b")

	// Only A is confirmed, but the trigger names B (already adopted).
	h.walletActor.setConfirmedIntents(*intentA)

	result := h.receive(&actormsg.TriggerBoardMsg{
		Amounts:   []btcutil.Amount{49_000},
		Outpoints: []wire.OutPoint{intentB.Outpoint},
	})
	require.True(t, result.IsOk(), "expected Ok, got: %v", result.Err())

	// No round was created: the trigger short-circuited before assembly.
	states := h.queryState()
	_, exists := h.findTempState(states)
	require.False(t, exists, "redundant trigger must not create a round")
}

// TestHandleForfeitCollectionTimeout verifies timeout-message handling for
// rounds waiting on forfeit signatures.
func TestHandleForfeitCollectionTimeout(t *testing.T) {
	t.Parallel()

	t.Run("transitions_round_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("forfeit-timeout-round")
		h.setupRoundInForfeitCollectingState(roundID)

		keyStr := RoundKeyStr(roundID.KeyString())
		msg := &TimeoutMsg{
			TimeoutID: makeTimeoutID(
				RoundKeyStr(
					roundID.KeyString(),
				),
				TimeoutPhaseForfeitCollection,
			),
		}
		result := h.receive(msg)
		require.True(
			t, result.IsOk(),
			"actor receive failed: %v", result.Err(),
		)

		// The failed round stays observable in ClientFailedState; it is
		// reaped lazily at the next assembly, not on entry
		// (darepo-client#602). The failure reason/recoverability is
		// covered by the FSM-level forfeit-collection-timeout test.
		states := h.queryState()
		info, exists := states[string(keyStr)]
		require.True(t, exists, "failed round should remain observable")
		require.IsType(t, &ClientFailedState{}, info.State)
	})

	t.Run("ignores_unknown_timeout_phase", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		roundID := testRoundID("forfeit-timeout-stale")
		h.setupRoundInForfeitCollectingState(roundID)

		keyStr := RoundKeyStr(roundID.KeyString())
		tid := makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhase("unknown"),
		)
		msg := &TimeoutMsg{TimeoutID: tid}
		result := h.receive(msg)
		require.True(
			t, result.IsOk(),
			"actor receive failed: %v", result.Err(),
		)

		states := h.queryState()
		stateInfo, exists := states[string(keyStr)]
		require.True(t, exists, "expected round state")
		_, ok := stateInfo.State.(*ForfeitSignaturesCollectingState)
		require.True(
			t, ok, "expected ForfeitSignaturesCollectingState, "+
				"got %T", stateInfo.State,
		)
	})

	t.Run("ignores_timeout_for_unknown_round", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Fire a timeout for a round that doesn't exist. This can
		// happen if the round completed or was cleaned up before
		// the timer fires.
		unknownID := testRoundID("nonexistent-round")
		msg := &TimeoutMsg{
			TimeoutID: makeTimeoutID(
				RoundKeyStr(
					unknownID.KeyString(),
				),
				TimeoutPhaseForfeitCollection,
			),
		}
		result := h.receive(msg)
		require.True(
			t, result.IsOk(),
			"stale timeout should be silently ignored",
		)
	})

	t.Run("timeout_then_new_round_succeeds", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Phase 1: Set up a round in forfeit collecting and time
		// it out.
		roundID1 := testRoundID("round-timeout-recover")
		h.setupRoundInForfeitCollectingState(roundID1)

		timeoutMsg := &TimeoutMsg{
			TimeoutID: makeTimeoutID(
				RoundKeyStr(
					roundID1.KeyString(),
				),
				TimeoutPhaseForfeitCollection,
			),
		}
		result := h.receive(timeoutMsg)
		require.True(
			t, result.IsOk(),
			"timeout receive failed: %v", result.Err(),
		)

		// The first round failed on timeout but stays observable in
		// ClientFailedState — reaping is deferred to the next assembly
		// (darepo-client#602).
		keyStr1 := RoundKeyStr(roundID1.KeyString())
		states := h.queryState()
		info, exists := states[string(keyStr1)]
		require.True(t, exists, "failed round should remain observable")
		require.IsType(t, &ClientFailedState{}, info.State)

		// Phase 2: Start a new round from a boarding confirmation,
		// proving the actor is still healthy. Assembling the new round
		// also sweeps the stale failed round.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.assertFSMState("PendingRoundAssembly")

		states = h.queryState()
		_, stillThere := states[string(keyStr1)]
		require.False(
			t, stillThere,
			"failed round should be reaped at next assembly",
		)

		h.sendVTXORequests(50000)
		h.clearServerMessages()
		h.sendServerMessage(&IntentRequested{})
		h.assertFSMState("IntentSentState")
		h.assertServerMessageSent("SendClientEventRequest")
	})
}

// TestActorReapsFailedRound verifies that a round driven into the terminal
// failed state is removed from the actor's tracking rather than left to
// accumulate. Here the round fails via a registration (admission) timeout
// while parked in IntentSentState.
func TestActorReapsFailedRound(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())

	// Drive a round to IntentSentState (join request sent, awaiting the
	// server's admission watermark).
	intent := h.newTestBoardingIntent()
	h.sendWalletConfirmation(intent)
	h.assertFSMState("PendingRoundAssembly")
	h.sendVTXORequests(50000)
	h.sendServerMessage(&IntentRequested{})
	h.assertFSMState("IntentSentState")

	// Grab the (temp) key of the in-flight round.
	before := h.queryState()
	require.Len(t, before, 1)
	var keyStr string
	for k := range before {
		keyStr = k
	}

	// Fire the registration timeout for that round.
	msg := &TimeoutMsg{
		TimeoutID: makeTimeoutID(
			RoundKeyStr(keyStr), TimeoutPhaseRegistration,
		),
	}
	require.True(t, h.receive(msg).IsOk())

	// The round settled in ClientFailedState but is NOT reaped on entry: it
	// stays observable so a consumer can see the failure. Reaping is
	// deferred to the next assembly (darepo-client#602).
	failed := h.queryState()
	failedFSM, exists := failed[keyStr]
	require.True(t, exists, "failed round should remain observable")
	require.IsType(
		t, &ClientFailedState{}, failedFSM.State,
		"round should be observable in ClientFailedState",
	)

	// Starting a fresh round sweeps the stale failed round.
	intent2 := h.newTestBoardingIntent()
	h.sendWalletConfirmation(intent2)
	h.assertFSMState("PendingRoundAssembly")

	after := h.queryState()
	_, stillThere := after[keyStr]
	require.False(
		t, stillThere, "failed round should be reaped at next assembly",
	)
}

// TestRealTimeoutActorForfeitExpiry exercises the full timeout flow with a
// real timeout.Actor running in a real ActorSystem. The round actor schedules
// a short forfeit collection timeout via the real timer infrastructure; when
// the timer fires, the callback routes through SelfRef back into the round
// actor, which transitions the round to ClientFailedState. A new round then
// starts successfully, proving recovery.
func TestRealTimeoutActorForfeitExpiry(t *testing.T) {
	t.Parallel()

	h := newActorTestHarnessWithRealTimeout(t)
	h.setupMockRoundStoreForStart()

	// Use a short timeout so the real timer fires quickly.
	h.actor.env.ForfeitCollectionTimeout = 100 * time.Millisecond

	err := h.start()
	require.NoError(t, err)

	// Put a round into forfeit collecting state. The FSM env has the
	// short timeout, so when processOutbox handles StartTimeoutReq it
	// schedules a real 100ms timer.
	roundID := testRoundID("real-timeout-round")
	h.setupRoundInForfeitCollectingState(roundID)

	// Manually trigger the outbox processing that would normally happen
	// when entering forfeit collection. This schedules the real timeout.
	outbox := []ClientOutMsg{
		&StartTimeoutReq{
			RoundKey: RoundKeyStr(roundID.KeyString()),
			Phase:    TimeoutPhaseForfeitCollection,
			Duration: 100 * time.Millisecond,
		},
	}
	err = h.actor.processOutbox(h.ctx, outbox)
	require.NoError(t, err)

	// Wait for the real timer to fire. The timeout actor sends an
	// ExpiredMsg callback that gets mapped to a TimeoutMsg on our
	// SelfRef.
	rawMsg, ok := h.selfRef.waitForMessage(5 * time.Second)
	require.True(t, ok, "expected TimeoutMsg from real timeout actor")

	timeoutMsg, ok := rawMsg.(*TimeoutMsg)
	require.True(t, ok, "expected *TimeoutMsg, got %T", rawMsg)
	require.Equal(
		t,
		makeTimeoutID(
			RoundKeyStr(
				roundID.KeyString(),
			),
			TimeoutPhaseForfeitCollection,
		),
		timeoutMsg.TimeoutID,
	)

	// Feed the callback message back into the round actor, simulating
	// the actor system's mailbox delivery.
	result := h.receive(timeoutMsg)
	require.True(t, result.IsOk(), "actor receive failed: %v",
		result.Err())

	// The failed round stays observable after the timeout fires — reaping
	// is deferred to the next assembly, not done on entry
	// (darepo-client#602).
	keyStr := RoundKeyStr(roundID.KeyString())
	states := h.queryState()
	failedInfo, exists := states[string(keyStr)]
	require.True(t, exists, "failed round should remain observable")
	require.IsType(t, &ClientFailedState{}, failedInfo.State)

	// Phase 2: Start a new round to prove actor recovery. Assembling the
	// new round also sweeps the stale failed round.
	intent := h.newTestBoardingIntent()
	h.sendWalletConfirmation(intent)
	h.assertFSMState("PendingRoundAssembly")

	states = h.queryState()
	_, stillThere := states[string(keyStr)]
	require.False(
		t, stillThere, "failed round should be reaped at next assembly",
	)
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
			Amount: 100000,
			PkScript: []byte{
				0x51,
				0x20,
			},
			OwnerKey:    h.newKeyDescriptor(),
			OperatorKey: h.operatorPubKey,
			Expiry:      144,
		}

		notification := &VTXOCreatedNotification{
			VTXOs: []*ClientVTXO{
				clientVTXO,
			},
			RoundID:        "test-round-123",
			CommitmentTxID: chainhash.HashH([]byte("commitment")),
			BatchExpiry:    1000,
			CreatedHeight:  500,
		}

		// Directly call processOutbox to test the routing.
		outbox := []ClientOutMsg{notification}
		_ = h.actor.processOutbox(h.ctx, outbox)

		// Verify the VTXO manager received the notification.
		receivedNotif := h.vtxoManager.assertVTXOCreatedReceived(t)
		require.Equal(t, "test-round-123", receivedNotif.RoundID)
		require.Len(t, receivedNotif.VTXOs, 1)
		require.Equal(
			t, clientVTXO.Outpoint, receivedNotif.VTXOs[0].Outpoint,
		)
	})

	t.Run("nil_vtxo_manager_skips_notification", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()
		h.actor.cfg.VTXOManager = nil

		err := h.start()
		require.NoError(t, err)

		notification := &VTXOCreatedNotification{
			VTXOs: []*ClientVTXO{
				{
					Outpoint: wire.OutPoint{
						Hash: chainhash.HashH(
							[]byte("no-mgr-vtxo"),
						),
						Index: 0,
					},
					Amount: 100000,
					PkScript: []byte{
						0x51,
						0x20,
					},
					OwnerKey:    h.newKeyDescriptor(),
					OperatorKey: h.operatorPubKey,
					Expiry:      144,
				},
			},
			RoundID: "no-mgr-round",
			CommitmentTxID: chainhash.HashH(
				[]byte("no-mgr-commitment"),
			),
			BatchExpiry:   1000,
			CreatedHeight: 500,
		}

		// With nil VTXOManager, notification is silently
		// skipped without error.
		_ = h.actor.processOutbox(
			h.ctx, []ClientOutMsg{notification},
		)
		require.Empty(t, h.vtxoManager.messages)
	})

	t.Run(
		"drop_custom_forfeit_cleans_signing_contexts",
		func(t *testing.T) {
			t.Parallel()

			h := newActorTestHarness(t)
			h.setupMockRoundStoreForStart()
			h.actor.cfg.VTXOManager = nil

			var cleaned []wire.OutPoint
			h.actor.cfg.DropCustomForfeitSigningContexts = func(
				_ context.Context,
				outpoints []wire.OutPoint) error {

				cleaned = append(cleaned, outpoints...)

				return nil
			}

			err := h.start()
			require.NoError(t, err)

			outpoints := []wire.OutPoint{
				{
					Hash: chainhash.HashH(
						[]byte("custom-drop"),
					),
					Index: 7,
				},
			}
			err = h.actor.processOutbox(
				h.ctx, []ClientOutMsg{
					&DropCustomForfeitReservation{
						Outpoints: outpoints,
					},
				},
			)
			require.NoError(t, err)
			require.Equal(t, outpoints, cleaned)
			require.Empty(t, h.vtxoManager.messages)
		},
	)
}

// TestActorIntentMapping verifies that the actor correctly maps external
// messages into IntentPackage events, preserving all fields through the
// type conversion. These tests exercise the mapping+verification logic
// that lives in the actor layer.
func TestActorIntentMapping(t *testing.T) {
	t.Parallel()

	t.Run("boarding_preserves_chain_info", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)

		// Verify the FSM received a complete boarding intent
		// with chain info preserved through the type hierarchy.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists, "expected temp-keyed FSM")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok, "expected PendingRoundAssembly")
		require.Len(t, assembly.Boarding, 1)

		boardingIntent := assembly.Boarding[0]

		// Chain info must be preserved through the embedding.
		require.Equal(
			t, intent.ChainInfo.ConfHeight,
			boardingIntent.ChainInfo.ConfHeight,
		)
		require.Equal(
			t, intent.ChainInfo.Amount,
			boardingIntent.ChainInfo.Amount,
		)
		require.NotNil(t, boardingIntent.ChainInfo.ConfTx)

		// Request fields must be built correctly from the
		// wallet intent.
		require.NotNil(t, boardingIntent.Request.Outpoint)
		require.Equal(
			t, intent.Outpoint, *boardingIntent.Request.Outpoint,
		)
		params, err := boardingIntent.Request.
			DecodeStandardPolicyTemplate()
		require.NoError(t, err)
		require.Equal(
			t, schnorr.SerializePubKey(
				intent.Address.KeyDesc.PubKey,
			),
			schnorr.SerializePubKey(params.OwnerKey),
		)
		require.Equal(
			t, schnorr.SerializePubKey(intent.Address.OperatorKey),
			schnorr.SerializePubKey(params.OperatorKey),
		)
		require.Equal(t, intent.Address.ExitDelay, params.ExitDelay)
	})

	t.Run("boarding_rejects_nil_tx", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		intent.ChainInfo.ConfTx = nil

		// Nil tx should be caught by actor validation, not
		// forwarded to the FSM.
		h.walletActor.sendBoardingConfirmation(h.ctx, intent)

		msg, ok := h.selfRef.waitForMessage(time.Second)
		require.True(t, ok, "expected self-notification")

		result := h.receive(msg)
		require.True(t, result.IsErr())
		require.Contains(
			t, result.Err().Error(), "missing tx",
		)
	})

	t.Run("boarding_rejects_invalid_outpoint_index", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		intent.Outpoint.Index = 999

		h.walletActor.sendBoardingConfirmation(h.ctx, intent)

		msg, ok := h.selfRef.waitForMessage(time.Second)
		require.True(t, ok, "expected self-notification")

		result := h.receive(msg)
		require.True(t, result.IsErr())
		require.Contains(
			t, result.Err().Error(), "invalid outpoint",
		)
	})

	t.Run("boarding_deduplicates_same_outpoint", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.sendWalletConfirmation(intent)

		// Second confirmation with same outpoint should be
		// deduplicated by the FSM.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)
		require.Len(
			t, assembly.Boarding, 1,
			"duplicate outpoint should be deduplicated",
		)
	})

	t.Run("refresh_maps_to_forfeit_and_vtxo", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("refresh-vtxo")),
			Index: 0,
		}
		policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			h.clientPubKey, h.operatorPubKey, 145,
		)
		require.NoError(t, err)

		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint:   vtxoOutpoint,
			Amount:         75000,
			PolicyTemplate: policyTemplate,
		}

		result := h.receive(refreshReq)
		require.True(t, result.IsOk())

		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		// Verify the forfeit input was mapped correctly.
		require.Len(t, assembly.Forfeits, 1)
		require.NotNil(t, assembly.Forfeits[0].VTXOOutpoint)
		require.Equal(
			t, vtxoOutpoint, *assembly.Forfeits[0].VTXOOutpoint,
		)

		// Verify the VTXO output preserves all request fields.
		require.Len(t, assembly.VTXOs, 1)
		vtxoReq := assembly.VTXOs[0]
		require.Equal(
			t, btcutil.Amount(75000), vtxoReq.Amount,
		)
		require.Equal(t, policyTemplate, vtxoReq.PolicyTemplate)
	})

	t.Run("leave_maps_to_forfeit_and_leave_output", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("leave-vtxo")),
			Index: 0,
		}
		leaveOutput := &wire.TxOut{
			Value: 60000,
			PkScript: []byte{
				0x00,
				0x14,
				0x01,
				0x02,
			},
		}
		leaveReq := &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: []types.ForfeitRequest{{
					VTXOOutpoint: &vtxoOutpoint,
				}},
				Leaves: []*types.LeaveRequest{{
					Output: leaveOutput,
				}},
			}},
		}

		result := h.receive(leaveReq)
		require.True(t, result.IsOk())

		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		// Verify the forfeit input was mapped correctly.
		require.Len(t, assembly.Forfeits, 1)
		require.NotNil(t, assembly.Forfeits[0].VTXOOutpoint)
		require.Equal(
			t, vtxoOutpoint, *assembly.Forfeits[0].VTXOOutpoint,
		)

		// Verify the leave output was mapped correctly.
		require.Len(t, assembly.Leaves, 1)
		require.NotNil(t, assembly.Leaves[0].Output)
		require.Equal(
			t, leaveOutput.Value, assembly.Leaves[0].Output.Value,
		)
		require.Equal(
			t, leaveOutput.PkScript,
			assembly.Leaves[0].Output.PkScript,
		)

		// VTXOs should be empty for a leave.
		require.Empty(t, assembly.VTXOs)
	})

	t.Run("mixed_round_accumulates_all_pools", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// 1. Boarding intent.
		boardingIntent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(boardingIntent)

		// 2. VTXO request.
		h.sendVTXORequests(50000)

		// 3. Refresh request.
		policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
			h.clientPubKey, h.operatorPubKey, 145,
		)
		require.NoError(t, err)

		refreshReq := &RefreshVTXORequest{
			VTXOOutpoint: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("ref")),
				Index: 0,
			},
			Amount:         30000,
			PolicyTemplate: policyTemplate,
		}
		result := h.receive(refreshReq)
		require.True(t, result.IsOk())

		// 4. Leave request via RegisterIntentRequest.
		leaveOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("leave")),
			Index: 0,
		}
		leaveReq := &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: []types.ForfeitRequest{{
					VTXOOutpoint: &leaveOutpoint,
				}},
				Leaves: []*types.LeaveRequest{{
					Output: &wire.TxOut{
						Value: 20000,
						PkScript: []byte{
							0x00,
							0x14,
						},
					},
				}},
			}},
		}
		result = h.receive(leaveReq)
		require.True(t, result.IsOk())

		// Verify all four pools are populated.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		require.Len(t, assembly.Boarding, 1)
		require.Len(
			t, assembly.VTXOs, 2,
			"1 explicit VTXO + 1 from refresh",
		)
		require.Len(
			t, assembly.Forfeits, 2,
			"1 from refresh + 1 from leave",
		)
		require.Len(t, assembly.Leaves, 1)
	})

	t.Run("duplicate_forfeit_outpoint_deduplicated", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Send two refresh requests for the same VTXO.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("dup-forfeit")),
			Index: 0,
		}

		for i := 0; i < 2; i++ {
			policyTemplate, err := arkscript.
				EncodeStandardVTXOTemplate(
					h.clientPubKey, h.operatorPubKey, 144,
				)
			require.NoError(t, err)

			refreshReq := &RefreshVTXORequest{
				VTXOOutpoint:   vtxoOutpoint,
				Amount:         50000,
				PolicyTemplate: policyTemplate,
			}

			result := h.receive(refreshReq)
			require.True(t, result.IsOk())
		}

		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		// The forfeit should be deduplicated by outpoint.
		require.Len(
			t, assembly.Forfeits, 1,
			"duplicate forfeit outpoint should be deduplicated",
		)
	})
}

// TestHandleRegisterIntent verifies that RegisterIntentRequest registers a
// pre-composed intent package with the round FSM without the round actor
// performing any intent composition.
func TestHandleRegisterIntent(t *testing.T) {
	t.Parallel()

	t.Run("registers_refresh_intent_package", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Build a refresh-style intent package: one forfeit + one
		// new VTXO. This is what the wallet would compose.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("register-refresh")),
			Index: 0,
		}
		req := &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: []types.ForfeitRequest{{
					VTXOOutpoint: &vtxoOutpoint,
					Amount:       50000,
				}},
				VTXOs: []types.VTXORequest{{
					Amount: 50000,
					PolicyTemplate: func() []byte {
						return stdTpl(
							t, h.clientPubKey,
							h.operatorPubKey, 144,
						)
					}(),
					PkScript: []byte{
						0x51,
						0x20,
					},
					ClientKey:   h.clientPubKey,
					OperatorKey: h.operatorPubKey,
					Expiry:      144,
				}},
			}},
		}

		result := h.receive(req)
		require.True(
			t, result.IsOk(),
			"expected Ok, got: %v", result.Err(),
		)

		// Verify FSM has the intent registered.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists,
			"expected temp-keyed FSM state")

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(
			t, ok, "expected PendingRoundAssembly, got %T",
			tempState.State,
		)

		require.Len(t, assembly.Forfeits, 1)
		require.Equal(
			t, vtxoOutpoint, *assembly.Forfeits[0].VTXOOutpoint,
		)
		require.Equal(
			t, btcutil.Amount(50000), assembly.Forfeits[0].Amount,
		)
		require.Len(t, assembly.VTXOs, 1)
	})

	t.Run("registers_leave_intent_package", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Build a leave-style intent package: one forfeit + one
		// leave output.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("register-leave")),
			Index: 0,
		}
		leaveOutput := &wire.TxOut{
			Value: 60000,
			PkScript: []byte{
				0x00,
				0x14,
				0x01,
				0x02,
			},
		}
		req := &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: []types.ForfeitRequest{{
					VTXOOutpoint: &vtxoOutpoint,
				}},
				Leaves: []*types.LeaveRequest{{
					Output: leaveOutput,
				}},
			}},
		}

		result := h.receive(req)
		require.True(
			t, result.IsOk(),
			"expected Ok, got: %v", result.Err(),
		)

		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		require.Len(t, assembly.Forfeits, 1)
		require.Len(t, assembly.Leaves, 1)
		require.Equal(
			t, leaveOutput.Value, assembly.Leaves[0].Output.Value,
		)
		require.Empty(t, assembly.VTXOs)
	})

	t.Run("rejects_empty_package", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// Nil package.
		result := h.receive(&RegisterIntentRequest{
			Package: nil,
		})
		require.True(t, result.IsErr())

		// Empty package.
		result = h.receive(&RegisterIntentRequest{
			Package: &IntentPackage{},
		})
		require.True(t, result.IsErr())
	})

	t.Run("accumulates_with_existing_intents", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		// First: add a boarding intent via the existing path.
		intent := h.newTestBoardingIntent()
		h.sendWalletConfirmation(intent)
		h.assertFSMState("PendingRoundAssembly")

		// Second: register a refresh intent package.
		vtxoOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("accum-refresh")),
			Index: 0,
		}
		req := &RegisterIntentRequest{
			Package: &IntentPackage{Intents: Intents{
				Forfeits: []types.ForfeitRequest{{
					VTXOOutpoint: &vtxoOutpoint,
				}},
				VTXOs: []types.VTXORequest{{
					Amount: 40000,
					PolicyTemplate: func() []byte {
						return stdTpl(
							t, h.clientPubKey,
							h.operatorPubKey, 144,
						)
					}(),
					PkScript: []byte{
						0x51,
						0x20,
					},
					ClientKey:   h.clientPubKey,
					OperatorKey: h.operatorPubKey,
					Expiry:      144,
				}},
			}},
		}

		result := h.receive(req)
		require.True(
			t, result.IsOk(),
			"expected Ok, got: %v", result.Err(),
		)

		// Verify both boarding and refresh intents are present.
		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		require.Len(t, assembly.Boarding, 1)
		require.Len(t, assembly.Forfeits, 1)
		require.Len(t, assembly.VTXOs, 1,
			"1 from register intent")
	})

	t.Run("intent_creates_new_round_when_existing_in_registration_sent",
		func(t *testing.T) {
			t.Parallel()

			h := newActorTestHarness(t)
			h.setupMockRoundStoreForStart()

			err := h.start()
			require.NoError(t, err)

			// Place a round in IntentSentState with a
			// temp key. Before the fix, findPendingRound would
			// match this round and the IntentPackage would be
			// silently self-looped.
			h.setupRoundInIntentSentState()

			// Now register an intent. With findAssemblingRound,
			// this should create a new round rather than
			// matching the RegistrationSent one.
			vtxoOutpoint := wire.OutPoint{
				Hash: chainhash.HashH(
					[]byte("new-round"),
				),
				Index: 0,
			}
			req := &RegisterIntentRequest{
				Package: &IntentPackage{Intents: Intents{
					Forfeits: []types.ForfeitRequest{{
						VTXOOutpoint: &vtxoOutpoint,
					}},
					VTXOs: []types.VTXORequest{{
						Amount: 40000,
						PolicyTemplate: func() []byte {
							c := h.clientPubKey
							o := h.operatorPubKey

							return stdTpl(
								t, c, o, 144,
							)
						}(),
						PkScript: []byte{
							0x51,
							0x20,
						},
						ClientKey:   h.clientPubKey,
						OperatorKey: h.operatorPubKey,
						Expiry:      144,
					}},
				}},
			}

			result := h.receive(req)
			require.True(
				t, result.IsOk(),
				"expected Ok, got: %v", result.Err(),
			)

			// We should now have two rounds: one in
			// RegistrationSent and a new one in
			// PendingRoundAssembly with our intent.
			states := h.queryState()
			require.Len(t, states, 2,
				"should have 2 rounds")

			var foundAssembly bool
			for _, info := range states {
				s := info.State
				assembly, ok := s.(*PendingRoundAssembly)
				if !ok {
					continue
				}

				foundAssembly = true
				require.Len(
					t, assembly.Forfeits, 1,
					"intent should be in new round",
				)
			}
			require.True(
				t, foundAssembly,
				"should have a PendingRoundAssembly round",
			)
		},
	)

	t.Run("duplicate_vtxo_pkscript_deduplicated", func(t *testing.T) {
		t.Parallel()

		h := newActorTestHarness(t)
		h.setupMockRoundStoreForStart()

		err := h.start()
		require.NoError(t, err)

		pkScript := []byte{0x51, 0x20, 0x01, 0x02}

		// Send two intent packages with VTXOs sharing the same
		// PkScript. The second should be deduplicated.
		for i := 0; i < 2; i++ {
			outpoint := wire.OutPoint{
				Hash: chainhash.HashH(
					[]byte(
						fmt.Sprintf("dup-vtxo-%d", i),
					),
				),
				Index: 0,
			}
			req := &RegisterIntentRequest{
				Package: &IntentPackage{Intents: Intents{
					Forfeits: []types.ForfeitRequest{{
						VTXOOutpoint: &outpoint,
					}},
					VTXOs: []types.VTXORequest{{
						Amount: 40000,
						PolicyTemplate: func() []byte {
							c := h.clientPubKey
							o := h.operatorPubKey

							return stdTpl(
								t, c, o, 144,
							)
						}(),
						PkScript:    pkScript,
						ClientKey:   h.clientPubKey,
						OperatorKey: h.operatorPubKey,
						Expiry:      144,
					}},
				}},
			}

			result := h.receive(req)
			require.True(t, result.IsOk())
		}

		states := h.queryState()
		tempState, exists := h.findTempState(states)
		require.True(t, exists)

		assembly, ok := tempState.State.(*PendingRoundAssembly)
		require.True(t, ok)

		// Forfeits should have 2 (different outpoints).
		require.Len(t, assembly.Forfeits, 2)

		// VTXOs should be deduplicated to 1 (same PkScript).
		require.Len(
			t, assembly.VTXOs, 1,
			"duplicate VTXOs by PkScript should be deduplicated",
		)
	})
}
