package rounds

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/routing/route"
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
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
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

	t.Run("forfeit VTXO locked during registration", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		// Create a boarding input outpoint.
		boardingOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding")),
			Index: 0,
		}

		// Create a forfeit VTXO outpoint.
		forfeitOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("forfeit")),
			Index: 0,
		}

		// Set up mocks to allow boarding input and forfeit VTXO to pass
		// validation.
		h.setupValidBoardingInput(
			&boardingOutpoint, client.boardingKey, exitDelay, 10,
			h.roundID,
		)
		h.setupValidForfeitVTXO(
			&forfeitOutpoint, client.boardingKey, h.roundID,
		)
		h.expectVTXOLocked(h.roundID, forfeitOutpoint)

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent with both boarding and forfeit.
		boardingReq := client.createBoardingRequest(&boardingOutpoint)
		forfeitReq := &types.ForfeitRequest{
			VTXOOutpoint: &forfeitOutpoint,
		}
		joinReqEvent := client.createJoinRequestWithForfeits(
			[]*types.BoardingRequest{boardingReq},
			[]*types.ForfeitRequest{forfeitReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we transitioned to RegistrationState.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)

		// Verify that the forfeit VTXO was locked by checking the
		// ClientRegistration stored in the state.
		reg, exists := regState.ClientRegistrations["client1"]
		require.True(t, exists)
		require.Len(t, reg.ForfeitInputs, 1)
		require.Equal(
			t, &forfeitOutpoint, reg.ForfeitInputs[0].Outpoint,
		)

		// Assert that we have the expected outbox messages.
		h.assertOutboxLen(2)

		// Check success response to client.
		successResp := assertOutboxMessageType[*ClientSuccessResp](h, 0)
		require.Equal(t, "client1", string(successResp.Client))
	})

	t.Run("forfeit VTXO lock failure", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		// Create a boarding input outpoint.
		boardingOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding")),
			Index: 0,
		}

		// Create a forfeit VTXO outpoint.
		forfeitOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("forfeit")),
			Index: 0,
		}

		// Set up valid boarding input.
		h.setupValidBoardingInput(
			&boardingOutpoint, client.boardingKey, exitDelay, 10,
			h.roundID,
		)

		// Set up valid forfeit VTXO but make locking fail.
		descriptor, err := tree.NewVTXODescriptor(
			50000, client.boardingKey, h.operatorPub, 144,
		)
		require.NoError(t, err)

		vtxo := &VTXO{
			RoundID:          h.roundID,
			BatchOutputIndex: 0,
			Descriptor:       descriptor,
			Status:           VTXOStatusLive,
		}
		h.expectVTXO(forfeitOutpoint, vtxo)

		// Set up the VTXO lock to fail (e.g., already locked by another
		// round).
		lockErr := fmt.Errorf("VTXO already locked")
		h.vtxoStore.On(
			"LockVTXO", mock.Anything, h.roundID,
			[]wire.OutPoint{forfeitOutpoint},
		).Return(lockErr).Once()

		// We also need to expect the boarding input to be unlocked when
		// the forfeit VTXO lock fails.
		h.expectInputUnlocked(&boardingOutpoint, h.roundID)

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent with both boarding and forfeit.
		boardingReq := client.createBoardingRequest(&boardingOutpoint)
		forfeitReq := &types.ForfeitRequest{
			VTXOOutpoint: &forfeitOutpoint,
		}
		joinReqEvent := client.createJoinRequestWithForfeits(
			[]*types.BoardingRequest{boardingReq},
			[]*types.ForfeitRequest{forfeitReq},
		)
		err = h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Assert we remain in CreatedState due to lock failure.
		assertStateType[*CreatedState](h)

		// Assert that we have the expected outbox message (client
		// error).
		h.assertOutboxLen(1)
		msg := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Contains(t, msg.ErrorMsg, "failed to lock forfeit VTXO")
	})
}

// TestFSMRegistrationState tests the FSM transitions from RegistrationState.
func TestFSMRegistrationState(t *testing.T) {
	t.Parallel()

	t.Run("second client joins successfully", func(t *testing.T) {
		t.Parallel()

		const exitDelay = 144
		const expiry = 144

		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Create a RegistrationState with client1 already registered.
		client1Reg := buildTestClientRegistration(
			"client1",
			&BoardingInput{Outpoint: &outpoint1},
		)
		regState := &RegistrationState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": client1Reg,
			},
		}

		h := newTestHarness(t, regState)

		client2 := newClientHarness(
			t, "client2", 20, h.operatorPub, exitDelay, expiry,
		)
		h.setupValidBoardingInput(
			&outpoint2, client2.boardingKey, exitDelay, 10,
			h.roundID,
		)

		// Second client joins from RegistrationState.
		boardingReq2 := client2.createBoardingRequest(&outpoint2)
		joinReqEvent2 := client2.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		)
		err := h.sendEvent(joinReqEvent2)
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

		const exitDelay = 144
		const expiry = 144

		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Create a RegistrationState with client1 already registered.
		client1Reg := buildTestClientRegistration(
			"client1",
			&BoardingInput{Outpoint: &outpoint1},
		)
		regState := &RegistrationState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": client1Reg,
			},
		}

		h := newTestHarness(t, regState)

		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		// Same client attempts to join again with different inputs.
		boardingReq2 := client.createBoardingRequest(&outpoint2)
		joinReqEvent2 := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		)
		err := h.sendEvent(joinReqEvent2)
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
				h.roundID,
			)

			// For client2, set up validation to succeed but lock to
			// fail.
			h.setupBoardingInputValidationOnly(
				&outpoint2, client2.boardingKey, exitDelay, 10,
			)
			h.expectFailedLock(
				&outpoint2, h.roundID,
				fmt.Errorf("lock failed"),
			)

			h.setupValidBoardingInput(
				&outpoint3, client3.boardingKey, exitDelay, 10,
				h.roundID,
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

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		// Set up mocks for successful registration and batch building.
		h.setupValidBoardingInput(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)
		h.setupBatchBuildingMocks()

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
		// (internal) which builds the PSBT and transitions to
		// BatchBuiltState, then PrepareClientNotificationsEvent
		// (internal) sends batch info to clients and transitions to
		// AwaitingInputSigsState.
		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Verify the batch was built correctly.
		require.NotNil(t, awaitState.PSBT)
		require.Len(t, awaitState.ClientRegistrations, 1)
		require.Contains(
			t, awaitState.ClientRegistrations, ClientID("client1"),
		)

		// Verify outbox messages.
		// Should contain:
		// 1. RoundSealedReq (from RegistrationState timeout)
		// 2. ClientBatchInfo for client1
		// 3. ClientAwaitingInputSigsResp for client1
		// 4. StartTimeoutReq for boarding signatures
		var (
			foundSealReq          bool
			foundBatchInfo        bool
			foundAwaitingBrdgSigs bool
			foundTimeoutReq       bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *RoundSealedReq:
				foundSealReq = true
				require.Equal(t, h.env.RoundID, m.SealedRoundID)

			case *ClientBatchInfo:
				foundBatchInfo = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.NotNil(t, m.BatchPSBT)

			case *ClientAwaitingInputSigsResp:
				foundAwaitingBrdgSigs = true
				require.Equal(t, ClientID("client1"), m.Client)

			case *StartTimeoutReq:
				// Should be boarding signatures timeout.
				if m.Phase == TimeoutPhaseInputSigs {
					foundTimeoutReq = true
					require.Equal(
						t, h.env.RoundID, m.RoundID,
					)
				}
			}
		}
		require.True(t, foundSealReq, "RoundSealedReq emitted")
		require.True(t, foundBatchInfo, "ClientBatchInfo emitted")
		require.True(t, foundAwaitingBrdgSigs,
			"ClientAwaitingInputSigsResp emitted")
		require.True(
			t, foundTimeoutReq, "boarding sig timeout should start",
		)
	})
}

// TestFSMBatchBuilding tests the batch building states and transitions.
func TestFSMBatchBuilding(t *testing.T) {
	t.Parallel()

	t.Run("multi-client batch building", func(t *testing.T) {
		t.Parallel()

		// Create RegistrationState with two clients already registered.
		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		client1Reg := buildTestClientRegistration(
			"client1",
			&BoardingInput{Outpoint: &outpoint1},
		)
		client2Reg := buildTestClientRegistration(
			"client2",
			&BoardingInput{Outpoint: &outpoint2},
		)
		regState := &RegistrationState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": client1Reg,
				"client2": client2Reg,
			},
		}

		h := newTestHarness(t, regState)
		h.setupBatchBuildingMocks()

		// Seal via manual SealEvent.
		err := h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Should transition to AwaitingInputSigsState after
		// internal events.
		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Verify both clients are in the batch.
		require.Len(t, awaitState.ClientRegistrations, 2)
		require.Contains(
			t, awaitState.ClientRegistrations, ClientID("client1"),
		)
		require.Contains(
			t, awaitState.ClientRegistrations, ClientID("client2"),
		)

		// Verify both clients get batch info and awaiting boarding sigs
		// notification.
		batchInfoCount := 0
		awaitingBrdgSigsCount := 0
		for _, msg := range h.outboxMessages {
			if info, ok := msg.(*ClientBatchInfo); ok {
				batchInfoCount++
				require.NotNil(t, info.BatchPSBT)
				require.NotNil(t, info.BatchPSBT.UnsignedTx)
			}

			if _, ok := msg.(*ClientAwaitingInputSigsResp); ok {
				awaitingBrdgSigsCount++
			}
		}
		require.Equal(t, 2, batchInfoCount, "both clients get batch")
		require.Equal(t, 2, awaitingBrdgSigsCount,
			"both clients get awaiting boarding sigs notification")
	})

	t.Run("client batch info includes connector leaves",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, expiry,
			)

			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit")),
				Index: 0,
			}

			h.setupValidBoardingInput(
				&boardingOutpoint, client.boardingKey,
				exitDelay, 10, h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint, client.boardingKey, h.roundID,
			)
			h.expectVTXOLocked(h.roundID, forfeitOutpoint)
			h.setupBatchBuildingMocks()

			boardingReq := client.createBoardingRequest(
				&boardingOutpoint,
			)
			forfeitReq := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint,
			}
			joinReqEvent := client.createJoinRequestWithForfeits(
				[]*types.BoardingRequest{boardingReq},
				[]*types.ForfeitRequest{forfeitReq},
			)
			err := h.sendEvent(joinReqEvent)
			require.NoError(t, err)
			assertStateType[*RegistrationState](h)

			h.outboxMessages = nil
			err = h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			assertStateType[*AwaitingInputSigsState](h)
			batchInfo := h.getClientBatchInfo(client.clientID)
			require.NotNil(t, batchInfo)
			require.NotNil(t, batchInfo.ConnectorLeafMap)

			leafInfo, ok :=
				batchInfo.ConnectorLeafMap[forfeitOutpoint]
			require.True(t, ok)
			require.NotNil(t, leafInfo.LeafOutput)
		})

	t.Run("two clients receive connector leaves",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client1 := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, expiry,
			)
			client2 := newClientHarness(
				t, "client2", 10, h.operatorPub,
				exitDelay, expiry,
			)

			boardingOutpoint1 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding1")),
				Index: 0,
			}
			boardingOutpoint2 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding2")),
				Index: 0,
			}
			forfeitOutpoint1 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit1")),
				Index: 0,
			}
			forfeitOutpoint2 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit2")),
				Index: 0,
			}

			h.setupValidBoardingInput(
				&boardingOutpoint1, client1.boardingKey,
				exitDelay, 10, h.roundID,
			)
			h.setupValidBoardingInput(
				&boardingOutpoint2, client2.boardingKey,
				exitDelay, 10, h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint1, client1.boardingKey,
				h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint2, client2.boardingKey,
				h.roundID,
			)
			h.expectVTXOLocked(h.roundID, forfeitOutpoint1)
			h.expectVTXOLocked(h.roundID, forfeitOutpoint2)
			h.setupBatchBuildingMocks()

			boardingReq1 := client1.createBoardingRequest(
				&boardingOutpoint1,
			)
			forfeitReq1 := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint1,
			}
			err := h.sendEvent(
				client1.createJoinRequestWithForfeits(
					[]*types.BoardingRequest{boardingReq1},
					[]*types.ForfeitRequest{forfeitReq1},
				),
			)
			require.NoError(t, err)

			boardingReq2 := client2.createBoardingRequest(
				&boardingOutpoint2,
			)
			forfeitReq2 := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint2,
			}
			err = h.sendEvent(
				client2.createJoinRequestWithForfeits(
					[]*types.BoardingRequest{boardingReq2},
					[]*types.ForfeitRequest{forfeitReq2},
				),
			)
			require.NoError(t, err)

			h.outboxMessages = nil
			err = h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			client1Info := findClientBatchInfo(
				h.outboxMessages, "client1",
			)
			require.NotNil(t, client1Info)
			require.Contains(
				t, client1Info.ConnectorLeafMap,
				forfeitOutpoint1,
			)

			client2Info := findClientBatchInfo(
				h.outboxMessages, "client2",
			)
			require.NotNil(t, client2Info)
			require.Contains(
				t, client2Info.ConnectorLeafMap,
				forfeitOutpoint2,
			)
		})

	t.Run("single client gets multiple connector leaves",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, expiry,
			)

			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			forfeitOutpoint1 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit1")),
				Index: 0,
			}
			forfeitOutpoint2 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit2")),
				Index: 0,
			}

			h.setupValidBoardingInput(
				&boardingOutpoint, client.boardingKey,
				exitDelay, 10, h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint1, client.boardingKey,
				h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint2, client.boardingKey,
				h.roundID,
			)
			h.expectVTXOLocked(
				h.roundID, forfeitOutpoint1, forfeitOutpoint2,
			)
			h.setupBatchBuildingMocks()

			boardingReq := client.createBoardingRequest(
				&boardingOutpoint,
			)
			forfeitReq1 := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint1,
			}
			forfeitReq2 := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint2,
			}
			err := h.sendEvent(
				client.createJoinRequestWithForfeits(
					[]*types.BoardingRequest{boardingReq},
					[]*types.ForfeitRequest{
						forfeitReq1,
						forfeitReq2,
					},
				),
			)
			require.NoError(t, err)

			h.outboxMessages = nil
			err = h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			batchInfo := h.getClientBatchInfo(client.clientID)
			require.NotNil(t, batchInfo)
			require.Len(t, batchInfo.ConnectorLeafMap, 2)
			require.Contains(
				t, batchInfo.ConnectorLeafMap, forfeitOutpoint1,
			)
			require.Contains(
				t, batchInfo.ConnectorLeafMap, forfeitOutpoint2,
			)
		})

	t.Run("batch building captures forfeit connectors",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupBatchBuildingMocks()

			clientKey, _ := testutils.CreateKey(3)
			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			pkScript := buildExpectedPkScript(
				t, clientKey, h.operatorPub,
				h.env.Terms.BoardingExitDelay,
			)
			boardingInput := &BoardingInput{
				Outpoint: &boardingOutpoint,
				Value:    10000,
				PkScript: pkScript,
			}

			forfeitOutpoint1 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit1")),
				Index: 0,
			}
			forfeitOutpoint2 := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit2")),
				Index: 0,
			}
			forfeitInputs := []*ForfeitInput{
				{Outpoint: &forfeitOutpoint1},
				{Outpoint: &forfeitOutpoint2},
			}

			clientRegs := map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					BoardingInputs: []*BoardingInput{
						boardingInput,
					},
					ForfeitInputs: forfeitInputs,
				},
			}

			regState := &BatchBuildingState{
				ClientRegistrations: clientRegs,
			}

			transition, err := regState.ProcessEvent(
				t.Context(), &BuildBatchTxEvent{}, h.env,
			)
			require.NoError(t, err)
			require.NotNil(t, transition)

			nextState, ok := transition.NextState.(*BatchBuiltState)
			require.True(t, ok)
			require.NotNil(t, nextState.ConnectorTrees)
			require.NotNil(t, nextState.ConnectorAssignments)
			require.Len(
				t, nextState.ConnectorAssignments,
				len(forfeitInputs),
			)

			for _, input := range forfeitInputs {
				outpoint := *input.Outpoint
				assignment, ok :=
					nextState.ConnectorAssignments[outpoint]
				require.True(t, ok)
				require.NotNil(t, assignment.LeafOutput)
			}
		})

	t.Run("stale timeout ignored during boarding sigs",
		func(t *testing.T) {
			t.Parallel()

			// Create an AwaitingInputSigsState to start from.
			outpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input1")),
				Index: 0,
			}
			client1Reg := buildTestClientRegistration(
				"client1",
				&BoardingInput{Outpoint: &outpoint},
			)
			awaitState := &AwaitingInputSigsState{
				//nolint:ll
				ClientRegistrations: map[ClientID]*ClientRegistration{
					"client1": client1Reg,
				},
				PSBT: &psbt.Packet{
					UnsignedTx: wire.NewMsgTx(2),
				},
				VTXOTrees:           map[int]*tree.Tree{},
				ClientsSubmitted:    map[ClientID]struct{}{},
				CollectedSignatures: InputSigsMap{},
			}

			h := newTestHarness(t, awaitState)

			// Send stale RegistrationTimeoutEvent.
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			// Should remain in AwaitingInputSigsState with no
			// outbox messages.
			assertStateType[*AwaitingInputSigsState](h)
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

		// Set up mocks for successful registration but failing batch
		// building.
		h.setupValidBoardingInput(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)
		h.setupBatchBuildingFailure(fmt.Errorf("insufficient funds"))

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

		// Set up expectation for boarding input unlock when round
		// fails.
		h.expectInputUnlocked(&outpoint, h.env.RoundID)

		// Seal the round - this should trigger batch building which
		// will fail due to wallet funding error.
		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "insufficient funds")

		// Verify outbox messages:
		// 1. ClientRoundFailedResp for client1
		// 2. RoundFailedReq for the actor
		// Note: Boarding inputs are unlocked directly by the FSM, not
		// via outbox message.
		var (
			foundClientFailed bool
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

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
				require.Contains(t, m.Reason, "insufficient "+
					"funds")
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundRoundFailed, "actor should be notified")
	})

	t.Run("forfeit VTXOs unlocked on batch building failure",
		func(t *testing.T) {
			t.Parallel()

			// Set up the test harness.
			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub, exitDelay,
				expiry,
			)

			// Create a boarding input outpoint.
			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}

			// Create a forfeit VTXO outpoint.
			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit")),
				Index: 0,
			}

			// Set up mocks for successful registration with both
			// boarding and forfeit inputs.
			h.setupValidBoardingInput(
				&boardingOutpoint, client.boardingKey,
				exitDelay, 10, h.roundID,
			)
			h.setupValidForfeitVTXO(
				&forfeitOutpoint, client.boardingKey, h.roundID,
			)
			h.expectVTXOLocked(h.roundID, forfeitOutpoint)

			// Set up batch building to fail.
			h.setupBatchBuildingFailure(
				fmt.Errorf("insufficient funds"),
			)

			// Join with both boarding and forfeit to get to
			// RegistrationState.
			boardingReq := client.createBoardingRequest(
				&boardingOutpoint,
			)
			forfeitReq := &types.ForfeitRequest{
				VTXOOutpoint: &forfeitOutpoint,
			}
			joinReqEvent := client.createJoinRequestWithForfeits(
				[]*types.BoardingRequest{boardingReq},
				[]*types.ForfeitRequest{forfeitReq},
			)
			err := h.sendEvent(joinReqEvent)
			require.NoError(t, err)

			// Assert we're in RegistrationState.
			assertStateType[*RegistrationState](h)

			// Clear outbox.
			h.outboxMessages = nil

			// Set up expectations for both boarding input and
			// forfeit VTXO unlock when round fails.
			h.expectInputUnlocked(&boardingOutpoint, h.env.RoundID)
			h.expectVTXOUnlocked(h.env.RoundID, forfeitOutpoint)

			// Seal the round - this should trigger batch building
			// which will fail due to wallet funding error.
			err = h.sendEvent(&SealEvent{})
			require.NoError(t, err)

			// Should transition to FailedState.
			failedState := assertStateType[*FailedState](h)
			require.Contains(
				t, failedState.Reason, "insufficient funds",
			)

			// Verify outbox messages:
			// 1. ClientRoundFailedResp for client1
			// 2. RoundFailedReq for the actor
			var (
				foundClientFailed bool
				foundRoundFailed  bool
			)
			for _, msg := range h.outboxMessages {
				switch m := msg.(type) {
				case *ClientRoundFailedResp:
					foundClientFailed = true
					require.Equal(
						t, ClientID("client1"),
						m.Client,
					)

				case *RoundFailedReq:
					foundRoundFailed = true
					require.Equal(
						t, h.env.RoundID,
						m.FailedRoundID,
					)
				}
			}
			require.True(
				t, foundClientFailed,
				"client should be notified",
			)
			require.True(
				t, foundRoundFailed,
				"actor should be notified",
			)
		},
	)

	t.Run("FailedState is terminal and ignores events", func(t *testing.T) {
		t.Parallel()

		// Create a FailedState to start from.
		failedState := &FailedState{
			Reason: "test failure reason",
		}

		h := newTestHarness(t, failedState)

		// Try to send various events - all should be ignored.
		err := h.sendEvent(&ClientJoinRequestEvent{
			ClientID: ClientID("client2"),
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

	t.Run("boarding sig timeout goes to FailedState", func(t *testing.T) {
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

		// Set up mocks for successful registration and batch building.
		h.setupValidBoardingInput(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)
		h.setupBatchBuildingMocks()

		// Join to get to RegistrationState.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Seal via RegistrationTimeoutEvent to get to
		// AwaitingInputSigsState.
		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should be in AwaitingInputSigsState.
		assertStateType[*AwaitingInputSigsState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Set up expectation for boarding input unlock when round
		// fails.
		h.expectInputUnlocked(&outpoint, h.env.RoundID)

		// Send InputSignaturesTimeoutEvent.
		err = h.sendEvent(&InputSignaturesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages.
		// Note: Boarding inputs are unlocked directly by the FSM, not
		// via outbox message.
		var (
			foundClientFailed bool
			foundRoundFailed  bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
			}
		}
		require.True(t, foundClientFailed, "client notified of failure")
		require.True(t, foundRoundFailed, "actor notified of failure")
	})
}

// TestFSMBoardingSignatures tests the boarding signature collection flow.
func TestFSMBoardingSignatures(t *testing.T) {
	t.Parallel()

	t.Run("single client submits signatures", func(t *testing.T) {
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

		// Set up complete registration flow (join + batch building).
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)

		// We expect the round building and signing to succeed and
		// therefore for the round to be finalized and persisted.
		h.expectRoundFinalized(wire.NewMsgTx(2))

		// Join to get to RegistrationState.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		err := h.sendEvent(joinReqEvent)
		require.NoError(t, err)

		// Seal to get to AwaitingInputSigsState.
		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		awaitState := assertStateType[*AwaitingInputSigsState](h)
		require.NotNil(t, awaitState.PSBT)

		// Submit boarding signatures.
		h.outboxMessages = nil
		sigEvent := client.createBoardingSignaturesEvent(awaitState)
		err = h.sendEvent(sigEvent)
		require.NoError(t, err)

		// Should transition through ServerSigningState to
		// FinalizedState since all clients have submitted and signing
		// completes.
		finalState := assertStateType[*FinalizedState](h)
		require.NotNil(t, finalState.FinalTx)
		require.Len(t, finalState.ClientRegistrations, 1)

		// Verify timeout was cancelled.
		var foundCancelTimeout bool
		for _, msg := range h.outboxMessages {
			if cancel, ok := msg.(*CancelTimeoutReq); ok {
				foundCancelTimeout = true
				require.Equal(t, h.env.RoundID, cancel.RoundID)
				require.Equal(
					t, TimeoutPhaseInputSigs,
					cancel.Phase,
				)
			}
		}
		require.True(
			t, foundCancelTimeout, "timeout should be cancelled",
		)
	})

	t.Run("multi-client signature collection", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)

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

		// Set up complete registration flow for both clients. Batch
		// building mocks are only set up once since there's only one
		// batch built.
		h.setupCompleteRegistrationFlow(
			&outpoint1, client1.boardingKey, exitDelay, 10,
			h.roundID,
		)
		h.setupValidBoardingInput(
			&outpoint2, client2.boardingKey, exitDelay, 10,
			h.roundID,
		)

		// We expect the round building and signing to succeed and
		// therefore for the round to be persisted.
		h.expectRoundFinalized(wire.NewMsgTx(2))

		// Both clients join.
		boardingReq1 := client1.createBoardingRequest(&outpoint1)
		err := h.sendEvent(client1.createJoinRequest(
			[]*types.BoardingRequest{boardingReq1},
		))
		require.NoError(t, err)

		boardingReq2 := client2.createBoardingRequest(&outpoint2)
		err = h.sendEvent(client2.createJoinRequest(
			[]*types.BoardingRequest{boardingReq2},
		))
		require.NoError(t, err)

		// Seal to get to AwaitingInputSigsState.
		h.outboxMessages = nil
		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		awaitState := assertStateType[*AwaitingInputSigsState](h)
		require.Empty(t, awaitState.ClientsSubmitted)

		// Client1 submits - should remain in AwaitingInputSigsState.
		h.outboxMessages = nil
		sig1Event := client1.createBoardingSignaturesEvent(awaitState)
		err = h.sendEvent(sig1Event)
		require.NoError(t, err)

		awaitState = assertStateType[*AwaitingInputSigsState](h)
		require.Len(t, awaitState.ClientsSubmitted, 1)
		require.True(t, awaitState.hasClientSubmitted("client1"))
		require.False(t, awaitState.hasClientSubmitted("client2"))

		// No outbox messages yet (no transition).
		h.assertOutboxLen(0)

		// Client2 submits - should transition through
		// ServerSigningState to FinalizedState.
		sig2Event := client2.createBoardingSignaturesEvent(awaitState)
		err = h.sendEvent(sig2Event)
		require.NoError(t, err)

		finalState := assertStateType[*FinalizedState](h)
		require.Len(t, finalState.ClientRegistrations, 2)

		// Verify timeout was cancelled.
		var foundCancelTimeout bool
		for _, msg := range h.outboxMessages {
			if cancel, ok := msg.(*CancelTimeoutReq); ok {
				foundCancelTimeout = true
				require.Equal(
					t, TimeoutPhaseInputSigs,
					cancel.Phase,
				)
			}
		}
		require.True(
			t, foundCancelTimeout, "timeout should be cancelled",
		)
	})

	t.Run("unknown client rejected", func(t *testing.T) {
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

		// Set up complete registration flow.
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)

		// We expect the round building and signing to succeed and
		// therefore for the round to be persisted.
		h.expectRoundFinalized(wire.NewMsgTx(2))

		// Join and seal.
		boardingReq := client.createBoardingRequest(&outpoint)
		err := h.sendEvent(client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		))
		require.NoError(t, err)

		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Unknown client tries to submit.
		h.outboxMessages = nil
		unknownSigEvent := &ClientBoardingSignaturesEvent{
			ClientID:   "unknown_client",
			Signatures: nil,
		}
		err = h.sendEvent(unknownSigEvent)
		require.NoError(t, err)

		// Should remain in AwaitingInputSigsState.
		assertStateType[*AwaitingInputSigsState](h)

		// Should have error response.
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("unknown_client"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "registered")

		// Original client should still be able to submit its valid sig.
		h.outboxMessages = nil
		sigEvent := client.createBoardingSignaturesEvent(awaitState)
		err = h.sendEvent(sigEvent)
		require.NoError(t, err)

		finalState := assertStateType[*FinalizedState](h)
		require.Len(t, finalState.ClientRegistrations, 1)
	})

	t.Run("duplicate submission rejected", func(t *testing.T) {
		t.Parallel()

		// Set up the test harness.
		h := newTestHarness(t)

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

		// Set up complete registration flow for both clients.
		h.setupCompleteRegistrationFlow(
			&outpoint1, client1.boardingKey, exitDelay, 10,
			h.roundID,
		)
		h.setupValidBoardingInput(
			&outpoint2, client2.boardingKey, exitDelay, 10,
			h.roundID,
		)

		// Both clients join.
		err := h.sendEvent(client1.createJoinRequest(
			[]*types.BoardingRequest{
				client1.createBoardingRequest(&outpoint1),
			},
		))
		require.NoError(t, err)

		err = h.sendEvent(client2.createJoinRequest(
			[]*types.BoardingRequest{
				client2.createBoardingRequest(&outpoint2),
			},
		))
		require.NoError(t, err)

		// Seal.
		h.outboxMessages = nil
		err = h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Client1 submits first time - success.
		h.outboxMessages = nil
		sig1Event := client1.createBoardingSignaturesEvent(awaitState)
		err = h.sendEvent(sig1Event)
		require.NoError(t, err)

		awaitState = assertStateType[*AwaitingInputSigsState](h)
		require.True(t, awaitState.hasClientSubmitted("client1"))

		// Client1 tries to submit again - should be rejected.
		h.outboxMessages = nil
		sig1EventDup := client1.createBoardingSignaturesEvent(
			awaitState,
		)
		err = h.sendEvent(sig1EventDup)
		require.NoError(t, err)

		// Should remain in same state.
		awaitState = assertStateType[*AwaitingInputSigsState](h)
		require.Len(t, awaitState.ClientsSubmitted, 1)

		// Should have error response.
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "already submitted")
	})

	t.Run("wrong signature count rejected", func(t *testing.T) {
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

		// Set up complete registration flow.
		h.setupCompleteRegistrationFlow(
			&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
		)

		// Join and seal.
		boardingReq := client.createBoardingRequest(&outpoint)
		err := h.sendEvent(client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		))
		require.NoError(t, err)

		h.outboxMessages = nil
		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		assertStateType[*AwaitingInputSigsState](h)

		// Submit with no signatures (client has 1 input).
		h.outboxMessages = nil
		badSigEvent := &ClientBoardingSignaturesEvent{
			ClientID:   "client1",
			Signatures: []*types.BoardingInputSignature{},
		}
		err = h.sendEvent(badSigEvent)
		require.NoError(t, err)

		// Should remain in AwaitingInputSigsState.
		assertStateType[*AwaitingInputSigsState](h)

		// Should have error response.
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "expected 1 signatures")
	})
}

// TestFSMFinalizedState tests the FSM transitions from FinalizedState.
func TestFSMFinalizedState(t *testing.T) {
	t.Parallel()

	t.Run("confirmation transitions to ConfirmedState", func(t *testing.T) {
		t.Parallel()

		// Create a FinalizedState to start from.
		finalTx := wire.NewMsgTx(2)
		finalState := &FinalizedState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
			FinalTx:   finalTx,
			VTXOTrees: map[int]*tree.Tree{},
		}

		h := newTestHarness(t, finalState)

		// Send TransactionConfirmedEvent.
		blockHash := chainhash.HashH([]byte("test-block"))
		confirmEvent := &TransactionConfirmedEvent{
			BlockHeight: 100,
			BlockHash:   blockHash,
			NumConfs:    6,
		}

		err := h.sendEvent(confirmEvent)
		require.NoError(t, err)

		// Should transition to ConfirmedState.
		confirmedState := assertStateType[*ConfirmedState](h)
		require.Equal(t, int32(100), confirmedState.BlockHeight)
		require.Equal(t, blockHash, confirmedState.BlockHash)
		require.Equal(t, finalTx, confirmedState.FinalTx)
		require.Len(t, confirmedState.ClientRegistrations, 1)

		// No outbox messages emitted.
		h.assertOutboxLen(0)
	})

	t.Run("stale timeout ignored", func(t *testing.T) {
		t.Parallel()

		// Create a FinalizedState to start from.
		finalState := &FinalizedState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
			FinalTx:   wire.NewMsgTx(2),
			VTXOTrees: map[int]*tree.Tree{},
		}

		h := newTestHarness(t, finalState)

		// Send stale RegistrationTimeoutEvent - should be ignored.
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in FinalizedState.
		assertStateType[*FinalizedState](h)
		h.assertOutboxLen(0)

		// Send stale InputSignaturesTimeoutEvent - should be
		// ignored.
		err = h.sendEvent(&InputSignaturesTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in FinalizedState.
		assertStateType[*FinalizedState](h)
		h.assertOutboxLen(0)
	})

	t.Run("ConfirmedState is terminal", func(t *testing.T) {
		t.Parallel()

		// Create a ConfirmedState to start from.
		originalBlockHeight := int32(100)
		originalBlockHash := chainhash.HashH([]byte("test-block"))
		confirmedState := &ConfirmedState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {},
			},
			FinalTx:     wire.NewMsgTx(2),
			VTXOTrees:   map[int]*tree.Tree{},
			BlockHeight: originalBlockHeight,
			BlockHash:   originalBlockHash,
		}

		h := newTestHarness(t, confirmedState)

		// Try to send various events - all should be ignored.
		err := h.sendEvent(&ClientJoinRequestEvent{})
		require.NoError(t, err)
		assertStateType[*ConfirmedState](h)
		h.assertOutboxLen(0)

		err = h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)
		assertStateType[*ConfirmedState](h)
		h.assertOutboxLen(0)

		// Another confirmation event should also be ignored.
		err = h.sendEvent(&TransactionConfirmedEvent{
			BlockHeight: 200,
			BlockHash:   chainhash.HashH([]byte("another-block")),
			NumConfs:    10,
		})
		require.NoError(t, err)

		// Should remain in same ConfirmedState with original data.
		confirmedState = assertStateType[*ConfirmedState](h)
		require.Equal(
			t, originalBlockHeight, confirmedState.BlockHeight,
		)
		require.Equal(t, originalBlockHash, confirmedState.BlockHash)
		h.assertOutboxLen(0)
	})
}

// TestFSMAwaitingVTXONoncesState tests the FSM transitions from
// AwaitingVTXONoncesState.
func TestFSMAwaitingVTXONoncesState(t *testing.T) {
	t.Parallel()

	t.Run("timeout transitions to FailedState", func(t *testing.T) {
		t.Parallel()

		// Create an AwaitingVTXONoncesState with one client registered.
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		awaitState := buildAwaitingVTXONoncesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {
					boardingInputs: []*BoardingInput{
						{Outpoint: &outpoint},
					},
				},
			},
		)

		h := newTestHarness(t, awaitState)

		// Set up expectation for boarding input unlock when round
		// fails.
		h.expectInputUnlocked(&outpoint, h.env.RoundID)

		// Send VTXONoncesTimeoutEvent.
		err := h.sendEvent(&VTXONoncesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "VTXO nonce collection")
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages:
		// 1. ClientRoundFailedResp for client1
		// 2. RoundFailedReq for the actor
		// Note: Boarding inputs are unlocked directly by the FSM, not
		// via outbox message.
		var (
			foundClientFailed bool
			foundRoundFailed  bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.Equal(t, h.env.RoundID, m.RoundID)
				require.Contains(t, m.Reason, "VTXO nonce")

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
				require.Contains(t, m.Reason, "VTXO nonce")
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundRoundFailed, "actor should be notified")
	})

	t.Run("stale registration timeout ignored", func(t *testing.T) {
		t.Parallel()

		// Create an AwaitingVTXONoncesState.
		awaitState := buildAwaitingVTXONoncesState(
			map[ClientID]vtxoNoncesStateOpts{"client1": {}},
		)
		h := newTestHarness(t, awaitState)

		// Send stale RegistrationTimeoutEvent - should be ignored.
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in AwaitingVTXONoncesState.
		assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(0)
	})

	t.Run("nonces from unregistered client rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with only client1 registered.
		awaitState := buildAwaitingVTXONoncesState(
			map[ClientID]vtxoNoncesStateOpts{"client1": {}},
		)
		h := newTestHarness(t, awaitState)

		// Send nonces from unregistered client2.
		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client2",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				route.NewVertex(h.operatorPub): {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client2"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "not registered")
	})

	t.Run("nonces from client without VTXOs rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with client1 registered but no VTXOs.
		awaitState := buildAwaitingVTXONoncesState(
			map[ClientID]vtxoNoncesStateOpts{"client1": {}},
		)
		h := newTestHarness(t, awaitState)

		// Send nonces from client1 who has no VTXOs.
		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client1",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				route.NewVertex(h.operatorPub): {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "no VTXOs")
	})

	t.Run("duplicate nonces submission rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with client1 having VTXOs and already submitted.
		awaitState := buildAwaitingVTXONoncesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {
					withVTXOs:        true,
					alreadySubmitted: true,
				},
			},
		)
		h := newTestHarness(t, awaitState)

		var signingKey SigningKeyHex
		for _, desc := range awaitState.ClientRegistrations["client1"].
			VTXODescriptors {
			signingKey = route.NewVertex(desc.CoSignerKey)
			break
		}

		// Send duplicate nonces from client1.
		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client1",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				signingKey: {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "already submitted")
	})

	t.Run("partial key submission rejected", func(t *testing.T) {
		t.Parallel()

		// Create client with multiple signing keys.
		key1, _ := testutils.CreateKey(100)
		key2, _ := testutils.CreateKey(101)
		keyHex1 := route.NewVertex(key1)
		keyHex2 := route.NewVertex(key2)

		awaitState := &AwaitingVTXONoncesState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{ //nolint:ll
						keyHex1: {CoSignerKey: key1},
						keyHex2: {CoSignerKey: key2},
					},
				},
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees:            map[int]*tree.Tree{},
			TreeSignCoordinators: map[int]*batch.TreeSignCoordinator{}, //nolint:ll
			ClientsWithNonces:    make(map[ClientID]struct{}),
		}

		h := newTestHarness(t, awaitState)

		// Submit nonces for only one of the two keys.
		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client1",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				keyHex1: {},
			},
		})
		require.NoError(t, err)

		// Should reject and remain in same state.
		assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "missing nonces")
		require.Contains(t, errResp.ErrorMsg, "signing key")
	})

	t.Run("empty nonces for key rejected", func(t *testing.T) {
		t.Parallel()

		key1, _ := testutils.CreateKey(200)
		key2, _ := testutils.CreateKey(201)
		keyHex1 := route.NewVertex(key1)
		keyHex2 := route.NewVertex(key2)

		awaitState := &AwaitingVTXONoncesState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{ //nolint:ll
						keyHex1: {CoSignerKey: key1},
						keyHex2: {CoSignerKey: key2},
					},
				},
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees:            map[int]*tree.Tree{},
			TreeSignCoordinators: map[int]*batch.TreeSignCoordinator{}, //nolint:ll
			ClientsWithNonces:    make(map[ClientID]struct{}),
		}

		h := newTestHarness(t, awaitState)

		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client1",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				keyHex1: {},
				keyHex2: {},
			},
		})
		require.NoError(t, err)

		state := assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(
			t, errResp.ErrorMsg, "no nonces for signing key",
		)
		require.Empty(t, state.ClientsWithNonces)
	})

	t.Run("empty nonce map rejected", func(t *testing.T) {
		t.Parallel()

		key, _ := testutils.CreateKey(150)
		keyHex := route.NewVertex(key)

		coordinators := map[int]*batch.TreeSignCoordinator{
			0: {},
		}

		awaitState := &AwaitingVTXONoncesState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{ //nolint:ll
						keyHex: {
							CoSignerKey: key,
						},
					},
				},
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees:            map[int]*tree.Tree{},
			TreeSignCoordinators: coordinators,
			ClientsWithNonces:    make(map[ClientID]struct{}),
		}

		h := newTestHarness(t, awaitState)

		err := h.sendEvent(&ClientVTXONoncesEvent{
			ClientID: "client1",
			Nonces: map[SigningKeyHex]map[tree.TxID]tree.Musig2PubNonce{ //nolint:ll
				keyHex: {},
			},
		})
		require.NoError(t, err)

		state := assertStateType[*AwaitingVTXONoncesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(
			t, errResp.ErrorMsg, "no nonces for signing key",
		)
		require.Empty(t, state.ClientsWithNonces)
	})
}

// TestFSMVTXOSigningFlowE2ERealSigs exercises the full VTXO signing flow with
// real MuSig2 signing and validates the aggregated signatures against the
// constructed VTXO tree.
func TestFSMVTXOSigningFlowE2ERealSigs(t *testing.T) {
	t.Parallel()

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

	// Allow the boarding input and set up funding/signing expectations.
	h.setupCompleteRegistrationFlow(
		&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
	)

	finalTx := wire.NewMsgTx(2)
	h.expectRoundFinalized(finalTx)

	boardingReq := client.createBoardingRequest(&outpoint)
	vtxoReq := client.createVTXORequest(btcutil.Amount(50000))
	h.expectPersistVTXOs(client.vtxoPkScripts()...)

	err := h.sendEvent(client.createJoinRequestWithVTXOs(
		[]*types.BoardingRequest{boardingReq},
		[]*types.VTXORequest{vtxoReq},
	))
	require.NoError(t, err)
	assertStateType[*RegistrationState](h)

	// Seal via timeout to start nonce collection.
	h.outboxMessages = nil
	err = h.sendEvent(&RegistrationTimeoutEvent{})
	require.NoError(t, err)

	awaitNonces := assertStateType[*AwaitingVTXONoncesState](h)
	require.NotNil(t, awaitNonces.PSBT)
	require.NotEmpty(t, awaitNonces.VTXOTrees)

	batchInfo := h.getClientBatchInfo(client.clientID)
	require.NotNil(t, batchInfo)
	require.NotNil(t, batchInfo.BatchPSBT)
	require.NotEmpty(t, batchInfo.VTXOTreePaths)

	keys := client.vtxoSigningKeys()
	require.NotEmpty(t, keys)

	// Client submits real MuSig2 nonces.
	h.outboxMessages = nil
	nonceEvent := client.createVTXONoncesEvent(
		keys[0], batchInfo.VTXOTreePaths,
	)
	err = h.sendEvent(nonceEvent)
	require.NoError(t, err)

	awaitSigs := assertStateType[*AwaitingVTXOSignaturesState](h)
	require.NotEmpty(t, awaitSigs.TreeSignCoordinators)

	aggNonces := h.getClientVTXOAggNonces(client.clientID)
	require.NotNil(t, aggNonces)
	require.NotEmpty(t, aggNonces.AggNonces)

	// Client registers aggregated nonces and submits partial signatures.
	h.outboxMessages = nil
	sigEvent := client.createVTXOPartialSigsEvent(
		keys[0], batchInfo.VTXOTreePaths, aggNonces.AggNonces,
	)
	err = h.sendEvent(sigEvent)
	require.NoError(t, err)

	awaitBoarding := assertStateType[*AwaitingInputSigsState](h)

	aggSigs := h.getClientVTXOAggSigs(client.clientID)
	require.NotNil(t, aggSigs)
	require.NotEmpty(t, aggSigs.AggSigs)

	// Validate aggregated signatures against the tree.
	for _, vtxoTree := range awaitBoarding.VTXOTrees {
		err := vtxoTree.SubmitTreeSigs(aggSigs.AggSigs)
		require.NoError(t, err)
		require.NoError(t, vtxoTree.VerifySigned())
	}

	// Client submits boarding signatures to finalize.
	h.outboxMessages = nil
	boardingSigEvent := client.createBoardingSignaturesEvent(
		awaitBoarding,
	)
	err = h.sendEvent(boardingSigEvent)
	require.NoError(t, err)

	finalState := assertStateType[*FinalizedState](h)
	require.NotNil(t, finalState.FinalTx)
	require.Len(t, finalState.ClientRegistrations, 1)

	var foundBroadcast bool
	for _, msg := range h.outboxMessages {
		if m, ok := msg.(*BroadcastRoundReq); ok {
			foundBroadcast = true
			require.Equal(t, finalTx, m.SignedTx)
		}
	}

	require.True(t, foundBroadcast, "broadcast should be requested")

	// Simulate confirmation and ensure round + VTXOs are updated.
	h.outboxMessages = nil
	err = h.sendEvent(&TransactionConfirmedEvent{
		BlockHeight: 100,
		BlockHash:   chainhash.Hash{},
	})
	require.NoError(t, err)
	assertStateType[*ConfirmedState](h)
	h.assertOutboxLen(0)
	h.vtxoStore.AssertCalled(t,
		"MarkVTXOsLive", mock.Anything, h.roundID,
	)
	h.roundStore.AssertCalled(t,
		"MarkRoundConfirmed", mock.Anything, h.roundID,
		int32(100), mock.Anything,
	)
}

// TestFSMVTXOMultiClientRealSigs covers two clients each with a VTXO, ensuring
// real MuSig2 nonces/signatures flow correctly across clients.
func TestFSMVTXOMultiClientRealSigs(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

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

	// Prepare mocks: fund once, validate both boarding inputs.
	h.setupCompleteRegistrationFlow(
		&outpoint1, client1.boardingKey, exitDelay, 10, h.roundID,
	)
	h.setupValidBoardingInput(
		&outpoint2, client2.boardingKey, exitDelay, 10, h.roundID,
	)

	finalTx := wire.NewMsgTx(2)
	h.expectRoundFinalized(finalTx)

	vtxoReq1 := client1.createVTXORequest(btcutil.Amount(50_000))
	vtxoReq2 := client2.createVTXORequest(btcutil.Amount(60_000))
	h.expectPersistVTXOs(
		append(client1.vtxoPkScripts(), client2.vtxoPkScripts()...)...,
	)

	// Both clients join with boarding + VTXO requests.
	err := h.sendEvent(client1.createJoinRequestWithVTXOs(
		[]*types.BoardingRequest{
			client1.createBoardingRequest(&outpoint1),
		},
		[]*types.VTXORequest{
			vtxoReq1,
		},
	))
	require.NoError(t, err)

	err = h.sendEvent(client2.createJoinRequestWithVTXOs(
		[]*types.BoardingRequest{
			client2.createBoardingRequest(&outpoint2),
		},
		[]*types.VTXORequest{
			vtxoReq2,
		},
	))
	require.NoError(t, err)

	// Seal to move into nonce collection.
	h.outboxMessages = nil
	err = h.sendEvent(&SealEvent{})
	require.NoError(t, err)

	awaitNonces := assertStateType[*AwaitingVTXONoncesState](h)
	require.NotEmpty(t, awaitNonces.VTXOTrees)

	batchInfo1 := h.getClientBatchInfo(client1.clientID)
	batchInfo2 := h.getClientBatchInfo(client2.clientID)
	require.NotNil(t, batchInfo1)
	require.NotNil(t, batchInfo2)

	// Client1 submits nonces.
	h.outboxMessages = nil
	key1 := client1.vtxoSigningKeys()[0]
	err = h.sendEvent(client1.createVTXONoncesEvent(
		key1, batchInfo1.VTXOTreePaths,
	))
	require.NoError(t, err)

	// Still waiting on client2.
	assertStateType[*AwaitingVTXONoncesState](h)

	// Client2 submits nonces; should transition to AwaitingVTXOSignatures.
	key2 := client2.vtxoSigningKeys()[0]
	err = h.sendEvent(client2.createVTXONoncesEvent(
		key2, batchInfo2.VTXOTreePaths,
	))
	require.NoError(t, err)

	awaitSigs := assertStateType[*AwaitingVTXOSignaturesState](h)
	require.Empty(t, awaitSigs.ClientsWithSignatures)

	aggNonces1 := h.getClientVTXOAggNonces(client1.clientID)
	aggNonces2 := h.getClientVTXOAggNonces(client2.clientID)
	require.NotNil(t, aggNonces1)
	require.NotNil(t, aggNonces2)

	// Client1 submits partial sigs; still waiting on client2.
	h.outboxMessages = nil
	err = h.sendEvent(client1.createVTXOPartialSigsEvent(
		key1, batchInfo1.VTXOTreePaths, aggNonces1.AggNonces,
	))
	require.NoError(t, err)
	assertStateType[*AwaitingVTXOSignaturesState](h)

	// Client2 submits partial sigs; transition to boarding sigs.
	err = h.sendEvent(client2.createVTXOPartialSigsEvent(
		key2, batchInfo2.VTXOTreePaths, aggNonces2.AggNonces,
	))
	require.NoError(t, err)

	awaitBoarding := assertStateType[*AwaitingInputSigsState](h)

	aggSigs1 := h.getClientVTXOAggSigs(client1.clientID)
	aggSigs2 := h.getClientVTXOAggSigs(client2.clientID)
	require.NotNil(t, aggSigs1)
	require.NotNil(t, aggSigs2)

	combinedSigs := make(map[tree.TxID]*schnorr.Signature)
	for txid, sig := range aggSigs1.AggSigs {
		combinedSigs[txid] = sig
	}
	for txid, sig := range aggSigs2.AggSigs {
		combinedSigs[txid] = sig
	}

	// Validate aggregated signatures once against the tree set.
	for _, vtxoTree := range awaitBoarding.VTXOTrees {
		err := vtxoTree.SubmitTreeSigs(combinedSigs)
		require.NoError(t, err)
		require.NoError(t, vtxoTree.VerifySigned())
	}

	// Both clients submit boarding signatures.
	h.outboxMessages = nil
	err = h.sendEvent(client1.createBoardingSignaturesEvent(
		awaitBoarding,
	))
	require.NoError(t, err)

	awaitBoarding = assertStateType[*AwaitingInputSigsState](h)
	err = h.sendEvent(client2.createBoardingSignaturesEvent(
		awaitBoarding,
	))
	require.NoError(t, err)

	finalState := assertStateType[*FinalizedState](h)
	require.NotNil(t, finalState.FinalTx)
	require.Len(t, finalState.ClientRegistrations, 2)
}

// TestFSMVTXOMultiKeyPerClientRealSigs ensures a single client with multiple
// VTXO signing keys can complete the signing flow.
func TestFSMVTXOMultiKeyPerClientRealSigs(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	const exitDelay = 144
	const expiry = 144
	client := newClientHarness(
		t, "client1", 10, h.operatorPub, exitDelay, expiry,
	)

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("input-multi")),
		Index: 0,
	}

	h.setupCompleteRegistrationFlow(
		&outpoint, client.boardingKey, exitDelay, 10, h.roundID,
	)

	finalTx := wire.NewMsgTx(2)
	h.expectRoundFinalized(finalTx)

	// Two VTXO requests with distinct signing keys.
	vtxoReq1 := client.createVTXORequest(btcutil.Amount(40_000))
	vtxoReq2 := client.createVTXORequest(btcutil.Amount(50_000))
	h.expectPersistVTXOs(client.vtxoPkScripts()...)

	err := h.sendEvent(client.createJoinRequestWithVTXOs(
		[]*types.BoardingRequest{
			client.createBoardingRequest(&outpoint),
		},
		[]*types.VTXORequest{vtxoReq1, vtxoReq2},
	))
	require.NoError(t, err)

	h.outboxMessages = nil
	err = h.sendEvent(&RegistrationTimeoutEvent{})
	require.NoError(t, err)

	awaitNonces := assertStateType[*AwaitingVTXONoncesState](h)
	require.NotEmpty(t, awaitNonces.VTXOTrees)

	batchInfo := h.getClientBatchInfo(client.clientID)
	require.NotNil(t, batchInfo)

	keys := client.vtxoSigningKeys()
	require.Len(t, keys, 2)

	// Submit all nonces in a single message.
	err = h.sendEvent(client.createVTXONoncesEventAll(
		batchInfo.VTXOTreePaths,
	))
	require.NoError(t, err)

	assertStateType[*AwaitingVTXOSignaturesState](h)
	aggNonces := h.getClientVTXOAggNonces(client.clientID)
	require.NotNil(t, aggNonces)

	// Submit all partial signatures in a single message.
	err = h.sendEvent(client.createVTXOPartialSigsEventAll(
		batchInfo.VTXOTreePaths, aggNonces.AggNonces,
	))
	require.NoError(t, err)

	awaitBoarding := assertStateType[*AwaitingInputSigsState](h)

	aggSigs := h.getClientVTXOAggSigs(client.clientID)
	require.NotNil(t, aggSigs)

	for _, vtxoTree := range awaitBoarding.VTXOTrees {
		err := vtxoTree.SubmitTreeSigs(aggSigs.AggSigs)
		require.NoError(t, err)
		require.NoError(t, vtxoTree.VerifySigned())
	}

	// Finish boarding signatures.
	h.outboxMessages = nil
	err = h.sendEvent(client.createBoardingSignaturesEvent(awaitBoarding))
	require.NoError(t, err)

	finalState := assertStateType[*FinalizedState](h)
	require.NotNil(t, finalState.FinalTx)
	require.Len(t, finalState.ClientRegistrations, 1)
}

// TestFSMBatchBuiltState tests the FSM transitions from BatchBuiltState.
func TestFSMBatchBuiltState(t *testing.T) {
	t.Parallel()

	t.Run("without VTXOs transitions to AwaitingInputSigsState",
		func(t *testing.T) {
			t.Parallel()

			// Create a BatchBuiltState with no VTXOs.
			outpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input1")),
				Index: 0,
			}
			client1Reg := buildTestClientRegistration(
				"client1",
				&BoardingInput{Outpoint: &outpoint},
			)

			regs := map[ClientID]*ClientRegistration{
				"client1": client1Reg,
			}
			batchBuiltState := &BatchBuiltState{
				ClientRegistrations: regs,
				PSBT: &psbt.Packet{
					UnsignedTx: wire.NewMsgTx(2),
				},
				VTXOTrees: nil,
			}

			h := newTestHarness(t, batchBuiltState)

			// Send PrepareClientNotificationsEvent.
			err := h.sendEvent(&PrepareClientNotificationsEvent{})
			require.NoError(t, err)

			// Should transition to AwaitingInputSigsState.
			bs := assertStateType[*AwaitingInputSigsState](h)
			require.NotNil(t, bs.PSBT)
			require.Len(t, bs.ClientRegistrations, 1)

			// Verify outbox messages.
			var foundBatch, foundBrdgSigs, foundTimeout bool
			client1ID := ClientID("client1")
			for _, msg := range h.outboxMessages {
				switch m := msg.(type) {
				case *ClientBatchInfo:
					foundBatch = true
					require.Equal(t, client1ID, m.Client)
					require.Empty(t, m.VTXOTreePaths)

				case *ClientAwaitingInputSigsResp:
					foundBrdgSigs = true
					require.Equal(t, client1ID, m.Client)

				case *StartTimeoutReq:
					if m.Phase == TimeoutPhaseInputSigs {
						foundTimeout = true
					}
				}
			}
			require.True(t, foundBatch)
			require.True(t, foundBrdgSigs)
			require.True(t, foundTimeout)
		},
	)

	t.Run("sends batch info to each client", func(t *testing.T) {
		t.Parallel()

		// Create BatchBuiltState with two clients.
		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}
		client1Reg := buildTestClientRegistration(
			"client1", &BoardingInput{Outpoint: &outpoint1},
		)
		client2Reg := buildTestClientRegistration(
			"client2", &BoardingInput{Outpoint: &outpoint2},
		)

		batchBuiltState := &BatchBuiltState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": client1Reg,
				"client2": client2Reg,
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees: nil,
		}

		h := newTestHarness(t, batchBuiltState)

		// Send PrepareClientNotificationsEvent.
		err := h.sendEvent(&PrepareClientNotificationsEvent{})
		require.NoError(t, err)

		// Both clients should receive ClientBatchInfo.
		client1Info := findClientBatchInfo(h.outboxMessages, "client1")
		client2Info := findClientBatchInfo(h.outboxMessages, "client2")
		require.NotNil(t, client1Info, "client1 should get batch info")
		require.NotNil(t, client2Info, "client2 should get batch info")
	})
}

// findClientBatchInfo searches the outbox messages for a ClientBatchInfo
// message for the specified client.
func findClientBatchInfo(msgs []OutboxEvent,
	clientID ClientID) *ClientBatchInfo {

	for _, msg := range msgs {
		if info, ok := msg.(*ClientBatchInfo); ok {
			if info.Client == clientID {
				return info
			}
		}
	}

	return nil
}

// TestFSMAwaitingVTXOSignaturesState tests FSM transitions from
// AwaitingVTXOSignaturesState.
func TestFSMAwaitingVTXOSignaturesState(t *testing.T) {
	t.Parallel()

	t.Run("timeout transitions to FailedState", func(t *testing.T) {
		t.Parallel()

		// Create state with one client with boarding inputs.
		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {
					boardingInputs: []*BoardingInput{
						{Outpoint: &outpoint},
					},
				},
			},
		)
		h := newTestHarness(t, awaitState)

		// Set up expectation for boarding input unlock when round
		// fails.
		h.expectInputUnlocked(&outpoint, h.env.RoundID)

		// Send VTXOSignaturesTimeoutEvent.
		err := h.sendEvent(&VTXOSignaturesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "VTXO signature")
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages.
		// Note: Boarding inputs are unlocked directly by the FSM, not
		// via outbox message.
		var (
			foundClientFailed bool
			foundRoundFailed  bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundRoundFailed, "actor should be notified")
	})

	t.Run("stale nonces timeout ignored", func(t *testing.T) {
		t.Parallel()

		// Create state.
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{"client1": {}},
		)
		h := newTestHarness(t, awaitState)

		// Send stale VTXONoncesTimeoutEvent - should be ignored.
		err := h.sendEvent(&VTXONoncesTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in AwaitingVTXOSignaturesState.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(0)
	})

	t.Run("stale registration timeout ignored", func(t *testing.T) {
		t.Parallel()

		// Create state.
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{"client1": {}},
		)
		h := newTestHarness(t, awaitState)

		// Send stale RegistrationTimeoutEvent - should be ignored.
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Should remain in AwaitingVTXOSignaturesState.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(0)
	})

	t.Run("sigs from unregistered client rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with only client1 registered.
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {withVTXOs: true},
			},
		)
		h := newTestHarness(t, awaitState)

		// Send partial sigs from unregistered client2.
		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client2",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				route.NewVertex(h.operatorPub): {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client2"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "not registered")
	})

	t.Run("sigs from client without VTXOs rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with client1 registered but no VTXOs.
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {withVTXOs: false},
			},
		)
		h := newTestHarness(t, awaitState)

		// Send partial sigs from client1 who has no VTXOs.
		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client1",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				route.NewVertex(h.operatorPub): {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "no VTXOs")
	})

	t.Run("duplicate sigs submission rejected", func(t *testing.T) {
		t.Parallel()

		// Create state with client1 having VTXOs and already submitted.
		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {
					withVTXOs:        true,
					alreadySubmitted: true,
				},
			},
		)
		h := newTestHarness(t, awaitState)

		var signingKey SigningKeyHex
		for _, desc := range awaitState.ClientRegistrations["client1"].
			VTXODescriptors {
			signingKey = route.NewVertex(desc.CoSignerKey)
			break
		}

		// Send duplicate partial sigs from client1.
		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client1",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				signingKey: {},
			},
		})
		require.NoError(t, err)

		// Should remain in same state and send error to client.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "already submitted")
	})

	t.Run("partial key submission rejected", func(t *testing.T) {
		t.Parallel()

		// Create client with multiple signing keys.
		key1, _ := testutils.CreateKey(100)
		key2, _ := testutils.CreateKey(101)
		keyHex1 := route.NewVertex(key1)
		keyHex2 := route.NewVertex(key2)

		awaitState := &AwaitingVTXOSignaturesState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{ //nolint:ll
						keyHex1: {CoSignerKey: key1},
						keyHex2: {CoSignerKey: key2},
					},
				},
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees:             map[int]*tree.Tree{},
			TreeSignCoordinators:  map[int]*batch.TreeSignCoordinator{}, //nolint:ll
			ClientsWithSignatures: make(map[ClientID]struct{}),
		}

		h := newTestHarness(t, awaitState)

		// Submit signatures for only one of the two keys.
		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client1",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				keyHex1: {},
			},
		})
		require.NoError(t, err)

		// Should reject and remain in same state.
		assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "missing signatures")
		require.Contains(t, errResp.ErrorMsg, "signing key")
	})

	t.Run("empty signatures rejected", func(t *testing.T) {
		t.Parallel()

		awaitState := buildAwaitingVTXOSignaturesState(
			map[ClientID]vtxoNoncesStateOpts{
				"client1": {withVTXOs: true},
			},
		)
		h := newTestHarness(t, awaitState)

		var signingKey SigningKeyHex
		for _, desc := range awaitState.ClientRegistrations["client1"].
			VTXODescriptors {
			signingKey = route.NewVertex(desc.CoSignerKey)
			break
		}

		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client1",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				signingKey: {},
			},
		})
		require.NoError(t, err)

		state := assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(
			t, errResp.ErrorMsg, "no signatures for signing key",
		)
		require.Empty(t, state.ClientsWithSignatures)
	})

	t.Run("empty signatures for key rejected", func(t *testing.T) {
		t.Parallel()

		key1, _ := testutils.CreateKey(300)
		key2, _ := testutils.CreateKey(301)
		keyHex1 := route.NewVertex(key1)
		keyHex2 := route.NewVertex(key2)

		awaitState := &AwaitingVTXOSignaturesState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": {
					ClientID: "client1",
					VTXODescriptors: map[SigningKeyHex]*tree.VTXODescriptor{ //nolint:ll
						keyHex1: {CoSignerKey: key1},
						keyHex2: {CoSignerKey: key2},
					},
				},
			},
			PSBT: &psbt.Packet{
				UnsignedTx: wire.NewMsgTx(2),
			},
			VTXOTrees:            map[int]*tree.Tree{},
			TreeSignCoordinators: map[int]*batch.TreeSignCoordinator{}, //nolint:ll
			ClientsWithSignatures: make(
				map[ClientID]struct{},
			),
		}

		h := newTestHarness(t, awaitState)

		err := h.sendEvent(&ClientVTXOPartialSigsEvent{
			ClientID: "client1",
			Signatures: map[SigningKeyHex]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				keyHex1: {},
				keyHex2: {},
			},
		})
		require.NoError(t, err)

		state := assertStateType[*AwaitingVTXOSignaturesState](h)
		h.assertOutboxLen(1)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(
			t, errResp.ErrorMsg, "no signatures for signing key",
		)
		require.Empty(t, state.ClientsWithSignatures)
	})
}
