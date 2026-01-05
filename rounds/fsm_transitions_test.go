package rounds

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestFSMCreatedState tests the FSM transitions from the CreatedState state.
func TestFSMCreatedState(t *testing.T) {
	t.Parallel()

	t.Run("join request validation failure", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("bad")),
			Index: 0,
		}

		// Set up the boarding locker mock to simulate that the input is
		// already locked. This will cause validation to fail.
		otherRoundID, err := NewRoundID()
		require.NoError(t, err)
		h.lockBoardingInput(&outpoint, otherRoundID)

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Now, send a ClientJoinRequestEvent. Since we simulated a
		// failure in validation, we expect to remain in CreatedState
		// and receive a client error outbox message.
		joinReqEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{
						Outpoint: &outpoint,
					},
				},
			},
		}
		err = h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we are still in CreatedState.
		assertStateType[*CreatedState](h)

		// Assert that we have the expected outbox message.
		h.assertOutboxLen(1)
		msg := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Contains(t, msg.ErrorMsg, ErrJoinRequestInvalid.Error())
	})

	t.Run("successful join request", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("good")),
			Index: 0,
		}

		// Set up mocks to allow the boarding input to pass validation.
		h.setupValidBoardingInput(
			&outpoint, client.boardingKey, exitDelay, 10,
		)

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent with valid parameters.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we transitioned to RegistrationState.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)

		// Assert that we have the expected outbox messages:
		// 1. ClientSuccessResp to the client
		// 2. StartTimeoutReq to schedule the timeout
		h.assertOutboxLen(2)

		// Check success response to client.
		successResp := assertOutboxMessageType[*ClientSuccessResp](h, 0)
		require.Equal(t, "client1", string(successResp.Client))
		require.Equal(t, h.env.RoundID, successResp.RoundID)

		// Check timeout request with the round ID and phase.
		timeoutReq := assertOutboxMessageType[*StartTimeoutReq](h, 1)
		require.Equal(t, h.env.RoundID, timeoutReq.RoundID)
		require.Equal(t, TimeoutPhaseRegistration, timeoutReq.Phase)
	})
}

// TestFSMRegistrationState tests the FSM transitions from RegistrationState.
func TestFSMRegistrationState(t *testing.T) {
	t.Parallel()

	t.Run("second client joins successfully", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client1 := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)
		client2 := newClientHarness(
			t, "client2", 20, h.operatorPub, exitDelay, expiry,
		)

		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Set up mocks for both clients' boarding inputs.
		h.setupValidBoardingInput(
			&outpoint1, client1.boardingKey, exitDelay, 10,
		)
		h.setupValidBoardingInput(
			&outpoint2, client2.boardingKey, exitDelay, 10,
		)

		// First client joins from CreatedState.
		boardingReq1 := client1.createBoardingRequest(&outpoint1)
		joinReqEvent1 := client1.createJoinRequest(
			[]*types.BoardingRequest{boardingReq1},
		)
		err := h.sendEvent(joinReqEvent1)
		require.NoError(t, err)

		// Assert we transitioned to RegistrationState.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)
		require.True(t, regState.isClientRegistered("client1"))

		// Clear outbox for next test.
		h.outboxMessages = nil

		// Second client joins from RegistrationState.
		boardingReq2 := client2.createBoardingRequest(&outpoint2)
		joinReqEvent2 := client2.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		)
		err = h.sendEvent(joinReqEvent2)
		require.NoError(t, err)

		// Assert we remain in RegistrationState with both clients.
		regState = assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 2)
		require.True(t, regState.isClientRegistered("client1"))
		require.True(t, regState.isClientRegistered("client2"))

		// Assert outbox messages for second client (no timeout for
		// subsequent joins).
		h.assertOutboxLen(1)

		successResp := assertOutboxMessageType[*ClientSuccessResp](h, 0)
		require.Equal(t, "client2", string(successResp.Client))
	})

	t.Run("duplicate client rejected", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Set up mocks. Only outpoint1 is needed since outpoint2
		// won't be validated when a duplicate client is rejected.
		h.setupValidBoardingInput(
			&outpoint1, client.boardingKey, exitDelay, 10,
		)

		// First client joins.
		boardingReq1 := client.createBoardingRequest(&outpoint1)
		joinReqEvent1 := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq1},
		)
		err := h.sendEvent(joinReqEvent1)
		require.NoError(t, err)

		// Assert we transitioned to RegistrationState.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)

		// Clear outbox for next test.
		h.outboxMessages = nil

		// Same client attempts to join again with different inputs.
		boardingReq2 := client.createBoardingRequest(&outpoint2)
		joinReqEvent2 := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		)
		err = h.sendEvent(joinReqEvent2)
		require.NoError(t, err)

		// Assert we remain in RegistrationState with only client1 and
		// original inputs.
		regState = assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)
		require.True(t, regState.isClientRegistered("client1"))

		// Assert we received an error response.
		h.assertOutboxLen(1)

		errorResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, "client1", string(errorResp.Client))
		require.Contains(t, errorResp.ErrorMsg, "already registered")
	})

	t.Run("lock failure rejects client but allows others",
		func(t *testing.T) {
			t.Parallel()

			// Set up the test harness.
			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client1 := newClientHarness(
				t, "client1", 10, h.operatorPub, exitDelay,
				expiry,
			)
			client2 := newClientHarness(
				t, "client2", 20, h.operatorPub, exitDelay,
				expiry,
			)
			client3 := newClientHarness(
				t, "client3", 30, h.operatorPub, exitDelay,
				expiry,
			)

			outpoint1 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input1")),
				Index: 0,
			}
			outpoint2 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input2")),
				Index: 0,
			}
			outpoint3 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input3")),
				Index: 0,
			}

			// Set up valid boarding inputs for all three clients,
			// but client2's lock will fail.
			h.setupValidBoardingInput(
				&outpoint1, client1.boardingKey, exitDelay, 10,
			)

			// For client2, set up validation to succeed but lock to
			// fail.
			h.allowBoardingInput(&outpoint2)
			h.mockBoardingUTXO(
				outpoint2, client2.boardingKey, exitDelay, 10,
			)
			h.boardingLocker.On("Lock", mock.Anything, &outpoint2,
				h.env.RoundID).
				Return(fmt.Errorf("lock failed")).Once()

			h.setupValidBoardingInput(
				&outpoint3, client3.boardingKey, exitDelay, 10,
			)

			// First client joins successfully from CreatedState.
			boardingReq1 := client1.createBoardingRequest(
				&outpoint1,
			)
			joinReqEvent1 := client1.createJoinRequest(
				[]*types.BoardingRequest{boardingReq1},
			)
			err := h.sendEvent(joinReqEvent1)
			require.NoError(t, err)

			// Assert we transitioned to RegistrationState with
			// client1.
			regState := assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 1)
			require.True(t, regState.isClientRegistered("client1"))

			// Clear outbox for next test.
			h.outboxMessages = nil

			// Second client attempts to join but lock fails.
			boardingReq2 := client2.createBoardingRequest(
				&outpoint2,
			)
			joinReqEvent2 := client2.createJoinRequest(
				[]*types.BoardingRequest{boardingReq2},
			)
			err = h.sendEvent(joinReqEvent2)
			require.NoError(t, err)

			// Assert we remain in RegistrationState with only
			// client1.
			regState = assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 1)
			require.True(t, regState.isClientRegistered("client1"))
			require.False(t, regState.isClientRegistered("client2"))

			// Assert client2 received an error response.
			h.assertOutboxLen(1)
			errorResp := assertOutboxMessageType[*ClientErrorResp](
				h, 0,
			)
			require.Equal(t, "client2", string(errorResp.Client))
			require.Contains(
				t, errorResp.ErrorMsg, "failed to lock",
			)

			// Clear outbox for next test.
			h.outboxMessages = nil

			// Third client joins successfully, proving the FSM is
			// still functional after the lock failure.
			boardingReq3 := client3.createBoardingRequest(
				&outpoint3,
			)
			joinReqEvent3 := client3.createJoinRequest(
				[]*types.BoardingRequest{boardingReq3},
			)
			err = h.sendEvent(joinReqEvent3)
			require.NoError(t, err)

			// Assert we remain in RegistrationState with client1
			// and client3 (client2 was rejected).
			regState = assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 2)
			require.True(t, regState.isClientRegistered("client1"))
			require.False(t, regState.isClientRegistered("client2"))
			require.True(t, regState.isClientRegistered("client3"))

			// Assert client3 received a success response.
			h.assertOutboxLen(1)

			//nolint:ll
			successResp := assertOutboxMessageType[*ClientSuccessResp](h, 0)
			require.Equal(t, "client3", string(successResp.Client))
		},
	)

	t.Run("registration timeout triggers seal", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		h.setupValidBoardingInput(
			&outpoint, client.boardingKey, exitDelay, 10,
		)

		// Join to get to RegistrationState.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we're in RegistrationState.
		assertStateType[*RegistrationState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Send RegistrationTimeoutEvent.
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// The timeout should emit SealEvent (internal) which causes
		// transition to BatchBuildingState, then BuildBatchTxEvent
		// (internal) builds the PSBT and transitions to
		// BatchBuiltState.
		assertStateType[*BatchBuiltState](h)

		// Verify RoundSealedReq was emitted.
		assertOutboxContains[*RoundSealedReq](h)
	})
}

// TestFSMBatchBuilding tests the batch building states and transitions.
func TestFSMBatchBuilding(t *testing.T) {
	t.Parallel()

	const (
		exitDelay = 144
		expiry    = 144
	)

	t.Run("multi-client batch building", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		client1 := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)
		client2 := newClientHarness(
			t, "client2", 20, h.operatorPub, exitDelay, expiry,
		)

		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Allow both inputs.
		h.allowBoardingInput(&outpoint1)
		h.allowBoardingInput(&outpoint2)

		h.mockBoardingUTXO(
			outpoint1, client1.boardingKey, exitDelay, 10,
		)
		h.mockBoardingUTXO(
			outpoint2, client2.boardingKey, exitDelay, 10,
		)

		// First client joins.
		boardingReq1 := client1.createBoardingRequest(&outpoint1)
		joinReqEvent1 := client1.createJoinRequest(
			[]*types.BoardingRequest{boardingReq1},
		)
		err := h.sendEvent(joinReqEvent1)
		require.NoError(t, err)

		// Second client joins.
		h.outboxMessages = nil
		boardingReq2 := client2.createBoardingRequest(&outpoint2)
		joinReqEvent2 := client2.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		)
		err = h.sendEvent(joinReqEvent2)
		require.NoError(t, err)

		// Assert both clients registered.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.ClientRegistrations, 2)

		// Seal via manual SealEvent.
		h.outboxMessages = nil
		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Should transition to BatchBuiltState after internal events.
		builtState := assertStateType[*BatchBuiltState](h)

		// Verify both clients are in the batch.
		require.Len(t, builtState.ClientRegistrations, 2)
		require.Contains(
			t, builtState.ClientRegistrations, ClientID("client1"),
		)
		require.Contains(
			t, builtState.ClientRegistrations, ClientID("client2"),
		)

		// Verify both clients get batch info.
		batchInfoCount := 0
		for _, msg := range h.outboxMessages {
			if info, ok := msg.(*ClientBatchInfo); ok {
				batchInfoCount++
				require.NotNil(t, info.BatchPSBT)
				require.NotNil(t, info.BatchPSBT.UnsignedTx)
			}
		}
		require.Equal(t, 2, batchInfoCount, "both clients get batch")

		h.assertMockExpectations()
	})

	t.Run("stale timeout ignored in BatchBuiltState", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		h.allowBoardingInput(&outpoint)
		h.mockBoardingUTXO(outpoint, client.boardingKey, exitDelay, 10)

		// Join to get to RegistrationState.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Seal via RegistrationTimeoutEvent.
		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should be in BatchBuiltState.
		builtState := assertStateType[*BatchBuiltState](h)
		require.NotNil(t, builtState.PSBT)

		// Clear outbox and send another stale RegistrationTimeoutEvent.
		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in BatchBuiltState with no outbox messages.
		assertStateType[*BatchBuiltState](h)
		h.assertOutboxLen(0)
	})
}

// TestFSMFailureScenarios tests the FSM failure handling and transitions to
// FailedState.
func TestFSMFailureScenarios(t *testing.T) {
	t.Parallel()

	t.Run("batch building failure goes to FailedState", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		h.allowBoardingInput(&outpoint)
		h.mockBoardingUTXO(outpoint, client.boardingKey, exitDelay, 10)

		// Set up explicit mocks for this test.
		h.boardingLocker.On("Lock", mock.Anything, &outpoint,
			h.env.RoundID).Return(nil).Once()
		h.feeEstimator.On("EstimateFeePerKW", uint32(6)).
			Return(chainfee.SatPerKWeight(1000), nil).Once()

		// Configure wallet to fail on funding.
		h.walletController.On("FundPsbt", mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything).
			Return(int32(0), fmt.Errorf("insufficient funds")).
			Once()

		// Join to get to RegistrationState.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we're in RegistrationState.
		assertStateType[*RegistrationState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Seal the round - this should trigger batch building which
		// will fail due to wallet funding error.
		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "insufficient funds")

		// Verify outbox messages:
		// 1. ClientRoundFailedResp for client1
		// 2. UnlockBoardingInputsReq for the outpoint
		// 3. RoundFailedReq for the actor
		var (
			foundClientFailed bool
			foundUnlock       bool
			foundRoundFailed  bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.Equal(t, h.env.RoundID, m.RoundID)
				require.Contains(t, m.Reason, "insufficient "+
					"funds")

			case *UnlockBoardingInputsReq:
				foundUnlock = true
				require.Equal(t, h.env.RoundID, m.RoundID)
				require.Len(t, m.Outpoints, 1)
				require.Equal(t, &outpoint, m.Outpoints[0])

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
				require.Contains(t, m.Reason, "insufficient "+
					"funds")
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundUnlock, "inputs should be unlocked")
		require.True(t, foundRoundFailed, "actor should be notified")
	})

	t.Run("FailedState is terminal and ignores events", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness and drive it to FailedState by
		// causing a batch building failure.
		h := newTestHarness(t)

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
		h.allowBoardingInput(&outpoint)
		h.mockBoardingUTXO(outpoint, client.boardingKey, exitDelay, 10)

		// Set up explicit mocks for this test.
		h.boardingLocker.On("Lock", mock.Anything, &outpoint,
			h.env.RoundID).Return(nil).Once()
		h.feeEstimator.On("EstimateFeePerKW", uint32(6)).
			Return(chainfee.SatPerKWeight(1000), nil).Once()

		// Configure wallet to fail on funding.
		h.walletController.On("FundPsbt", mock.Anything, mock.Anything,
			mock.Anything, mock.Anything, mock.Anything).
			Return(int32(0), fmt.Errorf("insufficient funds")).
			Once()

		// Join client.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinEvent)
		require.NoError(t, err)

		// Trigger seal which will cause batch building to fail.
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should now be in FailedState.
		assertStateType[*FailedState](h)

		// Clear outbox messages from the failure.
		h.outboxMessages = nil

		// Try to send various events - all should be ignored.
		err = h.sendEvent(&ClientJoinRequestEvent{
			ClientID: ClientID("client2"),
			Request:  joinEvent.Request,
		})
		require.NoError(t, err)
		assertStateType[*FailedState](h)
		h.assertOutboxLen(0)

		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)
		assertStateType[*FailedState](h)
		h.assertOutboxLen(0)

		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)
		assertStateType[*FailedState](h)
		h.assertOutboxLen(0)
	})
}
