package round

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestStateProperties verifies IsTerminal() and String() for all states.
func TestStateProperties(t *testing.T) {
	t.Parallel()

	t.Run("terminal_states", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			state    ClientState
			expected string
		}{
			{
				name:     "Confirmed",
				state:    &ConfirmedState{},
				expected: "Confirmed",
			},
			{
				name:     "ClientFailed",
				state:    &ClientFailedState{Reason: "test"},
				expected: "ClientFailed",
			},
			{
				name: "RecoveryInitiated",
				state: &RecoveryInitiatedState{
					Reason: "CSV",
				},
				expected: "RecoveryInitiated",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				require.True(t, tc.state.IsTerminal())
				require.Contains(
					t, tc.state.String(), tc.expected,
				)
			})
		}
	})

	t.Run("non_terminal_states", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			state    ClientState
			expected string
		}{
			{
				"Idle", &Idle{}, "Idle",
			},
			{
				"PendingRoundAssembly", &PendingRoundAssembly{},
				"BoardingIntents",
			},
			{
				"RegistrationSent", &RegistrationSentState{},
				"RegistrationSent",
			},
			{
				"RoundJoined", &RoundJoinedState{},
				"RoundJoined",
			},
			{
				"CommitmentTxReceived",
				&CommitmentTxReceivedState{},
				"CommitmentTxReceived",
			},
			{
				"CommitmentTxValidated",
				&CommitmentTxValidatedState{},
				"CommitmentTxValidated",
			},
			{
				"NoncesSent", &NoncesSentState{}, "NoncesSent",
			},
			{
				"NoncesAggregated", &NoncesAggregatedState{},
				"NoncesAggregated",
			},
			{
				"PartialSigsSent", &PartialSigsSentState{},
				"PartialSigsSent",
			},
			{
				"InputSigSent", &InputSigSentState{},
				"InputSigSent",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				require.False(t, tc.state.IsTerminal())
				require.Equal(t, tc.expected, tc.state.String())
			})
		}
	})
}

// TestUnexpectedEventErrors verifies all states reject invalid events.
func TestUnexpectedEventErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(h *boardingTestHarness) ClientState
		event ClientEvent
	}{
		{
			name: "Idle_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				return &Idle{}
			},
			event: &RoundJoined{RoundID: "test"},
		},
		{
			name: "PendingRoundAssembly_rejects_RoundComplete",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				intents := map[wire.OutPoint]BoardingIntent{
					intent.Outpoint: intent,
				}

				return &PendingRoundAssembly{
					Intents: intents,
				}
			},
			event: &RoundComplete{},
		},
		{
			name: "RegistrationSent_rejects_BoardingConfirmed",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RegistrationSentState{
					Intents: []BoardingIntent{intent},
				}
			},
			event: &BoardingConfirmed{},
		},
		{
			name: "RoundJoined_rejects_duplicate_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RoundJoinedState{
					RoundID: "round-1",
					Intents: []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "CommitmentTxReceived_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return h.newCommitmentTxReceivedState(
					"round-001", []BoardingIntent{intent},
				)
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "CommitmentTxValidated_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return h.newCommitmentTxValidatedState(
					"round-001", []BoardingIntent{intent},
				)
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "NoncesSent_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &NoncesSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "NoncesAggregated_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &NoncesAggregatedState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "PartialSigsSent_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &PartialSigsSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
		{
			name: "InputSigSent_rejects_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &InputSigSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: "another-round"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.withState(tc.setup(h))

			_, err := h.sendEvent(tc.event)
			require.Error(t, err)
			require.Contains(t, err.Error(), "unexpected")
		})
	}
}

// TestBoardingFailedTransitions verifies all non-terminal states handle
// BoardingFailed by transitioning to ClientFailedState.
func TestBoardingFailedTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(h *boardingTestHarness) ClientState
	}{
		{
			name: "RegistrationSentState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RegistrationSentState{
					Intents: []BoardingIntent{intent},
				}
			},
		},
		{
			name: "RoundJoinedState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RoundJoinedState{
					RoundID: "round-001",
					Intents: []BoardingIntent{intent},
				}
			},
		},
		{
			name: "CommitmentTxReceivedState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return h.newCommitmentTxReceivedState(
					"round-001", []BoardingIntent{intent},
				)
			},
		},
		{
			name: "NoncesSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &NoncesSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
		},
		{
			name: "PartialSigsSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &PartialSigsSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
		},
		{
			name: "InputSigSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				return &InputSigSentState{
					RoundID:  "round-001",
					VTXTTree: vtxtTree,
					Intents:  []BoardingIntent{intent},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.withState(tc.setup(h))

			event := &BoardingFailed{
				Reason:      "test failure reason",
				Recoverable: true,
			}

			transition, err := h.sendEvent(event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			failedState := assertStateType[*ClientFailedState](h)
			require.Equal(
				t, "test failure reason", failedState.Reason,
			)
			require.True(t, failedState.Recoverable)
		})
	}
}

// TestTerminalStateSelfLoop verifies terminal states self-loop on any event.
func TestTerminalStateSelfLoop(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()

	tests := []struct {
		name  string
		state ClientState
	}{
		{"ConfirmedState", &ConfirmedState{BlockHeight: 100}},
		{"ClientFailedState", &ClientFailedState{Reason: "failed"}},
		{
			"RecoveryInitiatedState",
			&RecoveryInitiatedState{Outpoint: intent.Outpoint},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.withState(tc.state)

			// Terminal states ignore all events and remain in their
			// current state, preventing further transitions once
			// boarding reaches a final outcome.
			event := &RoundJoined{RoundID: "test"}
			transition, err := h.sendEvent(event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			require.Equal(
				t, tc.state.String(), h.currentState.String(),
			)
		})
	}
}

func TestIdleState(t *testing.T) {
	t.Parallel()

	t.Run("BoardingUTXOConfirmed_to_pending", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Intents, 1)
		require.Contains(t, nextState.Intents, intent.Outpoint)
	})

	t.Run("ResumeBoardingIntents_with_intents", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := &ResumeBoardingIntents{
			Intents: map[wire.OutPoint]BoardingIntent{
				intent.Outpoint: intent,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Intents, 1)
	})

	t.Run("ResumeBoardingIntents_empty_stays_idle", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		event := &ResumeBoardingIntents{
			Intents: map[wire.OutPoint]BoardingIntent{},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		_ = assertStateType[*Idle](h)
	})

	t.Run("nil_tx_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)
		event.Tx = nil

		_, err := h.sendEvent(event)
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing transaction")
	})

	t.Run("invalid_outpoint_index_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)
		event.Outpoint.Index = 999

		_, err := h.sendEvent(event)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid outpoint index")
	})
}

func TestPendingRoundAssemblyState(t *testing.T) {
	t.Parallel()

	t.Run("additional_confirmation_accumulates", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		existingIntent := h.newTestBoardingIntent()
		h.withState(&PendingRoundAssembly{
			Intents: map[wire.OutPoint]BoardingIntent{
				existingIntent.Outpoint: existingIntent,
			},
		})

		newIntent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(newIntent)

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Intents, 2)
	})

	t.Run("registration_requested_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		h.withState(&PendingRoundAssembly{
			Intents: map[wire.OutPoint]BoardingIntent{
				intent.Outpoint: intent,
			},
		})

		intents := []BoardingIntent{intent}
		event := &RegistrationRequested{Intents: intents}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*RegistrationSentState](h)
		require.Len(t, nextState.Intents, 1)
	})

	t.Run("no_intents_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&PendingRoundAssembly{
			Intents: map[wire.OutPoint]BoardingIntent{},
		})

		event := &RegistrationRequested{Intents: []BoardingIntent{}}

		_, err := h.sendEvent(event)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no boarding requests")
	})
}

func TestRegistrationSentState(t *testing.T) {
	t.Parallel()

	t.Run("RoundJoined_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		h.withState(&RegistrationSentState{
			Intents: []BoardingIntent{intent},
		})

		event := &RoundJoined{RoundID: "test-round-123"}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*RoundJoinedState](h)
		require.Equal(t, "test-round-123", nextState.RoundID)
		require.Len(t, nextState.Intents, 1)
	})
}

func TestRoundJoinedState(t *testing.T) {
	t.Parallel()

	t.Run("CommitmentTxBuilt_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		h.withState(&RoundJoinedState{
			RoundID: "round-001",
			Intents: []BoardingIntent{intent},
		})

		vtxtTree, _ := h.newTestVTXOTree(1)
		commitEvent := h.newCommitmentTxBuiltEvent(
			"round-001", []BoardingIntent{intent}, vtxtTree,
		)

		transition, err := h.sendEvent(commitEvent)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*CommitmentTxReceivedState](h)
		require.Equal(t, "round-001", nextState.RoundID)
		require.NotNil(t, nextState.CommitmentTx)
		require.True(t, transition.NewEvents.IsSome())
	})
}

func TestCommitmentTxReceivedState(t *testing.T) {
	t.Parallel()

	t.Run("validation_succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{intent.Outpoint},
		)

		state := &CommitmentTxReceivedState{
			RoundID:      "round-001",
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.TxHash(),
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			CommitmentTxBuiltEvent: CommitmentTxBuiltEvent{
				RoundID:  "round-001",
				Tx:       commitmentTx,
				VTXTTree: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		validatedState := assertStateType[*CommitmentTxValidatedState](
			h,
		)
		require.Equal(t, "round-001", validatedState.RoundID)
		require.NotEmpty(t, validatedState.BoardingInputIndices)
		assertTransitionEmitsInternalEvent[*GenerateNonces](
			h, transition,
		)
	})

	t.Run("missing_boarding_input_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)

		// Create tx WITHOUT the intent's outpoint.
		differentOutpoint := h.newTestOutpoint()
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{differentOutpoint},
		)

		state := &CommitmentTxReceivedState{
			RoundID:      "round-001",
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.TxHash(),
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			CommitmentTxBuiltEvent: CommitmentTxBuiltEvent{
				RoundID:  "round-001",
				Tx:       commitmentTx,
				VTXTTree: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "validation failed")
	})
}

func TestCommitmentTxValidatedState(t *testing.T) {
	t.Parallel()

	t.Run("GenerateNonces_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockWalletForMuSig2()

		intent := h.newTestBoardingIntent()
		state := h.newCommitmentTxValidatedState(
			"round-001", []BoardingIntent{intent},
		)
		h.withState(state)

		transition, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		noncesSentState := assertStateType[*NoncesSentState](h)
		require.Equal(t, "round-001", noncesSentState.RoundID)
		require.NotNil(t, noncesSentState.Musig2Sessions)

		h.assertOutboxLen(1)
		h.assertOutboxContainsType("*round.SubmitNoncesRequest")
	})
}

func TestNoncesSentState(t *testing.T) {
	t.Parallel()

	t.Run("NoncesAggregated_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockWalletForMuSig2()

		intent := h.newTestBoardingIntent()

		// Build up through actual flow to get real sessions.
		validatedState := h.newCommitmentTxValidatedState(
			"round-001", []BoardingIntent{intent},
		)
		h.withState(validatedState)

		_, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		h.clearOutbox()

		noncesSentState := assertStateType[*NoncesSentState](h)

		event := h.newNoncesAggregatedEvent(
			"round-001", noncesSentState.VTXTTree,
		)

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		noncesAggState := assertStateType[*NoncesAggregatedState](h)
		require.Equal(t, "round-001", noncesAggState.RoundID)
		assertTransitionEmitsInternalEvent[*GeneratePartialSigs](
			h, transition,
		)
	})
}

func TestNoncesAggregatedState(t *testing.T) {
	t.Parallel()

	t.Run("GeneratePartialSigs_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockWalletForMuSig2()

		intent := h.newTestBoardingIntent()

		// We build through the full state machine flow to ensure MuSig2
		// sessions are properly initialized with real nonce state.
		validatedState := h.newCommitmentTxValidatedState(
			"round-001", []BoardingIntent{intent},
		)
		h.withState(validatedState)

		_, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		h.clearOutbox()

		noncesSentState := assertStateType[*NoncesSentState](h)

		aggEvent := h.newNoncesAggregatedEvent(
			"round-001", noncesSentState.VTXTTree,
		)
		_, err = h.sendEvent(aggEvent)
		require.NoError(t, err)
		h.clearOutbox()

		_ = assertStateType[*NoncesAggregatedState](h)

		transition, err := h.sendEvent(&GeneratePartialSigs{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		partialSigsState := assertStateType[*PartialSigsSentState](h)
		require.Equal(t, "round-001", partialSigsState.RoundID)
		h.assertOutboxContainsType("*round.SubmitPartialSigRequest")
	})
}

func TestPartialSigsSentState(t *testing.T) {
	t.Parallel()

	t.Run("OperatorSigned_with_real_signatures", func(t *testing.T) {
		t.Parallel()

		h := newRealSigningTestHarness(t)

		intent := h.newTestBoardingIntentWithTapscript()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)

		validSigs, err := h.generateValidTreeSignatures(vtxtTree)
		require.NoError(t, err)
		require.NotEmpty(t, validSigs)

		commitmentTx := h.newCommitmentTxForIntents(
			[]BoardingIntent{intent}, vtxtTree,
		)

		clientTrees := make(map[SignerKey]*tree.Tree)
		for _, vtxoReq := range intent.VtxoTemplate {
			signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)
			clientTrees[signerKey] = vtxtTree
		}

		state := &PartialSigsSentState{
			RoundID:      "round-real-sig-001",
			CommitmentTx: commitmentTx,
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  clientTrees,
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
			Musig2Sessions: make(map[SignerKey]*tree.SignerSession),
		}

		h.setupMockWalletForBoardingSigning()
		h.setupMockRoundStoreForCommit()
		h.withState(state)

		event := &OperatorSigned{
			OperatorSignedEvent: OperatorSignedEvent{
				RoundID:    "round-real-sig-001",
				Signatures: validSigs,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		inputSigState := assertStateType[*InputSigSentState](
			h.boardingTestHarness,
		)
		require.Equal(t, "round-real-sig-001", inputSigState.RoundID)
		require.NotEmpty(t, inputSigState.InputSigs)

		h.assertOutboxContainsType("*round.SubmitForfeitSigRequest")
		h.assertOutboxContainsType("*round.RegisterConfirmationRequest")
	})

	t.Run("empty_signatures_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{intent.Outpoint},
		)

		state := &PartialSigsSentState{
			RoundID:      "round-001",
			CommitmentTx: commitmentTx,
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}
		h.withState(state)

		event := &OperatorSigned{
			OperatorSignedEvent: OperatorSignedEvent{
				RoundID:    "round-001",
				Signatures: [][]byte{},
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "signature")
	})

	t.Run("invalid_signature_format_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{intent.Outpoint},
		)

		state := &PartialSigsSentState{
			RoundID:      "round-001",
			CommitmentTx: commitmentTx,
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}
		h.withState(state)

		event := &OperatorSigned{
			OperatorSignedEvent: OperatorSignedEvent{
				RoundID:    "round-001",
				Signatures: [][]byte{make([]byte, 10)},
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "signature")
	})

	t.Run("missing_input_index_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{intent.Outpoint},
		)

		state := &PartialSigsSentState{
			RoundID:              "round-001",
			CommitmentTx:         commitmentTx,
			VTXTTree:             vtxtTree,
			Intents:              []BoardingIntent{intent},
			ClientTrees:          make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: make(map[wire.OutPoint]int),
			Musig2Sessions: make(
				map[SignerKey]*tree.SignerSession,
			),
		}
		h.withState(state)

		validSig := make([]byte, 64)
		for i := range validSig {
			validSig[i] = byte(i + 1)
		}

		event := &OperatorSigned{
			OperatorSignedEvent: OperatorSignedEvent{
				RoundID:    "round-001",
				Signatures: [][]byte{validSig},
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "signature")
	})

	t.Run("too_few_signatures_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(
			[]wire.OutPoint{intent.Outpoint},
		)

		state := &PartialSigsSentState{
			RoundID:      "round-001",
			CommitmentTx: commitmentTx,
			VTXTTree:     vtxtTree,
			Intents:      []BoardingIntent{intent},
			ClientTrees:  make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}
		h.withState(state)

		nodeCount := 0
		_ = vtxtTree.Root.ForEach(func(n *tree.Node) error {
			nodeCount++
			return nil
		})

		if nodeCount > 1 {
			event := &OperatorSigned{
				OperatorSignedEvent: OperatorSignedEvent{
					RoundID:    "round-001",
					Signatures: [][]byte{make([]byte, 64)},
				},
			}

			transition, err := h.sendEvent(event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			failedState := assertStateType[*ClientFailedState](h)
			require.Contains(t, failedState.Reason, "signature")
		}
	})
}

func TestInputSigSentState(t *testing.T) {
	t.Parallel()

	t.Run("BoardingConfirmed_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockVTXOStoreForSave()

		intent := h.newTestBoardingIntent()
		state := h.newInputSigSentState(
			"round-001", []BoardingIntent{intent},
		)
		h.withState(state)

		event := &BoardingConfirmed{
			TxID:          state.CommitmentTx.TxHash(),
			BlockHeight:   101,
			Confirmations: 6,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		confirmedState := assertStateType[*ConfirmedState](h)
		require.Equal(t, int32(101), confirmedState.BlockHeight)
		require.Equal(t, int32(6), confirmedState.Confirmations)

		h.assertOutboxLen(2)
		h.vtxoStore.AssertCalled(t, "SaveVTXOs", mock.Anything)
	})

	t.Run("buildClientVTXOs_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		vtxtTree, _ := h.newTestVTXOTree(1)
		intent := h.newTestBoardingIntent()

		// Empty ClientTrees will cause buildClientVTXOs to fail.
		state := &InputSigSentState{
			RoundID:     "round-001",
			VTXTTree:    vtxtTree,
			Intents:     []BoardingIntent{intent},
			ClientTrees: make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &BoardingConfirmed{
			TxID:          vtxtTree.BatchOutpoint.Hash,
			BlockHeight:   101,
			Confirmations: 6,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "build client VTXOs")
	})
}

func TestConfirmedState(t *testing.T) {
	t.Parallel()

	t.Run("RoundComplete_transitions_to_idle", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&ConfirmedState{BlockHeight: 100})

		transition, err := h.sendEvent(&RoundComplete{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		_ = assertStateType[*Idle](h)
	})
}

func TestClientFailedState(t *testing.T) {
	t.Parallel()

	t.Run("RecoveryInitiated_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		h.withState(&ClientFailedState{
			Reason:      "round failed",
			Recoverable: true,
		})

		event := &RecoveryInitiated{
			Outpoint: intent.Outpoint,
			Reason:   "CSV timeout recovery",
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		recoveryState := assertStateType[*RecoveryInitiatedState](h)
		require.Equal(t, intent.Outpoint, recoveryState.Outpoint)
		require.Equal(t, "CSV timeout recovery", recoveryState.Reason)
	})
}

func TestValidateBoardingInputs(t *testing.T) {
	t.Parallel()

	t.Run("nil_tx", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent := h.newTestBoardingIntent()
		intents := []BoardingIntent{intent}

		result, err := validateBoardingInputs(nil, intents)

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "commitment tx is nil")
	})

	t.Run("empty_intents", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		tx := h.newTestCommitmentTx(nil)

		result, err := validateBoardingInputs(tx, []BoardingIntent{})

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "no boarding intents")
	})

	t.Run("missing_outpoint", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent := h.newTestBoardingIntent()
		differentOutpoint := h.newTestOutpoint()
		tx := h.newTestCommitmentTx([]wire.OutPoint{differentOutpoint})
		intents := []BoardingIntent{intent}

		result, err := validateBoardingInputs(tx, intents)

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "not found in commitment tx")
	})

	t.Run("success_single", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent := h.newTestBoardingIntent()
		tx := h.newTestCommitmentTx([]wire.OutPoint{intent.Outpoint})
		intents := []BoardingIntent{intent}

		result, err := validateBoardingInputs(tx, intents)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, result, 1)

		idx, found := result[intent.Outpoint]
		require.True(t, found)
		require.Equal(t, 0, idx)
	})

	t.Run("success_multiple", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent1 := h.newTestBoardingIntent()
		intent2 := h.newTestBoardingIntent()
		intent3 := h.newTestBoardingIntent()

		tx := h.newTestCommitmentTx([]wire.OutPoint{
			intent1.Outpoint,
			intent2.Outpoint,
			intent3.Outpoint,
		})

		intents := []BoardingIntent{intent1, intent2, intent3}
		result, err := validateBoardingInputs(tx, intents)

		require.NoError(t, err)
		require.Len(t, result, 3)
		require.Equal(t, 0, result[intent1.Outpoint])
		require.Equal(t, 1, result[intent2.Outpoint])
		require.Equal(t, 2, result[intent3.Outpoint])
	})
}

func TestNewSignerKey(t *testing.T) {
	t.Parallel()

	t.Run("valid_key", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		result := NewSignerKey(h.clientPubKey)

		// SignerKey is [33]byte (compressed pubkey).
		require.Len(t, result, 33)
		require.Equal(
			t, h.clientPubKey.SerializeCompressed(), result[:],
		)
	})

	t.Run("deterministic", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		result1 := NewSignerKey(h.clientPubKey)
		result2 := NewSignerKey(h.clientPubKey)

		require.Equal(t, result1, result2)
	})
}

func TestBoardingFlowIdleToPendingToRegistrationSent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	intentsMap := map[wire.OutPoint]BoardingIntent{intent.Outpoint: intent}
	h.withState(&PendingRoundAssembly{Intents: intentsMap})

	// Step 1: Request registration.
	regEvent := &RegistrationRequested{Intents: []BoardingIntent{intent}}
	_, err := h.sendEvent(regEvent)
	require.NoError(t, err)

	regState := assertStateType[*RegistrationSentState](h)
	require.Len(t, regState.Intents, 1)

	// Step 2: Server accepts.
	joinEvent := &RoundJoined{RoundID: "round-001"}
	_, err = h.sendEvent(joinEvent)
	require.NoError(t, err)

	joinedState := assertStateType[*RoundJoinedState](h)
	require.Equal(t, "round-001", joinedState.RoundID)
}

func TestBoardingFlowMultipleIntentsAccumulation(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.withState(&Idle{})

	// We confirm multiple UTXOs to verify that PendingRoundAssembly
	// correctly accumulates boarding intents as they arrive, rather than
	// replacing or dropping previous ones.
	for i := 0; i < 3; i++ {
		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		state := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, state.Intents, i+1)
	}
}

func TestBoardingFlowPendingToRoundJoined(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	intentsMap := map[wire.OutPoint]BoardingIntent{intent.Outpoint: intent}
	h.withState(&PendingRoundAssembly{Intents: intentsMap})

	// Step 1: PendingRoundAssembly → RegistrationSentState.
	regEvent := &RegistrationRequested{Intents: []BoardingIntent{intent}}
	_, err := h.sendEvent(regEvent)
	require.NoError(t, err)

	regSentState := assertStateType[*RegistrationSentState](h)
	require.Len(t, regSentState.Intents, 1)

	// Step 2: RegistrationSentState → RoundJoinedState.
	joinEvent := &RoundJoined{RoundID: "round-integration-001"}
	_, err = h.sendEvent(joinEvent)
	require.NoError(t, err)

	joinedState := assertStateType[*RoundJoinedState](h)
	require.Equal(t, "round-integration-001", joinedState.RoundID)
}

func TestBoardingFlowRoundJoinedToPartialSigsSent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockWalletForMuSig2()

	intent := h.newTestBoardingIntent()

	h.withState(&RoundJoinedState{
		RoundID: "round-integration-002",
		Intents: []BoardingIntent{intent},
	})

	// Step 1: RoundJoined → CommitmentTxReceived.
	vtxtTree := h.newTestVTXOTreeForIntent(intent)
	commitEvent := h.newCommitmentTxBuiltEvent(
		"round-integration-002", []BoardingIntent{intent}, vtxtTree,
	)
	_, err := h.sendEvent(commitEvent)
	require.NoError(t, err)

	ctxReceivedState := assertStateType[*CommitmentTxReceivedState](h)
	require.NotNil(t, ctxReceivedState.CommitmentTx)

	// Step 2: CommitmentTxReceived → CommitmentTxValidated.
	_, err = h.sendEvent(&CommitmentTxBuilt{
		CommitmentTxBuiltEvent: CommitmentTxBuiltEvent{
			RoundID:  "round-integration-002",
			Tx:       ctxReceivedState.CommitmentTx,
			VTXTTree: ctxReceivedState.VTXTTree,
		},
	})
	require.NoError(t, err)

	ctxValidatedState := assertStateType[*CommitmentTxValidatedState](h)
	require.NotEmpty(t, ctxValidatedState.BoardingInputIndices)
	h.clearOutbox()

	// Step 3: CommitmentTxValidated → NoncesSent.
	_, err = h.sendEvent(&GenerateNonces{})
	require.NoError(t, err)

	noncesSentState := assertStateType[*NoncesSentState](h)
	require.NotEmpty(t, noncesSentState.Musig2Sessions)
	h.clearOutbox()

	// Step 4: NoncesSent → NoncesAggregated.
	aggNoncesEvent := h.newNoncesAggregatedEvent(
		"round-integration-002", noncesSentState.VTXTTree,
	)
	_, err = h.sendEvent(aggNoncesEvent)
	require.NoError(t, err)

	noncesAggState := assertStateType[*NoncesAggregatedState](h)
	require.NotEmpty(t, noncesAggState.AggregatedNonces)
	h.clearOutbox()

	// Step 5: NoncesAggregated → PartialSigsSent.
	_, err = h.sendEvent(&GeneratePartialSigs{})
	require.NoError(t, err)

	partialSigsState := assertStateType[*PartialSigsSentState](h)
	require.Equal(t, "round-integration-002", partialSigsState.RoundID)
}

func TestBoardingFlowFailureAndRecovery(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	h.withState(&RoundJoinedState{
		RoundID: "round-fail-001",
		Intents: []BoardingIntent{intent},
	})

	// We inject a failure event to verify the state machine correctly
	// transitions to ClientFailedState, preserving the failure reason
	// and recoverability flag for later recovery attempts.
	_, err := h.sendEvent(&BoardingFailed{
		Reason:      "operator went offline",
		Recoverable: true,
	})
	require.NoError(t, err)

	failedState := assertStateType[*ClientFailedState](h)
	require.Equal(t, "operator went offline", failedState.Reason)

	// After failure, we verify recovery can be initiated by transitioning
	// to RecoveryInitiatedState, which triggers CSV timeout-based fund
	// recovery.
	_, err = h.sendEvent(&RecoveryInitiated{
		Outpoint: intent.Outpoint,
		Reason:   "CSV timeout recovery",
	})
	require.NoError(t, err)

	recoveryState := assertStateType[*RecoveryInitiatedState](h)
	require.Equal(t, intent.Outpoint, recoveryState.Outpoint)
}
