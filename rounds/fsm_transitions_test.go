package rounds

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent. The FSM now delegates
		// validation to the OutboxHandler, so we feed a failure
		// event to simulate validation rejection.
		joinReqEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{
						Outpoint: &wire.OutPoint{
							Hash: chainhash.HashH(
								[]byte("bad"),
							),
							Index: 0,
						},
					},
				},
			},
		}
		feedJoinFailure(
			h, joinReqEvent,
			ErrJoinRequestInvalid.Error()+": boarding "+
				"input already locked",
		)

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

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("good")),
			Index: 0,
		}

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent. The FSM emits a
		// ValidateAndLockJoinReq outbox and transitions to
		// AwaitingJoinValidationState.
		joinReqEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &outpoint},
				},
			},
		}
		result := buildJoinResult(
			&BoardingInput{Outpoint: &outpoint},
		)
		feedJoinSuccess(h, "client1", joinReqEvent, result)

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

		// Create outpoints.
		boardingOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding")),
			Index: 0,
		}
		forfeitOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("forfeit")),
			Index: 0,
		}

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent with both boarding and
		// forfeit. The FSM delegates to the OutboxHandler, so we
		// feed a success event with pre-built result.
		joinReqEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
			},
		}
		result := buildJoinResultWithForfeits(
			[]*BoardingInput{
				{Outpoint: &boardingOutpoint},
			},
			[]*ForfeitInput{
				{Outpoint: &forfeitOutpoint},
			},
		)
		feedJoinSuccess(h, "client1", joinReqEvent, result)

		// Assert we transitioned to RegistrationState.
		regState := assertStateType[*RegistrationState](h)
		require.Len(t, regState.getAllBoardingInputs(), 1)

		// Verify that the forfeit input is stored in the
		// ClientRegistration.
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
		h.setupPermissiveMocks()

		// Assert the initial state is CreatedState.
		assertStateType[*CreatedState](h)

		// Send a ClientJoinRequestEvent with both boarding and
		// forfeit. The FSM delegates to the OutboxHandler, so we
		// feed a failure event to simulate the lock failure.
		boardingOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding")),
			Index: 0,
		}
		forfeitOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("forfeit")),
			Index: 0,
		}
		joinReqEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
			},
		}
		feedJoinFailure(
			h, joinReqEvent, "failed to lock forfeit VTXOs: "+
				"VTXO already locked",
		)

		// Assert we remain in CreatedState due to lock failure.
		assertStateType[*CreatedState](h)

		// Assert that we have the expected outbox message (client
		// error).
		h.assertOutboxLen(1)
		msg := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Contains(t, msg.ErrorMsg, "failed to lock forfeit VTXO")
	})
}

// TestValidateJoinRequestForAdmissionRequiresHeight asserts join-auth
// validation fails fast when no validation height is available.
func TestValidateJoinRequestForAdmissionRequiresHeight(t *testing.T) {
	t.Parallel()

	env := &Environment{
		DisableJoinRequestAuth: false,
		StartHeight:            0,
	}

	_, err := validateJoinRequestForAdmission(
		t.Context(), env, &types.JoinRoundRequest{}, 0,
	)
	require.ErrorIs(t, err, ErrJoinAuthHeightUnavailable)
}

// TestFSMRegistrationState tests the FSM transitions from RegistrationState.
func TestFSMRegistrationState(t *testing.T) {
	t.Parallel()

	t.Run("second client joins successfully", func(t *testing.T) {
		t.Parallel()

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
		h.setupPermissiveMocks()

		// Second client joins via the two-hop outbox pattern.
		joinEvent := &ClientJoinRequestEvent{
			ClientID: "client2",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &outpoint2},
				},
			},
		}
		result := buildJoinResult(
			&BoardingInput{Outpoint: &outpoint2},
		)
		feedJoinSuccess(h, "client2", joinEvent, result)

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
			h.setupPermissiveMocks()

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

			// First client joins successfully from CreatedState
			// via the two-hop outbox pattern.
			joinEvent1 := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request: &types.JoinRoundRequest{
					BoardingReqs: []*types.BoardingRequest{
						{Outpoint: &outpoint1},
					},
				},
			}
			result1 := buildJoinResult(
				&BoardingInput{Outpoint: &outpoint1},
			)
			feedJoinSuccess(
				h, "client1", joinEvent1, result1,
			)

			// Assert we transitioned to RegistrationState with
			// client1.
			regState := assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 1)
			require.True(t, regState.isClientRegistered("client1"))

			// Second client attempts to join but lock fails.
			// Feed a failure event.
			joinEvent2 := &ClientJoinRequestEvent{
				ClientID: "client2",
				Request: &types.JoinRoundRequest{
					BoardingReqs: []*types.BoardingRequest{
						{Outpoint: &outpoint2},
					},
				},
			}
			feedJoinFailure(
				h, joinEvent2,
				"failed to lock boarding input",
			)

			// Assert we remain in RegistrationState with only
			// client1.
			regState = assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 1)
			require.True(t, regState.isClientRegistered("client1"))
			require.False(
				t, regState.isClientRegistered("client2"),
			)

			// Assert client2 received an error response.
			h.assertOutboxLen(1)
			errorResp := assertOutboxMessageType[*ClientErrorResp](
				h, 0,
			)
			require.Equal(t, "client2", string(errorResp.Client))
			require.Contains(
				t, errorResp.ErrorMsg, "failed to lock",
			)

			// Third client joins successfully, proving the FSM
			// is still functional after the lock failure.
			joinEvent3 := &ClientJoinRequestEvent{
				ClientID: "client3",
				Request: &types.JoinRoundRequest{
					BoardingReqs: []*types.BoardingRequest{
						{Outpoint: &outpoint3},
					},
				},
			}
			result3 := buildJoinResult(
				&BoardingInput{Outpoint: &outpoint3},
			)
			feedJoinSuccess(
				h, "client3", joinEvent3, result3,
			)

			// Assert we remain in RegistrationState with client1
			// and client3 (client2 was rejected).
			regState = assertStateType[*RegistrationState](h)
			require.Len(t, regState.getAllBoardingInputs(), 2)
			require.True(t, regState.isClientRegistered("client1"))
			require.False(
				t, regState.isClientRegistered("client2"),
			)
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

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		// Join to get to RegistrationState via outbox pattern.
		joinEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &outpoint},
				},
			},
		}
		result := buildJoinResult(
			&BoardingInput{Outpoint: &outpoint},
		)
		feedJoinSuccess(h, "client1", joinEvent, result)

		// Assert we're in RegistrationState.
		assertStateType[*RegistrationState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Send RegistrationTimeoutEvent.
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Verify RoundSealedReq emitted before batch build clears
		// the outbox.
		assertOutboxContains[*RoundSealedReq](h)

		// Feed batch build success to advance through batch building.
		feedBatchBuildSuccess(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Verify the batch was built correctly.
		require.NotNil(t, awaitState.PSBT)
		require.Len(t, awaitState.ClientRegistrations, 1)
		require.Contains(
			t, awaitState.ClientRegistrations, ClientID("client1"),
		)

		// Verify locked outpoints are propagated.
		require.Equal(
			t, testLockedOutpoints, awaitState.LockedOutpoints,
		)

		// Verify outbox messages from BatchBuiltState's
		// PrepareClientNotificationsEvent.
		var (
			foundBatchInfo        bool
			foundAwaitingBrdgSigs bool
			foundTimeoutReq       bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
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

		// Get deterministic operator key (same as harness uses).
		operatorPub, _ := testutils.CreateKey(1)

		// Create RegistrationState with two clients already registered.
		outpoint1 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}
		outpoint2 := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input2")),
			Index: 0,
		}

		// Use fully populated boarding inputs for batch building.
		bi1 := buildTestBoardingInput(
			t, &outpoint1, 100_000, operatorPub,
		)
		bi2 := buildTestBoardingInput(
			t, &outpoint2, 100_000, operatorPub,
		)

		client1Reg := buildTestClientRegistration("client1", bi1)
		client2Reg := buildTestClientRegistration("client2", bi2)
		regState := &RegistrationState{
			ClientRegistrations: map[ClientID]*ClientRegistration{
				"client1": client1Reg,
				"client2": client2Reg,
			},
		}

		h := newTestHarness(t, regState)

		// Seal via manual SealEvent.
		err := h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Feed batch build success to advance.
		feedBatchBuildSuccess(h)

		// Should transition to AwaitingInputSigsState.
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
			h.setupPermissiveMocks()

			const exitDelay = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, 144,
			)

			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit")),
				Index: 0,
			}

			// Build a real boarding input for batch building.
			bi := buildTestBoardingInput(
				t, &boardingOutpoint, 100_000,
				h.operatorPub,
			)
			fi := &ForfeitInput{
				Outpoint: &forfeitOutpoint,
			}

			// Join via outbox pattern with pre-built result.
			req := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
			}
			joinReq := &ClientJoinRequestEvent{
				ClientID: client.clientID,
				Request:  req,
			}
			result := buildJoinResultWithForfeits(
				[]*BoardingInput{bi},
				[]*ForfeitInput{fi},
			)
			feedJoinSuccess(
				h, client.clientID, joinReq, result,
			)
			assertStateType[*RegistrationState](h)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			feedBatchBuildViaHandler(h)

			assertStateType[*AwaitingInputSigsState](h)
		})

	t.Run("round with forfeits builds connector outputs",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			// Only set boarding locker permissive — custom
			// FundPsbt with change output is needed below.
			h.boardingLocker.On("Lock", mock.Anything,
				mock.Anything, mock.Anything,
			).Return(nil).Maybe()
			h.boardingLocker.On("Unlock", mock.Anything,
				mock.Anything, mock.Anything,
			).Return(nil).Maybe()
			h.boardingLocker.On("IsLocked", mock.Anything,
				mock.Anything,
			).Return(false, RoundID{}, nil).Maybe()

			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit-vtxo")),
				Index: 0,
			}

			h.feeEstimator.On(
				"EstimateFeePerKW", uint32(6),
			).Return(
				chainfee.SatPerKWeight(1000), nil,
			).Once()
			h.walletController.On("FundPsbt",
				mock.Anything, mock.Anything,
				mock.Anything, mock.Anything,
				mock.Anything, mock.Anything).
				Run(func(args mock.Arguments) {
					p, ok := args.Get(1).(*psbt.Packet)
					require.True(t, ok)

					changeScript := []byte{
						0x00, 0x14, 0xaa,
					}
					changeOut := &wire.TxOut{
						Value:    1000,
						PkScript: changeScript,
					}
					p.UnsignedTx.TxOut = append(
						[]*wire.TxOut{changeOut},
						p.UnsignedTx.TxOut...,
					)
					emptyOutputs := []psbt.POutput{{}}
					p.Outputs = append(
						emptyOutputs, p.Outputs...,
					)
				}).
				Return(
					int32(0), testLockedOutpoints, nil,
				).Once()

			leaveScript := []byte{0x00, 0x14, 0x01, 0x02}
			leaveOutput := &wire.TxOut{
				Value:    30000,
				PkScript: leaveScript,
			}

			// Build join result with forfeit + leave.
			fi := &ForfeitInput{Outpoint: &forfeitOutpoint}
			joinResult := &JoinRequestResult{
				ForfeitInputs: []*ForfeitInput{fi},
				RequiredOutputs: []*wire.TxOut{
					leaveOutput,
				},
			}
			req := &types.JoinRoundRequest{
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
				LeaveReqs: []*types.LeaveRequest{
					{Output: leaveOutput},
				},
			}
			joinReq := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request:  req,
			}
			feedJoinSuccess(
				h, "client1", joinReq, joinResult,
			)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			// Run the BuildBatchReq through the handler to
			// produce real connector assignments.
			feedBatchBuildViaHandler(h)

			awaitState :=
				assertStateType[*AwaitingInputSigsState](h)
			require.NotNil(t, awaitState.PSBT)

			assignment, ok :=
				awaitState.ConnectorAssignments[forfeitOutpoint]
			require.True(t, ok)

			outputIdx := assignment.ConnectorOutputIndex
			connectorOutput :=
				awaitState.PSBT.UnsignedTx.TxOut[outputIdx]
			expectedScript, err := txscript.PayToAddrScript(
				h.env.Terms.ConnectorAddress,
			)
			require.NoError(t, err)

			require.Equal(t, expectedScript,
				connectorOutput.PkScript)
			require.Equal(t,
				int64(h.env.Terms.ConnectorDustAmount),
				connectorOutput.Value,
			)
		})

	t.Run("multiple forfeits split across connector trees",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupPermissiveMocks()
			h.env.Terms.MaxConnectorsPerTree = 2

			forfeitOutpoints := []wire.OutPoint{
				{Hash: chainhash.HashH([]byte("forfeit-a"))},
				{Hash: chainhash.HashH([]byte("forfeit-b"))},
				{Hash: chainhash.HashH([]byte("forfeit-c"))},
			}
			var (
				forfeitReqs   []*types.ForfeitRequest
				forfeitInputs []*ForfeitInput
			)
			for i := range forfeitOutpoints {
				outpoint := &forfeitOutpoints[i]
				forfeitReqs = append(forfeitReqs,
					&types.ForfeitRequest{
						VTXOOutpoint: outpoint,
					},
				)
				forfeitInputs = append(forfeitInputs,
					&ForfeitInput{Outpoint: outpoint},
				)
			}

			joinReq := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request: &types.JoinRoundRequest{
					ForfeitReqs: forfeitReqs,
				},
			}
			joinResult := buildJoinResultWithForfeits(
				nil, forfeitInputs,
			)
			feedJoinSuccess(
				h, "client1", joinReq, joinResult,
			)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			// Verify the FSM emits a BuildBatchReq with
			// the forfeit inputs.
			assertStateType[*AwaitingBatchBuildState](h)
			buildReq :=
				assertOutboxContains[*BuildBatchReq](h)
			require.Len(t, buildReq.ForfeitInputs, 3)

			feedBatchBuildViaHandler(h)
			assertStateType[*AwaitingInputSigsState](h)
		})

	t.Run("VTXO rounds also include connector outputs",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupPermissiveMocks()

			const exitDelay = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, 144,
			)

			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit")),
				Index: 0,
			}

			bi := buildTestBoardingInput(
				t, &boardingOutpoint, 100_000,
				h.operatorPub,
			)
			fi := &ForfeitInput{Outpoint: &forfeitOutpoint}

			// Build a VTXO descriptor to put in the join
			// result so that BuildBatchReq will contain
			// VTXODescriptors.
			desc, err := tree.NewVTXODescriptor(
				50000, client.boardingKey,
				h.operatorPub, exitDelay,
			)
			require.NoError(t, err)
			keyHex := route.NewVertex(client.boardingKey)

			vtxoDescs := map[SigningKeyHex]*tree.VTXODescriptor{
				keyHex: desc,
			}
			joinResult := &JoinRequestResult{
				BoardingInputs:  []*BoardingInput{bi},
				ForfeitInputs:   []*ForfeitInput{fi},
				VTXODescriptors: vtxoDescs,
			}
			req := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
			}
			joinReq := &ClientJoinRequestEvent{
				ClientID: client.clientID,
				Request:  req,
			}
			feedJoinSuccess(
				h, client.clientID, joinReq, joinResult,
			)

			h.outboxMessages = nil
			err = h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			// Verify outbox carries the build request.
			assertStateType[*AwaitingBatchBuildState](h)
			buildReq :=
				assertOutboxContains[*BuildBatchReq](h)
			require.NotEmpty(t, buildReq.VTXODescriptors)
			require.NotEmpty(t, buildReq.ForfeitInputs)

			feedBatchBuildViaHandler(h)

			// With VTXOs, the batch goes to nonce collection
			// first.
			assertStateType[*AwaitingVTXONoncesState](h)
		})

	t.Run("two clients receive connector leaves",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupPermissiveMocks()

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

			bi1 := buildTestBoardingInput(
				t, &boardingOutpoint1, 100_000,
				h.operatorPub,
			)
			bi2 := buildTestBoardingInput(
				t, &boardingOutpoint2, 100_000,
				h.operatorPub,
			)

			// Client 1 joins.
			req1 := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint1},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint1},
				},
			}
			joinReq1 := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request:  req1,
			}
			feedJoinSuccess(h, "client1", joinReq1,
				buildJoinResultWithForfeits(
					[]*BoardingInput{bi1},
					[]*ForfeitInput{
						{Outpoint: &forfeitOutpoint1},
					},
				),
			)

			// Client 2 joins.
			req2 := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint2},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint2},
				},
			}
			joinReq2 := &ClientJoinRequestEvent{
				ClientID: "client2",
				Request:  req2,
			}
			feedJoinSuccess(h, "client2", joinReq2,
				buildJoinResultWithForfeits(
					[]*BoardingInput{bi2},
					[]*ForfeitInput{
						{Outpoint: &forfeitOutpoint2},
					},
				),
			)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			// Verify the FSM emits a BuildBatchReq with
			// both clients' forfeit inputs.
			assertStateType[*AwaitingBatchBuildState](h)
			buildReq :=
				assertOutboxContains[*BuildBatchReq](h)
			require.Len(t, buildReq.ForfeitInputs, 2)

			feedBatchBuildViaHandler(h)
			assertStateType[*AwaitingInputSigsState](h)
		})

	t.Run("single client gets multiple connector leaves",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupPermissiveMocks()

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

			bi := buildTestBoardingInput(
				t, &boardingOutpoint, 100_000,
				h.operatorPub,
			)
			req := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint1},
					{VTXOOutpoint: &forfeitOutpoint2},
				},
			}
			joinReq := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request:  req,
			}
			feedJoinSuccess(h, "client1", joinReq,
				buildJoinResultWithForfeits(
					[]*BoardingInput{bi},
					[]*ForfeitInput{
						{Outpoint: &forfeitOutpoint1},
						{Outpoint: &forfeitOutpoint2},
					},
				),
			)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			assertStateType[*AwaitingBatchBuildState](h)
			buildReq :=
				assertOutboxContains[*BuildBatchReq](h)
			require.Len(t, buildReq.ForfeitInputs, 2)

			feedBatchBuildViaHandler(h)
			assertStateType[*AwaitingInputSigsState](h)
		})

	t.Run("batch building captures forfeit connectors",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

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
					ClientID:      "client1",
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

			// After extraction, BatchBuildingState emits a
			// BuildBatchReq and transitions to
			// AwaitingBatchBuildState.
			nextState, ok :=
				transition.NextState.(*AwaitingBatchBuildState)
			require.True(t, ok)
			require.Len(
				t, nextState.ClientRegistrations, 1,
			)

			// Verify the outbox carries the build request
			// with both forfeit inputs.
			outbox := transition.NewEvents.UnwrapOr(
				EmittedEvent{},
			)
			require.Len(t, outbox.Outbox, 1)
			buildReq, ok :=
				outbox.Outbox[0].(*BuildBatchReq)
			require.True(t, ok)
			require.Len(
				t, buildReq.ForfeitInputs,
				len(forfeitInputs),
			)
		},
	)

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
				CollectedForfeitTxs: ForfeitTxsMap{},
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
		h.setupPermissiveMocks()

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		// Join via outbox pattern.
		joinEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &outpoint},
				},
			},
		}
		feedJoinSuccess(h, "client1", joinEvent,
			buildJoinResult(&BoardingInput{Outpoint: &outpoint}),
		)

		// Assert we're in RegistrationState.
		assertStateType[*RegistrationState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Seal the round to trigger batch building.
		err := h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		// Should be in AwaitingBatchBuildState.
		assertStateType[*AwaitingBatchBuildState](h)

		// Feed batch build failure.
		h.outboxMessages = nil
		err = h.sendEvent(&BuildBatchFailedEvent{
			Reason: "build commitment tx: insufficient funds",
		})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "insufficient funds")

		// Verify outbox messages:
		// 1. ClientRoundFailedResp for client1
		// 2. UnlockBoardingInputsReq
		// 3. UnlockForfeitVTXOsReq
		// 4. RoundFailedReq for the actor
		// NO ReleaseWalletInputsReq (pre-batch failure).
		var (
			foundClientFailed  bool
			foundRoundFailed   bool
			foundUnlockBI      bool
			foundUnlockVTXO    bool
			foundReleaseWallet bool
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
				foundUnlockBI = true
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *UnlockForfeitVTXOsReq:
				foundUnlockVTXO = true
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *ReleaseWalletInputsReq:
				foundReleaseWallet = true

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
				require.Contains(t, m.Reason, "insufficient "+
					"funds")
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundUnlockBI,
			"boarding inputs should be unlocked via outbox")
		require.True(t, foundUnlockVTXO,
			"forfeit VTXOs should be unlocked via outbox")
		require.False(t, foundReleaseWallet,
			"pre-batch failure should not release wallet inputs")
		require.True(t, foundRoundFailed, "actor should be notified")
	})

	t.Run("forfeit VTXOs unlocked on batch building failure",
		func(t *testing.T) {
			t.Parallel()

			// Set up the test harness.
			h := newTestHarness(t)
			h.setupPermissiveMocks()

			boardingOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("boarding")),
				Index: 0,
			}
			forfeitOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("forfeit")),
				Index: 0,
			}

			// Join with both boarding and forfeit via outbox.
			req := &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &boardingOutpoint},
				},
				ForfeitReqs: []*types.ForfeitRequest{
					{VTXOOutpoint: &forfeitOutpoint},
				},
			}
			joinReq := &ClientJoinRequestEvent{
				ClientID: "client1",
				Request:  req,
			}
			feedJoinSuccess(h, "client1", joinReq,
				buildJoinResultWithForfeits(
					[]*BoardingInput{
						{Outpoint: &boardingOutpoint},
					},
					[]*ForfeitInput{
						{Outpoint: &forfeitOutpoint},
					},
				),
			)

			// Assert we're in RegistrationState.
			assertStateType[*RegistrationState](h)

			// Clear outbox.
			h.outboxMessages = nil

			// Seal the round to trigger batch building.
			err := h.sendEvent(&SealEvent{})
			require.NoError(t, err)

			// Should be in AwaitingBatchBuildState.
			assertStateType[*AwaitingBatchBuildState](h)

			// Feed batch build failure.
			h.outboxMessages = nil
			err = h.sendEvent(&BuildBatchFailedEvent{
				Reason: "build commitment tx: " +
					"insufficient funds",
			})
			require.NoError(t, err)

			// Should transition to FailedState.
			failedState := assertStateType[*FailedState](h)
			require.Contains(
				t, failedState.Reason, "insufficient funds",
			)

			// Verify outbox messages include unlock events.
			var (
				foundClientFailed bool
				foundRoundFailed  bool
				foundUnlockBI     bool
				foundUnlockVTXO   bool
			)
			for _, msg := range h.outboxMessages {
				switch m := msg.(type) {
				case *ClientRoundFailedResp:
					foundClientFailed = true
					require.Equal(
						t, ClientID("client1"),
						m.Client,
					)

				case *UnlockBoardingInputsReq:
					foundUnlockBI = true
					require.Equal(
						t, h.env.RoundID, m.RoundID,
					)

				case *UnlockForfeitVTXOsReq:
					foundUnlockVTXO = true
					require.Equal(
						t, h.env.RoundID, m.RoundID,
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
				t, foundUnlockBI,
				"boarding inputs should be unlocked",
			)
			require.True(
				t, foundUnlockVTXO,
				"forfeit VTXOs should be unlocked",
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
		h.setupPermissiveMocks()

		outpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("input1")),
			Index: 0,
		}

		// Join via outbox pattern.
		joinEvent := &ClientJoinRequestEvent{
			ClientID: "client1",
			Request: &types.JoinRoundRequest{
				BoardingReqs: []*types.BoardingRequest{
					{Outpoint: &outpoint},
				},
			},
		}
		feedJoinSuccess(h, "client1", joinEvent,
			buildJoinResult(&BoardingInput{Outpoint: &outpoint}),
		)

		// Seal via RegistrationTimeoutEvent.
		h.outboxMessages = nil
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		// Feed batch build success to advance past batch building.
		feedBatchBuildSuccess(h)

		// Should be in AwaitingInputSigsState.
		assertStateType[*AwaitingInputSigsState](h)

		// Clear outbox.
		h.outboxMessages = nil

		// Send InputSignaturesTimeoutEvent.
		err = h.sendEvent(&InputSignaturesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages include unlock events and
		// wallet input release.
		expectedLockID := roundLockID(h.env.RoundID)
		var (
			foundClientFailed  bool
			foundRoundFailed   bool
			foundUnlockBI      bool
			foundReleaseWallet bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)

			case *UnlockBoardingInputsReq:
				foundUnlockBI = true
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *ReleaseWalletInputsReq:
				foundReleaseWallet = true
				require.Equal(
					t, expectedLockID, m.LockID,
				)
				require.Equal(
					t, testLockedOutpoints,
					m.LockedOutpoints,
				)

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
			}
		}
		require.True(t, foundClientFailed, "client notified of failure")
		require.True(t, foundUnlockBI,
			"boarding inputs should be unlocked via outbox")
		require.True(t, foundReleaseWallet,
			"wallet inputs should be released via outbox")
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

		h.setupPermissiveMocks()

		// Build a real boarding input for batch building.
		bi := buildTestBoardingInputForClient(
			t, &outpoint, 100_000, client.boardingKey,
			h.operatorPub, exitDelay,
		)

		// Join via outbox pattern.
		boardingReq := client.createBoardingRequest(&outpoint)
		joinReqEvent := client.createJoinRequest(
			[]*types.BoardingRequest{boardingReq},
		)
		feedJoinSuccess(
			h, client.clientID, joinReqEvent,
			buildJoinResult(bi),
		)

		// Seal and run the batch build through the handler to
		// produce a real PSBT needed for signature creation.
		h.outboxMessages = nil
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		feedBatchBuildViaHandler(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)
		require.NotNil(t, awaitState.PSBT)
		require.NotNil(t, awaitState.CollectedForfeitTxs)

		// Submit boarding signatures.
		h.outboxMessages = nil
		sigEvent := client.createInputSignaturesEvent(awaitState)
		err = h.sendEvent(sigEvent)
		require.NoError(t, err)

		// Should transition through ServerSigningState to
		// AwaitingSignAndFinalizeState with a signing outbox
		// request.
		assertStateType[*AwaitingSignAndFinalizeState](h)
		assertOutboxContains[*SignAndFinalizeRoundReq](h)

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

		// Feed signing success to move to persistence.
		finalTx := wire.NewMsgTx(2)
		h.outboxMessages = nil
		err = h.sendEvent(&SignAndFinalizeSucceededEvent{
			FinalTx:      finalTx,
			ForfeitInfos: make(map[wire.OutPoint]*ForfeitInfo),
		})
		require.NoError(t, err)

		assertStateType[*AwaitingServerSignPersistState](h)
		assertOutboxContains[*PersistServerSigningReq](h)

		// Feed persistence success to complete the transition.
		h.outboxMessages = nil
		err = h.sendEvent(&PersistServerSigningSucceededEvent{})
		require.NoError(t, err)

		finalState := assertStateType[*FinalizedState](h)
		require.NotNil(t, finalState.FinalTx)
		require.Len(t, finalState.ClientRegistrations, 1)
		assertOutboxContains[*BroadcastRoundReq](h)
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

		h.setupPermissiveMocks()

		// Build real boarding inputs for batch building.
		bi1 := buildTestBoardingInputForClient(
			t, &outpoint1, 100_000, client1.boardingKey,
			h.operatorPub, exitDelay,
		)
		bi2 := buildTestBoardingInputForClient(
			t, &outpoint2, 100_000, client2.boardingKey,
			h.operatorPub, exitDelay,
		)

		// Both clients join via outbox pattern.
		boardingReq1 := client1.createBoardingRequest(&outpoint1)
		feedJoinSuccess(
			h, client1.clientID,
			client1.createJoinRequest(
				[]*types.BoardingRequest{boardingReq1},
			),
			buildJoinResult(bi1),
		)

		boardingReq2 := client2.createBoardingRequest(&outpoint2)
		feedJoinSuccess(
			h, client2.clientID,
			client2.createJoinRequest(
				[]*types.BoardingRequest{boardingReq2},
			),
			buildJoinResult(bi2),
		)

		// Seal and run batch build through handler.
		h.outboxMessages = nil
		err := h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		feedBatchBuildViaHandler(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)
		require.Empty(t, awaitState.ClientsSubmitted)
		require.NotNil(t, awaitState.CollectedForfeitTxs)

		// Client1 submits - should remain in AwaitingInputSigsState.
		h.outboxMessages = nil
		sig1Event := client1.createInputSignaturesEvent(awaitState)
		err = h.sendEvent(sig1Event)
		require.NoError(t, err)

		awaitState = assertStateType[*AwaitingInputSigsState](h)
		require.Len(t, awaitState.ClientsSubmitted, 1)
		require.True(t, awaitState.hasClientSubmitted("client1"))
		require.False(t, awaitState.hasClientSubmitted("client2"))
		_, hasClient1 := awaitState.CollectedForfeitTxs["client1"]
		require.True(t, hasClient1)

		// No outbox messages yet (no transition).
		h.assertOutboxLen(0)

		// Client2 submits - should transition through
		// ServerSigningState to AwaitingSignAndFinalizeState.
		sig2Event := client2.createInputSignaturesEvent(awaitState)
		err = h.sendEvent(sig2Event)
		require.NoError(t, err)

		assertStateType[*AwaitingSignAndFinalizeState](h)
		assertOutboxContains[*SignAndFinalizeRoundReq](h)

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

		// Feed signing success.
		h.outboxMessages = nil
		err = h.sendEvent(&SignAndFinalizeSucceededEvent{
			FinalTx:      wire.NewMsgTx(2),
			ForfeitInfos: make(map[wire.OutPoint]*ForfeitInfo),
		})
		require.NoError(t, err)

		assertStateType[*AwaitingServerSignPersistState](h)
		assertOutboxContains[*PersistServerSigningReq](h)

		// Feed persistence success.
		h.outboxMessages = nil
		err = h.sendEvent(&PersistServerSigningSucceededEvent{})
		require.NoError(t, err)

		finalState := assertStateType[*FinalizedState](h)
		require.Len(t, finalState.ClientRegistrations, 2)
		assertOutboxContains[*BroadcastRoundReq](h)
	})

	t.Run("server signing state carries forfeit txs map",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			const exitDelay = 144
			const expiry = 144
			client := newClientHarness(
				t, "client1", 10, h.operatorPub,
				exitDelay, expiry,
			)

			outpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("input1")),
				Index: 0,
			}

			h.setupPermissiveMocks()

			bi := buildTestBoardingInputForClient(
				t, &outpoint, 100_000,
				client.boardingKey, h.operatorPub,
				exitDelay,
			)

			boardingReq := client.createBoardingRequest(
				&outpoint,
			)
			joinReqEvent := client.createJoinRequest(
				[]*types.BoardingRequest{boardingReq},
			)
			feedJoinSuccess(
				h, client.clientID, joinReqEvent,
				buildJoinResult(bi),
			)

			h.outboxMessages = nil
			err := h.sendEvent(&RegistrationTimeoutEvent{})
			require.NoError(t, err)

			feedBatchBuildViaHandler(h)

			awaitState :=
				assertStateType[*AwaitingInputSigsState](h)
			sigEvent :=
				client.createInputSignaturesEvent(awaitState)

			transition, err := awaitState.handleInputSignatures(
				t.Context(), sigEvent, h.env,
			)
			require.NoError(t, err)

			nextState, ok :=
				transition.NextState.(*ServerSigningState)
			require.True(t, ok)
			require.NotNil(t, nextState.CollectedForfeitTxs)
			_, hasClient := nextState.CollectedForfeitTxs["client1"]
			require.True(t, hasClient)
		})

	t.Run("unknown client rejected", func(t *testing.T) {
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

		bi := buildTestBoardingInputForClient(
			t, &outpoint, 100_000, client.boardingKey,
			h.operatorPub, exitDelay,
		)

		// Join and seal via outbox pattern.
		boardingReq := client.createBoardingRequest(&outpoint)
		feedJoinSuccess(
			h, client.clientID,
			client.createJoinRequest(
				[]*types.BoardingRequest{boardingReq},
			),
			buildJoinResult(bi),
		)

		h.outboxMessages = nil
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		feedBatchBuildViaHandler(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Unknown client tries to submit.
		h.outboxMessages = nil
		unknownSigEvent := &ClientInputSignaturesEvent{
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
		sigEvent := client.createInputSignaturesEvent(awaitState)
		err = h.sendEvent(sigEvent)
		require.NoError(t, err)

		// Should be in AwaitingSignAndFinalizeState.
		assertStateType[*AwaitingSignAndFinalizeState](h)
		assertOutboxContains[*SignAndFinalizeRoundReq](h)

		// Feed signing success.
		h.outboxMessages = nil
		err = h.sendEvent(&SignAndFinalizeSucceededEvent{
			FinalTx:      wire.NewMsgTx(2),
			ForfeitInfos: make(map[wire.OutPoint]*ForfeitInfo),
		})
		require.NoError(t, err)

		assertStateType[*AwaitingServerSignPersistState](h)
		assertOutboxContains[*PersistServerSigningReq](h)

		// Feed persistence success.
		h.outboxMessages = nil
		err = h.sendEvent(&PersistServerSigningSucceededEvent{})
		require.NoError(t, err)

		finalState := assertStateType[*FinalizedState](h)
		require.Len(t, finalState.ClientRegistrations, 1)
	})

	t.Run("missing forfeit txs rejected", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupPermissiveMocks()

		const exitDelay = 144
		const expiry = 144
		client := newClientHarness(
			t, "client1", 10, h.operatorPub, exitDelay, expiry,
		)

		boardingOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("boarding")),
			Index: 0,
		}
		forfeitOutpoint := wire.OutPoint{
			Hash:  chainhash.HashH([]byte("forfeit")),
			Index: 0,
		}

		bi := buildTestBoardingInputForClient(
			t, &boardingOutpoint, 100_000,
			client.boardingKey, h.operatorPub, exitDelay,
		)
		fi := &ForfeitInput{Outpoint: &forfeitOutpoint}

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
		feedJoinSuccess(
			h, client.clientID, joinReqEvent,
			buildJoinResultWithForfeits(
				[]*BoardingInput{bi},
				[]*ForfeitInput{fi},
			),
		)

		h.outboxMessages = nil
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		feedBatchBuildViaHandler(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)
		sigEvent := client.createInputSignaturesEvent(awaitState)
		sigEvent.ForfeitTxs = nil

		h.outboxMessages = nil
		err = h.sendEvent(sigEvent)
		require.NoError(t, err)

		assertStateType[*AwaitingInputSigsState](h)
		errResp := assertOutboxMessageType[*ClientErrorResp](h, 0)
		require.Equal(t, ClientID("client1"), errResp.Client)
		require.Contains(t, errResp.ErrorMsg, "expected 1 forfeit txs")
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

		h.setupPermissiveMocks()

		// Build real boarding inputs for batch building.
		bi1 := buildTestBoardingInputForClient(
			t, &outpoint1, 100_000, client1.boardingKey,
			h.operatorPub, exitDelay,
		)
		bi2 := buildTestBoardingInputForClient(
			t, &outpoint2, 100_000, client2.boardingKey,
			h.operatorPub, exitDelay,
		)

		// Both clients join via outbox pattern.
		feedJoinSuccess(
			h, client1.clientID,
			client1.createJoinRequest(
				[]*types.BoardingRequest{
					client1.createBoardingRequest(
						&outpoint1,
					),
				},
			),
			buildJoinResult(bi1),
		)
		feedJoinSuccess(
			h, client2.clientID,
			client2.createJoinRequest(
				[]*types.BoardingRequest{
					client2.createBoardingRequest(
						&outpoint2,
					),
				},
			),
			buildJoinResult(bi2),
		)

		// Seal and run batch build through handler.
		h.outboxMessages = nil
		err := h.sendEvent(&SealEvent{})
		require.NoError(t, err)

		feedBatchBuildViaHandler(h)

		awaitState := assertStateType[*AwaitingInputSigsState](h)

		// Client1 submits first time - success.
		h.outboxMessages = nil
		sig1Event := client1.createInputSignaturesEvent(awaitState)
		err = h.sendEvent(sig1Event)
		require.NoError(t, err)

		awaitState = assertStateType[*AwaitingInputSigsState](h)
		require.True(t, awaitState.hasClientSubmitted("client1"))

		// Client1 tries to submit again - should be rejected.
		h.outboxMessages = nil
		sig1EventDup := client1.createInputSignaturesEvent(
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

		h.setupPermissiveMocks()

		bi := buildTestBoardingInputForClient(
			t, &outpoint, 100_000, client.boardingKey,
			h.operatorPub, exitDelay,
		)

		// Join and seal via outbox pattern.
		boardingReq := client.createBoardingRequest(&outpoint)
		feedJoinSuccess(
			h, client.clientID,
			client.createJoinRequest(
				[]*types.BoardingRequest{boardingReq},
			),
			buildJoinResult(bi),
		)

		h.outboxMessages = nil
		err := h.sendEvent(&RegistrationTimeoutEvent{})
		require.NoError(t, err)

		feedBatchBuildSuccess(h)

		assertStateType[*AwaitingInputSigsState](h)

		// Submit with no signatures (client has 1 input).
		h.outboxMessages = nil
		badSigEvent := &ClientInputSignaturesEvent{
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

		// Should transition to AwaitingConfirmPersistState with
		// a ConfirmRoundReq outbox event.
		assertStateType[*AwaitingConfirmPersistState](h)
		h.assertOutboxLen(1)
		assertOutboxContains[*ConfirmRoundReq](h)

		// Feed the handler success event to complete the
		// confirmation.
		h.outboxMessages = nil
		err = h.sendEvent(&ConfirmRoundSucceededEvent{})
		require.NoError(t, err)

		confirmedState := assertStateType[*ConfirmedState](h)
		require.Equal(t, int32(100), confirmedState.BlockHeight)
		require.Equal(t, blockHash, confirmedState.BlockHash)
		require.Equal(t, finalTx, confirmedState.FinalTx)
		require.Len(t, confirmedState.ClientRegistrations, 1)
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

		// Send VTXONoncesTimeoutEvent.
		err := h.sendEvent(&VTXONoncesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "VTXO nonce collection")
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages include unlock events.
		var (
			foundClientFailed bool
			foundRoundFailed  bool
			foundUnlockBI     bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.Equal(t, h.env.RoundID, m.RoundID)
				require.Contains(t, m.Reason, "VTXO nonce")

			case *UnlockBoardingInputsReq:
				foundUnlockBI = true
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
				require.Contains(t, m.Reason, "VTXO nonce")
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundUnlockBI,
			"boarding inputs should be unlocked via outbox")
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

	const baseKeyIndex = 10
	const exitDelay = 144
	const expiry = 144
	client := newClientHarness(
		t, "client1", baseKeyIndex, h.operatorPub, exitDelay, expiry,
	)

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("input1")),
		Index: 0,
	}

	h.setupPermissiveMocks()

	bi := buildTestBoardingInputForClient(
		t, &outpoint, 100_000, client.boardingKey,
		h.operatorPub, exitDelay,
	)

	boardingReq := client.createBoardingRequest(&outpoint)
	vtxoReq := client.createVTXORequest(btcutil.Amount(50000))

	feedJoinSuccess(
		h, client.clientID,
		client.createJoinRequestWithVTXOs(
			[]*types.BoardingRequest{boardingReq},
			[]*types.VTXORequest{vtxoReq},
		),
		buildJoinResultWithVTXOs(
			[]*BoardingInput{bi}, client,
		),
	)

	// Seal via timeout, then run the batch build through the handler
	// to produce real VTXO trees needed for the nonce/signature flow.
	h.outboxMessages = nil
	err := h.sendEvent(&RegistrationTimeoutEvent{})
	require.NoError(t, err)

	feedBatchBuildViaHandler(h)

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
	boardingSigEvent := client.createInputSignaturesEvent(
		awaitBoarding,
	)
	err = h.sendEvent(boardingSigEvent)
	require.NoError(t, err)

	// Should be in AwaitingSignAndFinalizeState with a signing
	// request in the outbox.
	assertStateType[*AwaitingSignAndFinalizeState](h)
	assertOutboxContains[*SignAndFinalizeRoundReq](h)

	// Feed signing success.
	finalTx := wire.NewMsgTx(2)
	h.outboxMessages = nil
	err = h.sendEvent(&SignAndFinalizeSucceededEvent{
		FinalTx:      finalTx,
		ForfeitInfos: make(map[wire.OutPoint]*ForfeitInfo),
	})
	require.NoError(t, err)

	assertStateType[*AwaitingServerSignPersistState](h)
	assertOutboxContains[*PersistServerSigningReq](h)

	// Feed persistence success to transition to FinalizedState.
	h.outboxMessages = nil
	err = h.sendEvent(&PersistServerSigningSucceededEvent{})
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

	// Simulate confirmation — the FSM emits a ConfirmRoundReq outbox
	// event and transitions to AwaitingConfirmPersistState.
	h.outboxMessages = nil
	err = h.sendEvent(&TransactionConfirmedEvent{
		BlockHeight: 100,
		BlockHash:   chainhash.Hash{},
	})
	require.NoError(t, err)
	assertStateType[*AwaitingConfirmPersistState](h)
	h.assertOutboxLen(1)
	confirmReq := assertOutboxContains[*ConfirmRoundReq](h)
	require.Equal(t, h.roundID, confirmReq.RoundID)

	// Feed success to complete the confirmation cycle.
	h.outboxMessages = nil
	err = h.sendEvent(&ConfirmRoundSucceededEvent{})
	require.NoError(t, err)
	assertStateType[*ConfirmedState](h)
	h.assertOutboxLen(0)
}

// TestFSMForfeitSigningFlowE2ERealSigs exercises the full forfeit signing flow
// with real signatures and validates the completed forfeit transaction.
func TestFSMForfeitSigningFlowE2ERealSigs(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	const baseKeyIndex = 10
	const exitDelay = 144
	const expiry = 144
	client := newClientHarness(
		t, "client1", baseKeyIndex, h.operatorPub, exitDelay, expiry,
	)
	h.env.Terms.VTXOExitDelay = exitDelay
	connectorKey := txscript.ComputeTaprootOutputKey(
		h.operatorPub, nil,
	)
	connectorAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(connectorKey), h.env.ChainParams,
	)
	require.NoError(t, err)
	h.env.Terms.ConnectorAddress = connectorAddr

	boardingOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("boarding")),
		Index: 0,
	}
	forfeitOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("forfeit")),
		Index: 0,
	}

	h.setupPermissiveMocks()

	bi := buildTestBoardingInputForClient(
		t, &boardingOutpoint, 100_000, client.boardingKey,
		h.operatorPub, exitDelay,
	)

	// Build a VTXO descriptor for the forfeit input — needed later
	// for building the forfeit transaction and its signature.
	vtxoDescriptor, err := tree.NewVTXODescriptor(
		50000, client.boardingKey, h.operatorPub, 144,
	)
	require.NoError(t, err)
	vtxo := &VTXO{
		RoundID:          h.roundID,
		BatchOutputIndex: 0,
		Descriptor:       vtxoDescriptor,
		Status:           VTXOStatusLive,
	}

	fi := &ForfeitInput{Outpoint: &forfeitOutpoint, VTXO: vtxo}

	boardingReq := client.createBoardingRequest(&boardingOutpoint)
	forfeitReq := &types.ForfeitRequest{
		VTXOOutpoint: &forfeitOutpoint,
	}
	feedJoinSuccess(
		h, client.clientID,
		client.createJoinRequestWithForfeits(
			[]*types.BoardingRequest{boardingReq},
			[]*types.ForfeitRequest{forfeitReq},
		),
		buildJoinResultWithForfeits(
			[]*BoardingInput{bi},
			[]*ForfeitInput{fi},
		),
	)

	h.outboxMessages = nil
	err = h.sendEvent(&RegistrationTimeoutEvent{})
	require.NoError(t, err)

	feedBatchBuildViaHandler(h)

	awaitState := assertStateType[*AwaitingInputSigsState](h)
	assignment :=
		awaitState.ConnectorAssignments[forfeitOutpoint]
	require.NotNil(t, assignment)

	clientPriv := testForfeitPrivKey(byte(baseKeyIndex + 1))
	require.True(t, clientPriv.PubKey().IsEqual(
		client.boardingKey,
	))

	forfeitTx := buildForfeitTx(
		t, forfeitOutpoint, vtxo.Descriptor.Amount,
		assignment.LeafOutpoint, h.env.ForfeitScript,
	)
	clientSig := forfeitTxSig(
		t, forfeitTx, clientPriv, forfeitOutpoint,
		assignment.LeafOutput, h.operatorPub,
		h.env.Terms.VTXOExitDelay, vtxo.Descriptor,
	)

	sigEvent := client.createInputSignaturesEvent(awaitState)
	sigEvent.ForfeitTxs = []*types.ForfeitTxSig{{
		UnsignedTx:    forfeitTx,
		ClientVTXOSig: clientSig,
	}}

	h.outboxMessages = nil
	err = h.sendEvent(sigEvent)
	require.NoError(t, err)

	// Should be in AwaitingSignAndFinalizeState with signing request.
	assertStateType[*AwaitingSignAndFinalizeState](h)
	signReq := assertOutboxContains[*SignAndFinalizeRoundReq](h)

	// Verify the signing request carries the forfeit data.
	require.NotNil(t, signReq.CollectedForfeitTxs)
	require.Contains(t,
		signReq.CollectedForfeitTxs, client.clientID,
	)

	// Feed signing success with pre-built forfeit info. Actual
	// signing verification is covered by forfeits_test.go and
	// handler tests.
	finalTx := wire.NewMsgTx(2)
	forfeitInfos := map[wire.OutPoint]*ForfeitInfo{
		forfeitOutpoint: {
			RoundID:              h.env.RoundID,
			ConnectorOutputIndex: assignment.ConnectorOutputIndex,
			LeafIndex:            assignment.LeafIndex,
			ForfeitTx:            forfeitTx,
		},
	}
	h.outboxMessages = nil
	err = h.sendEvent(&SignAndFinalizeSucceededEvent{
		FinalTx:      finalTx,
		ForfeitInfos: forfeitInfos,
	})
	require.NoError(t, err)

	assertStateType[*AwaitingServerSignPersistState](h)
	assertOutboxContains[*PersistServerSigningReq](h)

	// Feed persistence success to transition to FinalizedState.
	h.outboxMessages = nil
	err = h.sendEvent(&PersistServerSigningSucceededEvent{})
	require.NoError(t, err)

	finalState := assertStateType[*FinalizedState](h)
	require.NotNil(t, finalState.FinalTx)
	require.Contains(t, finalState.ForfeitInfos, forfeitOutpoint)

	// Simulate confirmation — the FSM emits a ConfirmRoundReq and
	// transitions to the intermediate AwaitingConfirmPersistState.
	h.outboxMessages = nil
	err = h.sendEvent(&TransactionConfirmedEvent{
		BlockHeight: 100,
		BlockHash:   chainhash.Hash{},
		NumConfs:    6,
	})
	require.NoError(t, err)
	assertStateType[*AwaitingConfirmPersistState](h)
	h.assertOutboxLen(1)
	confirmReq := assertOutboxContains[*ConfirmRoundReq](h)
	require.Contains(t, confirmReq.ForfeitInfos, forfeitOutpoint)
	require.Equal(t,
		forfeitInfos[forfeitOutpoint],
		confirmReq.ForfeitInfos[forfeitOutpoint],
	)

	// Feed success to complete the confirmation cycle.
	h.outboxMessages = nil
	err = h.sendEvent(&ConfirmRoundSucceededEvent{})
	require.NoError(t, err)
	assertStateType[*ConfirmedState](h)
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

	h.setupPermissiveMocks()

	bi1 := buildTestBoardingInputForClient(
		t, &outpoint1, 100_000, client1.boardingKey,
		h.operatorPub, exitDelay,
	)
	bi2 := buildTestBoardingInputForClient(
		t, &outpoint2, 100_000, client2.boardingKey,
		h.operatorPub, exitDelay,
	)

	vtxoReq1 := client1.createVTXORequest(btcutil.Amount(50_000))
	vtxoReq2 := client2.createVTXORequest(btcutil.Amount(60_000))

	// Both clients join with boarding + VTXO requests via outbox.
	feedJoinSuccess(
		h, client1.clientID,
		client1.createJoinRequestWithVTXOs(
			[]*types.BoardingRequest{
				client1.createBoardingRequest(&outpoint1),
			},
			[]*types.VTXORequest{vtxoReq1},
		),
		buildJoinResultWithVTXOs(
			[]*BoardingInput{bi1}, client1,
		),
	)
	feedJoinSuccess(
		h, client2.clientID,
		client2.createJoinRequestWithVTXOs(
			[]*types.BoardingRequest{
				client2.createBoardingRequest(&outpoint2),
			},
			[]*types.VTXORequest{vtxoReq2},
		),
		buildJoinResultWithVTXOs(
			[]*BoardingInput{bi2}, client2,
		),
	)

	// Seal and run the batch build through the handler to produce
	// real VTXO trees.
	h.outboxMessages = nil
	err := h.sendEvent(&SealEvent{})
	require.NoError(t, err)

	feedBatchBuildViaHandler(h)

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
	err = h.sendEvent(client1.createInputSignaturesEvent(
		awaitBoarding,
	))
	require.NoError(t, err)

	awaitBoarding = assertStateType[*AwaitingInputSigsState](h)
	err = h.sendEvent(client2.createInputSignaturesEvent(
		awaitBoarding,
	))
	require.NoError(t, err)

	// Should be in AwaitingSignAndFinalizeState.
	assertStateType[*AwaitingSignAndFinalizeState](h)
	assertOutboxContains[*SignAndFinalizeRoundReq](h)

	// Feed signing success.
	h.outboxMessages = nil
	err = h.sendEvent(&SignAndFinalizeSucceededEvent{
		FinalTx:      wire.NewMsgTx(2),
		ForfeitInfos: make(map[wire.OutPoint]*ForfeitInfo),
	})
	require.NoError(t, err)

	assertStateType[*AwaitingServerSignPersistState](h)
	assertOutboxContains[*PersistServerSigningReq](h)

	// Feed persistence success.
	h.outboxMessages = nil
	err = h.sendEvent(&PersistServerSigningSucceededEvent{})
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

	h.setupPermissiveMocks()

	bi := buildTestBoardingInputForClient(
		t, &outpoint, 100_000, client.boardingKey,
		h.operatorPub, exitDelay,
	)

	// Two VTXO requests with distinct signing keys.
	vtxoReq1 := client.createVTXORequest(btcutil.Amount(40_000))
	vtxoReq2 := client.createVTXORequest(btcutil.Amount(50_000))

	feedJoinSuccess(
		h, client.clientID,
		client.createJoinRequestWithVTXOs(
			[]*types.BoardingRequest{
				client.createBoardingRequest(&outpoint),
			},
			[]*types.VTXORequest{vtxoReq1, vtxoReq2},
		),
		buildJoinResultWithVTXOs(
			[]*BoardingInput{bi}, client,
		),
	)

	// Seal and run the batch build through the handler.
	h.outboxMessages = nil
	err := h.sendEvent(&RegistrationTimeoutEvent{})
	require.NoError(t, err)

	feedBatchBuildViaHandler(h)

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
	err = h.sendEvent(client.createInputSignaturesEvent(awaitBoarding))
	require.NoError(t, err)

	// Should be in AwaitingSignAndFinalizeState with signing request.
	assertStateType[*AwaitingSignAndFinalizeState](h)
	assertOutboxContains[*SignAndFinalizeRoundReq](h)

	// Feed signing success.
	finalTx := wire.NewMsgTx(2)
	h.outboxMessages = nil
	err = h.sendEvent(&SignAndFinalizeSucceededEvent{
		FinalTx: finalTx,
	})
	require.NoError(t, err)

	// Should be in AwaitingServerSignPersistState.
	assertStateType[*AwaitingServerSignPersistState](h)
	assertOutboxContains[*PersistServerSigningReq](h)

	// Feed persistence success.
	h.outboxMessages = nil
	err = h.sendEvent(&PersistServerSigningSucceededEvent{})
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

		// Send VTXOSignaturesTimeoutEvent.
		err := h.sendEvent(&VTXOSignaturesTimeoutEvent{})
		require.NoError(t, err)

		// Should transition to FailedState.
		failedState := assertStateType[*FailedState](h)
		require.Contains(t, failedState.Reason, "VTXO signature")
		require.Contains(t, failedState.Reason, "timeout")

		// Verify outbox messages include unlock events.
		var (
			foundClientFailed bool
			foundRoundFailed  bool
			foundUnlockBI     bool
		)
		for _, msg := range h.outboxMessages {
			switch m := msg.(type) {
			case *ClientRoundFailedResp:
				foundClientFailed = true
				require.Equal(t, ClientID("client1"), m.Client)
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *UnlockBoardingInputsReq:
				foundUnlockBI = true
				require.Equal(t, h.env.RoundID, m.RoundID)

			case *RoundFailedReq:
				foundRoundFailed = true
				require.Equal(t, h.env.RoundID, m.FailedRoundID)
			}
		}
		require.True(t, foundClientFailed, "client should be notified")
		require.True(t, foundUnlockBI,
			"boarding inputs should be unlocked via outbox")
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
