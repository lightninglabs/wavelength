package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testRoundIDTr creates a deterministic RoundID from a string seed for tests.
func testRoundIDTr(seed string) RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return RoundID(id)
}

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
				"PendingRoundAssembly",
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
			{
				"ClientFailed",
				&ClientFailedState{Reason: "test"},
				"ClientFailed: test",
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

// TestUnexpectedEventSelfLoop verifies all states self-loop on unexpected
// events instead of returning an error. This prevents FSM halt.
func TestUnexpectedEventSelfLoop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(h *boardingTestHarness) ClientState
		event ClientEvent
	}{
		{
			name: "Idle_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				return &Idle{}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("test")},
		},
		{
			name: "PendingAssembly_self_loops_on_RoundComplete",
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
			name: "RegSent_self_loops_on_BoardingConfirmed",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RegistrationSentState{
					Intents: []BoardingIntent{intent},
				}
			},
			event: &BoardingConfirmed{},
		},
		{
			name: "RoundJoined_self_loops_on_duplicate_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return &RoundJoinedState{
					RoundID: testRoundIDTr("round-1"),
					Intents: []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "CommitmentTxReceived_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				return h.newCommitmentTxReceivedState(
					roundID, []BoardingIntent{intent},
				)
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "CommitmentTxValidated_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				return h.newCommitmentTxValidatedState(
					roundID, []BoardingIntent{intent},
				)
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "NoncesSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &NoncesSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "NoncesAggregated_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &NoncesAggregatedState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "PartialSigsSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &PartialSigsSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "InputSigSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &InputSigSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("other")},
		},
		{
			name: "ClientFailed_self_loops_on_unknown",
			setup: func(h *boardingTestHarness) ClientState {
				return &ClientFailedState{
					Reason:      "previous failure",
					Recoverable: true,
				}
			},
			event: &RoundJoined{RoundID: testRoundIDTr("test")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			initialState := tc.setup(h)
			h.withState(initialState)

			transition, err := h.sendEvent(tc.event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			// Verify self-loop: state type unchanged.
			require.Equal(
				t,
				initialState.String(),
				h.currentState.String(),
			)
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
					RoundID: testRoundIDTr("round-001"),
					Intents: []BoardingIntent{intent},
				}
			},
		},
		{
			name: "CommitmentTxReceivedState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				return h.newCommitmentTxReceivedState(
					testRoundIDTr("round-001"),
					[]BoardingIntent{intent},
				)
			},
		},
		{
			name: "NoncesSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &NoncesSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
		},
		{
			name: "PartialSigsSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &PartialSigsSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
				}
			},
		},
		{
			name: "InputSigSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{0: vtxtTree}

				return &InputSigSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       []BoardingIntent{intent},
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
			event := &RoundJoined{RoundID: testRoundIDTr("test")}
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

	t.Run("nil_tx_transitions_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)
		event.Tx = nil

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "missing transaction")
	})

	t.Run("invalid_outpoint_transitions_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := h.newBoardingUTXOConfirmedEvent(intent)
		event.Outpoint.Index = 999

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "invalid outpoint")
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

	t.Run("no_intents_transitions_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&PendingRoundAssembly{
			Intents: map[wire.OutPoint]BoardingIntent{},
		})

		event := &RegistrationRequested{Intents: []BoardingIntent{}}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "no boarding requests")
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

		event := &RoundJoined{RoundID: testRoundIDTr("test-round-123")}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*RoundJoinedState](h)
		expectedRoundID := testRoundIDTr("test-round-123")
		require.Equal(t, expectedRoundID, nextState.RoundID)
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
			RoundID: testRoundIDTr("round-001"),
			Intents: []BoardingIntent{intent},
		})

		vtxtTree, _ := h.newTestVTXOTree(1)
		commitEvent := h.newCommitmentTxBuiltEvent(
			testRoundIDTr("round-001"),
			[]BoardingIntent{intent},
			vtxtTree,
		)

		transition, err := h.sendEvent(commitEvent)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*CommitmentTxReceivedState](h)
		require.Equal(t, testRoundIDTr("round-001"), nextState.RoundID)
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
		intents := []BoardingIntent{intent}
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &CommitmentTxReceivedState{
			RoundID:       testRoundIDTr("round-001"),
			CommitmentTx:  commitmentTx,
			TxID:          commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       intents,
			ClientTrees:   make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID:       testRoundIDTr("round-001"),
			Tx:            commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		validatedState := assertStateType[*CommitmentTxValidatedState](
			h,
		)
		expectedID := testRoundIDTr("round-001")
		require.Equal(t, expectedID, validatedState.RoundID)
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

		// Create tx WITHOUT the intent's outpoint. Create a fake intent
		// with a different outpoint for the commitment tx.
		differentIntent := h.newTestBoardingIntent()
		differentIntents := []BoardingIntent{differentIntent}
		commitmentTx := h.newTestCommitmentTx(differentIntents)

		state := &CommitmentTxReceivedState{
			RoundID:       testRoundIDTr("round-001"),
			CommitmentTx:  commitmentTx,
			TxID:          commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID:       testRoundIDTr("round-001"),
			Tx:            commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
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
			testRoundIDTr("round-001"),
			[]BoardingIntent{intent},
		)
		h.withState(state)

		transition, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		noncesSentState := assertStateType[*NoncesSentState](h)
		expectedID := testRoundIDTr("round-001")
		require.Equal(t, expectedID, noncesSentState.RoundID)
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
			testRoundIDTr("round-001"),
			[]BoardingIntent{intent},
		)
		h.withState(validatedState)

		_, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		h.clearOutbox()

		noncesSentState := assertStateType[*NoncesSentState](h)

		event := h.newNoncesAggregatedEvent(
			testRoundIDTr("round-001"),
			noncesSentState.VTXOTreePaths[0],
		)

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		noncesAggState := assertStateType[*NoncesAggregatedState](h)
		expectedID := testRoundIDTr("round-001")
		require.Equal(t, expectedID, noncesAggState.RoundID)
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
			testRoundIDTr("round-001"),
			[]BoardingIntent{intent},
		)
		h.withState(validatedState)

		_, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		h.clearOutbox()

		noncesSentState := assertStateType[*NoncesSentState](h)

		aggEvent := h.newNoncesAggregatedEvent(
			testRoundIDTr("round-001"),
			noncesSentState.VTXOTreePaths[0],
		)
		_, err = h.sendEvent(aggEvent)
		require.NoError(t, err)
		h.clearOutbox()

		_ = assertStateType[*NoncesAggregatedState](h)

		transition, err := h.sendEvent(&GeneratePartialSigs{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		partialSigsState := assertStateType[*PartialSigsSentState](h)
		expectedID := testRoundIDTr("round-001")
		require.Equal(t, expectedID, partialSigsState.RoundID)
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
			RoundID:       testRoundIDTr("round-real-sig-001"),
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   clientTrees,
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
			Musig2Sessions: make(map[SignerKey]*tree.SignerSession),
		}

		h.setupMockWalletForBoardingSigning()
		h.setupMockRoundStoreForCommit()
		h.withState(state)

		event := &OperatorSigned{
			RoundID: testRoundIDTr("round-real-sig-001"),
			AggSigs: validSigs,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		inputSigState := assertStateType[*InputSigSentState](
			h.boardingTestHarness,
		)
		expectedRoundID := testRoundIDTr("round-real-sig-001")
		require.Equal(t, expectedRoundID, inputSigState.RoundID)
		require.NotEmpty(t, inputSigState.InputSigs)

		h.assertOutboxContainsType("*round.SubmitForfeitSigRequest")
		h.assertOutboxContainsType("*round.RegisterConfirmationRequest")
	})

	t.Run("empty_signatures_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxtTree := h.newTestVTXOTreeForIntent(intent)
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:       testRoundIDTr("round-001"),
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}
		h.withState(state)

		event := &OperatorSigned{
			RoundID: testRoundIDTr("round-001"),
			AggSigs: map[tree.TxID]*schnorr.Signature{},
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
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:       testRoundIDTr("round-001"),
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
		}
		h.withState(state)

		// Use a fake txid with a dummy signature.
		fakeTxid := chainhash.HashH([]byte("fake-tx"))
		event := &OperatorSigned{
			RoundID: testRoundIDTr("round-001"),
			AggSigs: map[tree.TxID]*schnorr.Signature{
				fakeTxid: {},
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
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:              testRoundIDTr("round-001"),
			CommitmentTx:         commitmentTx,
			VTXOTreePaths:        map[int]*tree.Tree{0: vtxtTree},
			Intents:              []BoardingIntent{intent},
			ClientTrees:          make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: make(map[wire.OutPoint]int),
			Musig2Sessions: make(
				map[SignerKey]*tree.SignerSession,
			),
		}
		h.withState(state)

		// Create a test signature with a fake txid.
		fakeTxid := chainhash.HashH([]byte("fake-tx"))

		event := &OperatorSigned{
			RoundID: testRoundIDTr("round-001"),
			AggSigs: map[tree.TxID]*schnorr.Signature{
				fakeTxid: {},
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
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:       testRoundIDTr("round-001"),
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   make(map[SignerKey]*tree.Tree),
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
			// Only provide one signature when more are needed.
			fakeTxid := chainhash.HashH([]byte("fake-tx"))
			event := &OperatorSigned{
				RoundID: testRoundIDTr("round-001"),
				AggSigs: map[tree.TxID]*schnorr.Signature{
					fakeTxid: {},
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
			testRoundIDTr("round-001"),
			[]BoardingIntent{intent},
		)
		h.withState(state)

		blockHash := chainhash.Hash{0x01, 0x02}
		event := &BoardingConfirmed{
			TxID:          state.CommitmentTx.UnsignedTx.TxHash(),
			BlockHeight:   101,
			BlockHash:     blockHash,
			Confirmations: 6,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		confirmedState := assertStateType[*ConfirmedState](h)
		require.Equal(t, int32(101), confirmedState.BlockHeight)
		require.Equal(t, blockHash, confirmedState.BlockHash)
		require.Equal(t, int32(6), confirmedState.Confirmations)

		h.assertOutboxLen(2)
		h.vtxoStore.AssertCalled(
			t, "SaveVTXOs", mock.Anything, mock.Anything,
		)
	})

	t.Run("buildClientVTXOs_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		vtxtTree, _ := h.newTestVTXOTree(1)
		intent := h.newTestBoardingIntent()

		// Empty ClientTrees will cause buildClientVTXOs to fail.
		state := &InputSigSentState{
			RoundID:       testRoundIDTr("round-001"),
			VTXOTreePaths: map[int]*tree.Tree{0: vtxtTree},
			Intents:       []BoardingIntent{intent},
			ClientTrees:   make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &BoardingConfirmed{
			TxID:          vtxtTree.BatchOutpoint.Hash,
			BlockHeight:   101,
			BlockHash:     chainhash.Hash{0x01},
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
		packet := h.newTestCommitmentTx(nil)

		result, err := validateBoardingInputs(
			packet.UnsignedTx, []BoardingIntent{},
		)

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "no boarding intents")
	})

	t.Run("missing_outpoint", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent := h.newTestBoardingIntent()

		// Create a different intent so the commitment tx has a
		// different outpoint than what we're validating.
		differentIntent := h.newTestBoardingIntent()
		packet := h.newTestCommitmentTx(
			[]BoardingIntent{differentIntent},
		)
		intents := []BoardingIntent{intent}

		result, err := validateBoardingInputs(
			packet.UnsignedTx, intents,
		)

		require.Error(t, err)
		require.Nil(t, result)
		require.Contains(t, err.Error(), "not found in commitment tx")
	})

	t.Run("success_single", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		intent := h.newTestBoardingIntent()
		intents := []BoardingIntent{intent}
		packet := h.newTestCommitmentTx(intents)

		result, err := validateBoardingInputs(
			packet.UnsignedTx, intents,
		)

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

		intents := []BoardingIntent{intent1, intent2, intent3}
		packet := h.newTestCommitmentTx(intents)

		result, err := validateBoardingInputs(
			packet.UnsignedTx, intents,
		)

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
	joinEvent := &RoundJoined{RoundID: testRoundIDTr("round-001")}
	_, err = h.sendEvent(joinEvent)
	require.NoError(t, err)

	joinedState := assertStateType[*RoundJoinedState](h)
	require.Equal(t, testRoundIDTr("round-001"), joinedState.RoundID)
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
	integrationRoundID := testRoundIDTr("round-integration-001")
	joinEvent := &RoundJoined{RoundID: integrationRoundID}
	_, err = h.sendEvent(joinEvent)
	require.NoError(t, err)

	joinedState := assertStateType[*RoundJoinedState](h)
	require.Equal(t, integrationRoundID, joinedState.RoundID)
}

func TestBoardingFlowRoundJoinedToPartialSigsSent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockWalletForMuSig2()

	intent := h.newTestBoardingIntent()

	h.withState(&RoundJoinedState{
		RoundID: testRoundIDTr("round-integration-002"),
		Intents: []BoardingIntent{intent},
	})

	// Step 1: RoundJoined → CommitmentTxReceived.
	vtxtTree := h.newTestVTXOTreeForIntent(intent)
	commitEvent := h.newCommitmentTxBuiltEvent(
		testRoundIDTr("round-integration-002"),
		[]BoardingIntent{intent},
		vtxtTree,
	)
	_, err := h.sendEvent(commitEvent)
	require.NoError(t, err)

	ctxReceivedState := assertStateType[*CommitmentTxReceivedState](h)
	require.NotNil(t, ctxReceivedState.CommitmentTx)

	// Step 2: CommitmentTxReceived → CommitmentTxValidated.
	_, err = h.sendEvent(&CommitmentTxBuilt{
		RoundID:       testRoundIDTr("round-integration-002"),
		Tx:            ctxReceivedState.CommitmentTx,
		VTXOTreePaths: ctxReceivedState.VTXOTreePaths,
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
		testRoundIDTr("round-integration-002"),
		noncesSentState.VTXOTreePaths[0],
	)
	_, err = h.sendEvent(aggNoncesEvent)
	require.NoError(t, err)

	noncesAggState := assertStateType[*NoncesAggregatedState](h)
	require.NotEmpty(t, noncesAggState.AggNonces)
	h.clearOutbox()

	// Step 5: NoncesAggregated → PartialSigsSent.
	_, err = h.sendEvent(&GeneratePartialSigs{})
	require.NoError(t, err)

	partialSigsState := assertStateType[*PartialSigsSentState](h)
	expectedID := testRoundIDTr("round-integration-002")
	require.Equal(t, expectedID, partialSigsState.RoundID)
}

func TestBoardingFlowFailureAndRecovery(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	h.withState(&RoundJoinedState{
		RoundID: testRoundIDTr("round-fail-001"),
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
