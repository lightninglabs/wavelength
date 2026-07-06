package round

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testRoundIDTr creates a deterministic RoundID from a string seed for tests.
func testRoundIDTr(seed string) RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return RoundID(id)
}

type reqTree struct {
	req  types.VTXORequest
	tree *tree.Tree
}

func mkReq(t *testing.T, op *btcec.PublicKey, seed byte, owned bool) reqTree {
	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	signingPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerPriv.PubKey(), op, testExitDelay,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(
		policyTemplate,
	)
	require.NoError(t, err)
	pkScript, err := template.PkScript()
	require.NoError(t, err)

	req := types.VTXORequest{
		Amount:         50000 + btcutil.Amount(seed),
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		Expiry:         testExitDelay,
		ClientKey:      ownerPriv.PubKey(),
		SigningKey: keychain.KeyDescriptor{
			PubKey: signingPriv.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOSigningKeyFamily,
				Index:  uint32(seed),
			},
		},
	}
	if owned {
		req.OwnerKey = keychain.KeyDescriptor{
			PubKey: ownerPriv.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: types.VTXOOwnerKeyFamily,
				Index:  uint32(100 + seed),
			},
		}
	}

	batchPkScript, err := txscript.PayToTaprootScript(
		op,
	)
	require.NoError(t, err)

	vtxoTree, err := tree.NewTree(
		wire.OutPoint{
			Hash:  chainhash.Hash{seed},
			Index: 0,
		},
		&wire.TxOut{
			Value:    int64(req.Amount),
			PkScript: batchPkScript,
		},
		[]tree.LeafDescriptor{{
			PkScript:    req.PkScript,
			Amount:      req.Amount,
			CoSignerKey: signingPriv.PubKey(),
		}},
		op, nil, 2,
	)
	require.NoError(t, err)

	return reqTree{
		req:  req,
		tree: vtxoTree,
	}
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
				"IntentSent", &IntentSentState{},
				"IntentSent",
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
				&ClientFailedState{
					Reason: "test",
				},
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
			event: &RoundJoined{
				RoundID: testRoundIDTr("test"),
			},
		},
		{
			name: "PendingAssembly_self_loops_on_RoundComplete",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)

				return &PendingRoundAssembly{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}
			},
			event: &RoundComplete{},
		},
		{
			name: "RegSent_self_loops_on_BoardingConfirmed",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &IntentSentState{
					Intents: intents,
				}
			},
			event: &BoardingConfirmed{},
		},
		{
			name: "RoundJoined_self_loops_on_duplicate_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &RoundJoinedState{
					RoundID: testRoundIDTr("round-1"),
					Intents: intents,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
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
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
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
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
		},
		{
			name: "NoncesSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &NoncesSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
		},
		{
			name: "NoncesAggregated_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &NoncesAggregatedState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
		},
		{
			name: "PartialSigsSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &PartialSigsSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
		},
		{
			name: "InputSigSent_self_loops_on_RoundJoined",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &InputSigSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("other"),
			},
		},
		{
			name: "ClientFailed_self_loops_on_unknown",
			setup: func(h *boardingTestHarness) ClientState {
				return &ClientFailedState{
					Reason:      "previous failure",
					Recoverable: true,
				}
			},
			event: &RoundJoined{
				RoundID: testRoundIDTr("test"),
			},
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
				t, initialState.String(),
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
			name: "IntentSentState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &IntentSentState{
					Intents: intents,
				}
			},
		},
		{
			name: "RoundJoinedState",
			setup: func(h *boardingTestHarness) ClientState {
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &RoundJoinedState{
					RoundID: testRoundIDTr("round-001"),
					Intents: intents,
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
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &NoncesSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
		},
		{
			name: "PartialSigsSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &PartialSigsSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
				}
			},
		},
		{
			name: "InputSigSentState",
			setup: func(h *boardingTestHarness) ClientState {
				vtxtTree, _ := h.newTestVTXOTree(1)
				intent := h.newTestBoardingIntent()
				vtxoReq := h.newTestVTXORequestForIntent(intent)
				roundID := testRoundIDTr("round-001")
				trees := map[int]*tree.Tree{
					0: vtxtTree,
				}
				intents := Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: []types.VTXORequest{
						vtxoReq,
					},
				}

				return &InputSigSentState{
					RoundID:       roundID,
					VTXOTreePaths: trees,
					Intents:       intents,
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
		{
			"ConfirmedState",
			&ConfirmedState{
				BlockHeight: 100,
			},
		},
		{
			"ClientFailedState",
			&ClientFailedState{
				Reason: "failed",
			},
		},
		{
			"RecoveryInitiatedState",
			&RecoveryInitiatedState{
				Outpoint: intent.Outpoint,
			},
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

	t.Run("IntentPackage_boarding_to_pending", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		event := &IntentPackage{Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
		}}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Boarding, 1)
	})

	t.Run("IntentPackage_resume_with_intents", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		event := &IntentPackage{Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
			VTXOs: []types.VTXORequest{
				vtxoReq,
			},
		}}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Boarding, 1)
	})

	t.Run("IntentPackage_empty_stays_idle", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&Idle{})

		event := &IntentPackage{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		_ = assertStateType[*Idle](h)
	})
}

func TestPendingRoundAssemblyState(t *testing.T) {
	t.Parallel()

	t.Run("additional_intent_package_accumulates", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		existingIntent := h.newTestBoardingIntent()
		existingVtxoReq := h.newTestVTXORequestForIntent(existingIntent)
		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{existingIntent},
			VTXOs:    []types.VTXORequest{existingVtxoReq},
		})

		newIntent := h.newTestBoardingIntent()
		event := &IntentPackage{Intents: Intents{
			Boarding: []BoardingIntent{
				newIntent,
			},
		}}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, nextState.Boarding, 2)
	})

	t.Run("registration_requested_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		})

		event := &IntentRequested{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		nextState := assertStateType[*IntentSentState](h)
		require.Len(t, nextState.Intents.Boarding, 1)
	})

	t.Run("no_intents_transitions_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{},
			VTXOs:    []types.VTXORequest{},
		})

		event := &IntentRequested{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// With no VTXOs, total output is zero which fails validation.
		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "no VTXO output amount")
	})

	t.Run("output_exceeds_input_fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Create intent with 50,000 sats.
		intent := h.newTestBoardingIntent()
		require.Equal(t, btcutil.Amount(50000), intent.ChainInfo.Amount)

		// Create VTXO request for 60,000 sats (more than input).
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxoReq.Amount = btcutil.Amount(60000)

		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		})

		event := &IntentRequested{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "outputs exceed inputs")
	})

	// NOTE: fee_exceeds_max_fails / fee_below_operator_minimum_fails
	// used to assert submit-time rejection against MaxOperatorFee /
	// MinOperatorFee. Under the #270 seal-time fee handshake those
	// checks have moved from PendingRoundAssembly (intent compose
	// time) to QuoteReceivedState (after the server issues a
	// JoinRoundQuote). See TestQuoteReceivedState_* for the
	// equivalent coverage.

	t.Run("fee_exactly_at_operator_minimum_succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set minimum operator fee to 5,000 sats.
		h.env.OperatorTerms.MinOperatorFee = btcutil.Amount(5000)

		// Create intent with 50,000 sats.
		intent := h.newTestBoardingIntent()
		require.Equal(t, btcutil.Amount(50000), intent.ChainInfo.Amount)

		// Create VTXO request for 45,000 sats, implying exactly
		// 5,000 sat fee (equal to minimum).
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxoReq.Amount = btcutil.Amount(45000)

		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		})

		event := &IntentRequested{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// Should succeed — fee is exactly at the minimum.
		nextState := assertStateType[*IntentSentState](h)
		require.Len(t, nextState.Intents.Boarding, 1)
	})

	t.Run("valid_fee_within_limit_succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Set max operator fee (10,000 sats).
		h.env.MaxOperatorFee = btcutil.Amount(10000)

		// Create intent with 50,000 sats.
		intent := h.newTestBoardingIntent()

		// Create VTXO request for 45,000 sats, implying 5,000 sat fee.
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxoReq.Amount = btcutil.Amount(45000)

		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		})

		event := &IntentRequested{}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// Should succeed and transition to IntentSentState.
		nextState := assertStateType[*IntentSentState](h)
		require.Len(t, nextState.Intents.Boarding, 1)
	})

	// Regression test for the auto-refresh single-marker bug
	// flagged on PR #298: prior to centralizing change-marker
	// designation, every output of a multi-VTXO refresh batch
	// (whether produced by buildVTXORequestFromRefresh on the
	// auto path or by handleRefreshVTXOs on the manual path)
	// carried IsChange=true. The composed JoinRoundRequest then
	// violated the proto contract's "exactly one marker" rule
	// for multi-output intents and the operator rejected the
	// round with INVALID_CHANGE_DESIGNATION.
	//
	// We drive PendingRoundAssembly with three refresh-shaped
	// VTXO requests that all leave IsChange unset (mirroring the
	// post-fix entry-point behavior) and assert that the
	// resulting IntentSentState carries exactly one marker.
	t.Run("multi_refresh_yields_single_change_marker", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		// Three boarding inputs to keep the input/output balance
		// sane: each refresh-style VTXO is funded by its own
		// forfeitable input. The exact provenance (forfeit vs
		// boarding) does not matter for the change-marker
		// invariant — what matters is that the composed intent
		// has multiple VTXO outputs and zero markers from source.
		intentA := h.newTestBoardingIntent()
		intentB := h.newTestBoardingIntent()
		intentC := h.newTestBoardingIntent()
		vtxoA := h.newTestVTXORequestForIntent(intentA)
		vtxoB := h.newTestVTXORequestForIntent(intentB)
		vtxoC := h.newTestVTXORequestForIntent(intentC)
		require.False(
			t, vtxoA.IsChange, "newTestVTXORequestForIntent "+
				"must leave IsChange unset post-fix",
		)
		require.False(t, vtxoB.IsChange)
		require.False(t, vtxoC.IsChange)

		h.withState(&PendingRoundAssembly{
			Boarding: []BoardingIntent{
				intentA, intentB, intentC,
			},
			VTXOs: []types.VTXORequest{
				vtxoA, vtxoB, vtxoC,
			},
		})

		_, err := h.sendEvent(&IntentRequested{})
		require.NoError(t, err)

		next := assertStateType[*IntentSentState](h)
		require.Len(
			t, next.Intents.VTXOs, 3,
			"composed intent must preserve all three VTXOs",
		)

		var markerCount int
		for _, req := range next.Intents.VTXOs {
			if req.IsChange {
				markerCount++
			}
		}
		require.Equal(
			t, 1, markerCount, "composed intent must carry "+
				"exactly one IsChange=true marker",
		)
		require.True(
			t, next.Intents.VTXOs[0].IsChange,
			"first VTXO must be the marker carrier",
		)
	})
}

func TestIntentSentState(t *testing.T) {
	t.Parallel()

	t.Run("RoundJoined_parks_state", func(t *testing.T) {
		t.Parallel()

		// Under the #270 seal-time handshake the server's admission
		// ack (RoundJoined) is a watermark only — the actor layer
		// uses it to re-key the FSM from the ephemeral temp key to
		// the server-assigned RoundID, but the state machine must
		// stay parked in IntentSentState until JoinRoundQuote
		// arrives. Transitioning out here would consume the state
		// before the quote handler runs. The transition does
		// however persist the admitted RoundID so the quote
		// handler can cross-check the server's claimed identity.
		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&IntentSentState{
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
		})

		admittedID := testRoundIDTr("test-round-123")
		event := &RoundJoined{RoundID: admittedID}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// FSM should remain in IntentSentState, preserving the
		// collected intents so the subsequent quote handler can
		// clone them into QuoteReceivedState. The admitted
		// RoundID is captured for downstream cross-checking.
		nextState := assertStateType[*IntentSentState](h)
		require.Len(t, nextState.Intents.Boarding, 1)
		require.Equal(t, admittedID, nextState.AdmittedRoundID)
	})

	// Quote events that arrive before the FSM has seen the
	// admission ack must fail loudly — the actor layer is
	// responsible for buffering pre-admission quotes, so reaching
	// the FSM in that state indicates a routing regression rather
	// than benign reordering.
	t.Run("quote_before_admission_fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&IntentSentState{
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
		})

		event := &JoinRoundQuoteReceived{
			RoundID: testRoundIDTr("test-quote-no-admit"),
			Quote:   &ClientQuote{},
		}

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(
			t, failedState.Reason, "quote arrived before admission",
		)
	})

	// A quote whose RoundID disagrees with the prior RoundJoined
	// ack is treated as a routing / server-trust violation —
	// signing against it would attribute the client's intent to
	// the wrong round.
	t.Run("quote_roundid_mismatch_fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		admittedID := testRoundIDTr("test-admitted")
		h.withState(&IntentSentState{
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
			AdmittedRoundID: admittedID,
		})

		event := &JoinRoundQuoteReceived{
			RoundID: testRoundIDTr("test-foreign"),
			Quote:   &ClientQuote{},
		}

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(
			t, failedState.Reason, "quote round_id mismatch",
		)
	})
}

func TestRoundJoinedState(t *testing.T) {
	t.Parallel()

	t.Run("CommitmentTxBuilt_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&RoundJoinedState{
			RoundID: testRoundIDTr("round-001"),
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
		})

		vtxtTree, _ := h.newTestVTXOTree(1)
		commitEvent := h.newCommitmentTxBuiltEvent(
			testRoundIDTr("round-001"), []BoardingIntent{intent},
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

	// A commitment-tx push whose RoundID disagrees with the
	// admitted RoundID is treated as a routing / server-trust
	// violation, mirroring the IntentSentState quote check.
	t.Run("CommitmentTxBuilt_round_id_mismatch", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&RoundJoinedState{
			RoundID: testRoundIDTr("admitted"),
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
		})

		vtxtTree, _ := h.newTestVTXOTree(1)
		commitEvent := h.newCommitmentTxBuiltEvent(
			testRoundIDTr("foreign"), []BoardingIntent{intent},
			vtxtTree,
		)

		_, err := h.sendEvent(commitEvent)
		require.NoError(t, err)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(
			t, failedState.Reason, "commitment round_id mismatch",
		)
	})

	// Reseal-after-accept: a fresh JoinRoundQuoteReceived with a
	// strictly higher SealPass reaches the FSM after the accept
	// has shipped. The FSM must walk back to QuoteReceivedState
	// and re-evaluate the new quote rather than self-loop.
	t.Run("reseal_after_accept_re_evaluates", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&RoundJoinedState{
			RoundID: testRoundIDTr("reseal-round"),
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
			Quote: &ClientQuote{SealPass: 1},
		})

		event := &JoinRoundQuoteReceived{
			RoundID: testRoundIDTr("reseal-round"),
			Quote: &ClientQuote{
				SealPass:       2,
				OperatorFeeSat: 500,
			},
		}

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		nextState := assertStateType[*QuoteReceivedState](h)
		require.Equal(t, uint32(2), nextState.Quote.SealPass)
		require.Equal(
			t, testRoundIDTr("reseal-round"), nextState.RoundID,
		)
	})

	// Stale reseal redeliveries (lower or equal SealPass) self-
	// loop without reverting state.
	t.Run("reseal_stale_pass_self_loops", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		h.withState(&RoundJoinedState{
			RoundID: testRoundIDTr("stale-round"),
			Intents: Intents{
				Boarding: []BoardingIntent{intent},
				VTXOs:    []types.VTXORequest{vtxoReq},
			},
			Quote: &ClientQuote{SealPass: 3},
		})

		event := &JoinRoundQuoteReceived{
			RoundID: testRoundIDTr("stale-round"),
			Quote: &ClientQuote{
				SealPass: 2,
			},
		}

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		// FSM stays in RoundJoinedState; no failure transition.
		_ = assertStateType[*RoundJoinedState](h)
	})
}

func TestCommitmentTxReceivedState(t *testing.T) {
	t.Parallel()

	t.Run("validation_succeeds", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		intents := []BoardingIntent{intent}
		vtxos := []types.VTXORequest{vtxoReq}
		vtxtTree := h.newTestVTXOTreeForIntents(vtxos)
		commitmentTx := h.bindTreeToCommitment(intents, vtxtTree)

		state := &CommitmentTxReceivedState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: intents,
				VTXOs:    vtxos,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			SweepDelay:  1008,
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID: testRoundIDTr("round-001"),
			Tx:      commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
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
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxos := []types.VTXORequest{vtxoReq}
		vtxtTree := h.newTestVTXOTreeForIntents(vtxos)

		// Create tx WITHOUT the intent's outpoint. Create a fake intent
		// with a different outpoint for the commitment tx.
		differentIntent := h.newTestBoardingIntent()
		differentIntents := []BoardingIntent{differentIntent}
		commitmentTx := h.bindTreeToCommitment(
			differentIntents, vtxtTree,
		)

		state := &CommitmentTxReceivedState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			SweepDelay:  1008,
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID: testRoundIDTr("round-001"),
			Tx:      commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "validation failed")
	})

	// quote_overrides_intent_target_for_change exercises the seal-
	// time amount-authority shift: the wallet packs the full input
	// value as vtxoReq.Amount on a change-marked VTXO request, the
	// server returns a residual via the quote, and on-chain the
	// tree leaf carries the residual amount. The FSM must compare
	// the leaf against the quote's amount, not the intent target.
	t.Run("quote_overrides_intent_target_for_change", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxoReq.IsChange = true

		// Server-decided residual (post-fee), strictly lower than
		// the intent target packed by the wallet.
		quotedAmount := int64(vtxoReq.Amount) - 1500
		require.Greater(t, quotedAmount, int64(0))

		// Build the tree with a leaf valued at the quote's amount,
		// not the intent target. Driving the tree builder via a
		// vtxoReq with the quoted Amount keeps the rest of the
		// leaf shape (script, cosigner key) consistent.
		vtxoReqForTree := vtxoReq
		vtxoReqForTree.Amount = btcutil.Amount(quotedAmount)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReqForTree},
		)

		intents := []BoardingIntent{intent}
		vtxos := []types.VTXORequest{vtxoReq}
		commitmentTx := h.bindTreeToCommitment(intents, vtxtTree)

		intentScript, err := vtxoReq.EffectivePkScript()
		require.NoError(t, err)

		state := &CommitmentTxReceivedState{
			RoundID:      testRoundIDTr("round-quote-override"),
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: intents,
				VTXOs:    vtxos,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			SweepDelay:  1008,
			Quote: &ClientQuote{
				OperatorFeeSat: 1500,
				VTXOQuotes: []VTXOQuoteEntry{{
					AmountSat: quotedAmount,
					PkScript:  intentScript,
					RecipientKey: vtxoReq.SigningKey.PubKey.
						SerializeCompressed(),
				}},
			},
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID: testRoundIDTr("round-quote-override"),
			Tx:      commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		validated := assertStateType[*CommitmentTxValidatedState](h)
		require.Equal(
			t, testRoundIDTr("round-quote-override"),
			validated.RoundID,
		)
	})

	// quote_mismatch_with_signed_leaf_rejects covers the malicious-
	// server case where the on-chain tree leaf does NOT carry the
	// amount the quote claimed. The FSM must reject rather than
	// sign whatever the server stamped into the tree.
	t.Run("quote_mismatch_with_signed_leaf_rejects", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxoReq.IsChange = true

		quotedAmount := int64(vtxoReq.Amount) - 1500
		// The server's signed tree leaf carries a different (lower)
		// amount than the quote advertised — the quote claims the
		// client gets `quotedAmount` while the leaf actually credits
		// less.
		leafAmount := quotedAmount - 1000
		require.Greater(t, leafAmount, int64(0))

		vtxoReqForTree := vtxoReq
		vtxoReqForTree.Amount = btcutil.Amount(leafAmount)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReqForTree},
		)

		intents := []BoardingIntent{intent}
		vtxos := []types.VTXORequest{vtxoReq}
		commitmentTx := h.bindTreeToCommitment(intents, vtxtTree)

		intentScript, err := vtxoReq.EffectivePkScript()
		require.NoError(t, err)

		state := &CommitmentTxReceivedState{
			RoundID:      testRoundIDTr("round-quote-mismatch"),
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: intents,
				VTXOs:    vtxos,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			SweepDelay:  1008,
			Quote: &ClientQuote{
				OperatorFeeSat: 1500,
				VTXOQuotes: []VTXOQuoteEntry{{
					AmountSat: quotedAmount,
					PkScript:  intentScript,
					RecipientKey: vtxoReq.SigningKey.PubKey.
						SerializeCompressed(),
				}},
			},
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID: testRoundIDTr("round-quote-mismatch"),
			Tx:      commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "validation failed")
	})

	// leave_output_validation_uses_quote_amount drives validateLeave
	// Outputs through the quote-aware path: the LeaveRequest carries
	// the intent target, the server's quote returns a residual, the
	// commitment-tx leave output reflects the quote, and the FSM
	// must accept the on-chain output even though it diverges from
	// the intent target.
	t.Run("leave_output_validation_uses_quote_amount", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		intents := []BoardingIntent{intent}
		vtxos := []types.VTXORequest{vtxoReq}
		vtxtTree := h.newTestVTXOTreeForIntents(vtxos)

		vtxoScript, err := vtxoReq.EffectivePkScript()
		require.NoError(t, err)

		const intentTargetValue = int64(50_000)
		const quotedLeaveValue = int64(48_500)
		leavePkScript := []byte{
			txscript.OP_1, 0x20,
		}
		leavePkScript = append(
			leavePkScript, bytes.Repeat([]byte{0xab}, 32)...,
		)

		// Build a commitment tx whose leave output carries the
		// quoted (post-fee) amount, not the intent target. The
		// stock helper hard-codes a single 100k output so we
		// stitch the leave output in by hand below.
		commitmentTx := h.bindTreeToCommitment(
			intents, vtxtTree, &wire.TxOut{
				Value:    quotedLeaveValue,
				PkScript: leavePkScript,
			},
		)

		leaves := []*types.LeaveRequest{{
			Output: &wire.TxOut{
				Value:    intentTargetValue,
				PkScript: leavePkScript,
			},
			IsChange: true,
		}}

		state := &CommitmentTxReceivedState{
			RoundID:      testRoundIDTr("round-leave-quote"),
			CommitmentTx: commitmentTx,
			TxID:         commitmentTx.UnsignedTx.TxHash(),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: intents,
				VTXOs:    vtxos,
				Leaves:   leaves,
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			SweepDelay:  1008,
			Quote: &ClientQuote{
				OperatorFeeSat: 1500,
				VTXOQuotes: []VTXOQuoteEntry{{
					AmountSat: int64(vtxoReq.Amount),
					PkScript:  vtxoScript,
					RecipientKey: vtxoReq.SigningKey.PubKey.
						SerializeCompressed(),
				}},
				LeaveQuotes: []LeaveQuoteEntry{{
					AmountSat: quotedLeaveValue,
					PkScript:  leavePkScript,
				}},
			},
		}
		h.withState(state)

		event := &CommitmentTxBuilt{
			RoundID: testRoundIDTr("round-leave-quote"),
			Tx:      commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		validated := assertStateType[*CommitmentTxValidatedState](h)
		require.Equal(
			t, testRoundIDTr("round-leave-quote"),
			validated.RoundID,
		)
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
			testRoundIDTr("round-001"), []BoardingIntent{intent},
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

	t.Run("leave_only_round_starts_forfeit_timeout", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()
		commitmentTx := h.newTestCommitmentTx(
			[]BoardingIntent{intent},
		)
		roundID := testRoundIDTr("round-leave-timeout")

		state := &CommitmentTxValidatedState{
			RoundID:       roundID,
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{},
			ForfeitKey:    h.forfeitPubKey,
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				Leaves: []*types.LeaveRequest{{
					Output: &wire.TxOut{
						Value: 40000,
						PkScript: []byte{
							0x51,
							0x20,
						},
					},
				}},
			},
			ClientTrees:          make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{},
			ForfeitMappings: map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{
						0x51,
						0x20,
					},
					ConnectorAmount: 546,
					VTXOAmount:      50000,
				},
			},
		}
		h.withState(state)

		transition, err := h.sendEvent(&GenerateNonces{})
		require.NoError(t, err)
		require.NotNil(t, transition)

		cs := assertStateType[*ForfeitSignaturesCollectingState](h)
		require.Equal(t, roundID, cs.RoundID)
		require.Len(t, cs.ExpectedForfeits, 1)

		h.assertOutboxContainsType("*round.ForfeitRequestToVTXO")
		h.assertOutboxContainsType("*round.StartTimeoutReq")

		// The collection timeout must be armed BEFORE any forfeit
		// request is dispatched. processOutbox aborts on the first
		// send error, so a trailing timeout would be skipped if a
		// per-VTXO forfeit Tell fails, stranding the round with no
		// timeout. See issue #386.
		h.assertOutboxFirstType("*round.StartTimeoutReq")
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
			testRoundIDTr("round-001"), []BoardingIntent{intent},
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
			testRoundIDTr("round-001"), []BoardingIntent{intent},
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
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)

		validSigs, err := h.generateValidTreeSignatures(vtxtTree)
		require.NoError(t, err)
		require.NotEmpty(t, validSigs)

		commitmentTx := h.newCommitmentTxForIntents(
			[]BoardingIntent{intent}, vtxtTree,
		)

		clientTrees := make(map[SignerKey]*tree.Tree)
		signerKey := NewSignerKey(vtxoReq.SigningKey.PubKey)
		clientTrees[signerKey] = vtxtTree

		state := &PartialSigsSentState{
			RoundID:      testRoundIDTr("round-real-sig-001"),
			CommitmentTx: commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: clientTrees,
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

	t.Run("OperatorSigned_propagates_sigs_to_extracted_client_tree",
		func(t *testing.T) {
			t.Parallel()

			h := newRealSigningTestHarness(t)

			intent := h.newTestBoardingIntentWithTapscript()
			vtxoReq := h.newTestVTXORequestForIntent(intent)
			vtxtTree := h.newTestVTXOTreeForIntents(
				[]types.VTXORequest{vtxoReq},
			)

			validSigs, err := h.generateValidTreeSignatures(
				vtxtTree,
			)
			require.NoError(t, err)
			require.NotEmpty(t, validSigs)

			commitmentTx := h.newCommitmentTxForIntents(
				[]BoardingIntent{intent}, vtxtTree,
			)

			roundVTXOReqs := []types.VTXORequest{vtxoReq}
			signerKey := NewSignerKey(
				roundVTXOReqs[0].SigningKey.PubKey,
			)

			clientTree, err := vtxtTree.ExtractPathForCoSigners(
				roundVTXOReqs[0].SigningKey.PubKey,
			)
			require.NoError(t, err)
			require.NotNil(t, clientTree)
			require.Error(t, clientTree.VerifySigned())

			state := &PartialSigsSentState{
				RoundID: testRoundIDTr(
					"round-client-tree-sigs",
				),
				CommitmentTx: commitmentTx,
				VTXOTreePaths: map[int]*tree.Tree{
					0: vtxtTree,
				},
				Intents: Intents{
					Boarding: []BoardingIntent{
						intent,
					},
					VTXOs: roundVTXOReqs,
				},
				ClientTrees: map[SignerKey]*tree.Tree{
					signerKey: clientTree,
				},
				BoardingInputIndices: map[wire.OutPoint]int{
					intent.Outpoint: 0,
				},
				Musig2Sessions: make(
					map[SignerKey]*tree.SignerSession,
				),
			}

			h.setupMockWalletForBoardingSigning()
			h.setupMockRoundStoreForCommit()
			h.withState(state)

			event := &OperatorSigned{
				RoundID: testRoundIDTr(
					"round-client-tree-sigs",
				),
				AggSigs: validSigs,
			}

			transition, err := h.sendEvent(event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			assertStateType[*InputSigSentState](
				h.boardingTestHarness,
			)
			require.NoError(t, clientTree.VerifySigned())
		})

	t.Run("OperatorSigned_starts_forfeit_timeout", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()
		commitmentTx := h.newTestCommitmentTx([]BoardingIntent{intent})
		roundID := testRoundIDTr("round-forfeit-timeout-start")

		state := &PartialSigsSentState{
			RoundID:       roundID,
			CommitmentTx:  commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{},
			ForfeitKey:    h.forfeitPubKey,
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
			BoardingInputIndices: map[wire.OutPoint]int{
				intent.Outpoint: 0,
			},
			ForfeitMappings: map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{
						0x51,
						0x20,
					},
					ConnectorAmount: 546,
					VTXOAmount:      50000,
				},
			},
		}
		h.withState(state)

		event := &OperatorSigned{
			RoundID: roundID,
			AggSigs: map[tree.TxID]*schnorr.Signature{},
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		cs := assertStateType[*ForfeitSignaturesCollectingState](h)
		require.Equal(t, roundID, cs.RoundID)
		require.Len(t, cs.ExpectedForfeits, 1)

		h.assertOutboxContainsType("*round.ForfeitRequestToVTXO")
		h.assertOutboxContainsType("*round.StartTimeoutReq")

		// The collection timeout must be armed BEFORE any forfeit
		// request is dispatched. processOutbox aborts on the first
		// send error, so a trailing timeout would be skipped if a
		// per-VTXO forfeit Tell fails, stranding the round with no
		// timeout. See issue #386.
		h.assertOutboxFirstType("*round.StartTimeoutReq")
	})

	t.Run("empty_signatures_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
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
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
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
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
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
		vtxoReq := h.newTestVTXORequestForIntent(intent)
		vtxtTree := h.newTestVTXOTreeForIntents(
			[]types.VTXORequest{vtxoReq},
		)
		intents := []BoardingIntent{intent}
		commitmentTx := h.newTestCommitmentTx(intents)

		state := &PartialSigsSentState{
			RoundID:      testRoundIDTr("round-001"),
			CommitmentTx: commitmentTx,
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
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
			testRoundIDTr("round-001"), []BoardingIntent{intent},
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

	t.Run("foreign_VTXOs_not_persisted", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockVTXOStoreForSave()

		intent := h.newTestBoardingIntent()
		state := h.newInputSigSentState(
			testRoundIDTr("round-foreign"),
			[]BoardingIntent{intent},
		)

		// Simulate a foreign-owned VTXO: drop the local owner
		// descriptor entirely, and install a checker that also
		// rejects the pkScript.
		state.Intents.VTXOs[0].OwnerKey = keychain.KeyDescriptor{}
		h.env.OwnedScriptChecker = newMockOwnedScriptChecker()

		h.withState(state)

		event := &BoardingConfirmed{
			TxID:        state.CommitmentTx.UnsignedTx.TxHash(),
			BlockHeight: 101,
			BlockHash: chainhash.Hash{
				0x01,
			},
			Confirmations: 6,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		_ = assertStateType[*ConfirmedState](h)

		h.assertOutboxLen(2)
		created, ok := transition.NewEvents.UnwrapOr(
			ClientEmittedEvent{},
		).Outbox[0].(*VTXOCreatedNotification)
		require.True(t, ok)
		require.Empty(t, created.VTXOs)
		require.Len(t, created.Outflows, 1)
		h.vtxoStore.AssertNotCalled(t, "SaveVTXOs")
	})
	t.Run("buildClientVTXOs_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		vtxtTree, _ := h.newTestVTXOTree(1)
		intent := h.newTestBoardingIntent()
		vtxoReq := h.newTestVTXORequestForIntent(intent)

		// Empty ClientTrees will cause buildClientVTXOs to fail.
		state := &InputSigSentState{
			RoundID: testRoundIDTr("round-001"),
			VTXOTreePaths: map[int]*tree.Tree{
				0: vtxtTree,
			},
			Intents: Intents{
				Boarding: []BoardingIntent{
					intent,
				},
				VTXOs: []types.VTXORequest{
					vtxoReq,
				},
			},
			ClientTrees: make(map[SignerKey]*tree.Tree),
		}
		h.withState(state)

		event := &BoardingConfirmed{
			TxID:        vtxtTree.BatchOutpoint.Hash,
			BlockHeight: 101,
			BlockHash: chainhash.Hash{
				0x01,
			},
			Confirmations: 6,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "build client VTXOs")
	})

	t.Run("buildClientVTXOs_skips_foreign_outputs", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		foreign := mkReq(t, h.operatorPubKey, 1, false)
		owned := mkReq(t, h.operatorPubKey, 2, true)
		foreignReq, foreignTree := foreign.req, foreign.tree
		ownedReq, ownedTree := owned.req, owned.tree

		vtxos, err := buildClientVTXOs(
			t.Context(), nil,
			Intents{
				VTXOs: []types.VTXORequest{
					foreignReq, ownedReq,
				},
			},
			map[SignerKey]*tree.Tree{
				NewSignerKey(
					foreignReq.SigningKey.PubKey,
				): foreignTree,
				NewSignerKey(
					ownedReq.SigningKey.PubKey,
				): ownedTree,
			},
			testRoundIDTr("owned-only"),
		)
		require.NoError(t, err)
		require.Len(t, vtxos, 1)
		require.Equal(t, ownedReq.Amount, vtxos[0].Amount)
		require.True(
			t, vtxos[0].OwnerKey.PubKey.IsEqual(
				ownedReq.OwnerKey.PubKey,
			),
		)
		require.Equal(
			t, ownedReq.OwnerKey.Family, vtxos[0].OwnerKey.Family,
		)
		require.Equal(
			t, ownedReq.OwnerKey.Index, vtxos[0].OwnerKey.Index,
		)
	})

	t.Run("buildClientVTXOs_keeps_zero_locator_owner", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		local := mkReq(t, h.operatorPubKey, 3, true)
		local.req.OwnerKey.KeyLocator = keychain.KeyLocator{}

		vtxos, err := buildClientVTXOs(
			t.Context(), nil,
			Intents{VTXOs: []types.VTXORequest{local.req}},
			map[SignerKey]*tree.Tree{NewSignerKey(
				local.req.SigningKey.PubKey,
			): local.tree},
			testRoundIDTr("owned-zero-locator"),
		)
		require.NoError(t, err)
		require.Len(t, vtxos, 1)
		require.True(
			t, vtxos[0].OwnerKey.PubKey.IsEqual(
				local.req.OwnerKey.PubKey,
			),
		)
	})

	t.Run("buildClientVTXOs_keeps_distinct_owner_and_signing_keys",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)

			local := mkReq(t, h.operatorPubKey, 4, true)
			require.NotNil(t, local.req.OwnerKey.PubKey)
			require.NotNil(t, local.req.SigningKey.PubKey)
			require.False(
				t, local.req.OwnerKey.PubKey.IsEqual(
					local.req.SigningKey.PubKey,
				),
			)

			vtxos, err := buildClientVTXOs(
				t.Context(), nil,
				Intents{VTXOs: []types.VTXORequest{local.req}},
				map[SignerKey]*tree.Tree{NewSignerKey(
					local.req.SigningKey.PubKey,
				): local.tree},
				testRoundIDTr("owned-distinct-signer"),
			)
			require.NoError(t, err)
			require.Len(t, vtxos, 1)
			require.True(
				t, vtxos[0].OwnerKey.PubKey.IsEqual(
					local.req.OwnerKey.PubKey,
				),
			)
			require.False(
				t, vtxos[0].OwnerKey.PubKey.IsEqual(
					local.req.SigningKey.PubKey,
				),
			)

			expectedOutpoint, err := local.tree.Root.
				GetLeafNodes()[0].GetNonAnchorOutpoint()
			require.NoError(t, err)
			require.Equal(t, *expectedOutpoint, vtxos[0].Outpoint)
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
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	h.withState(&PendingRoundAssembly{
		Boarding: []BoardingIntent{intent},
		VTXOs:    []types.VTXORequest{vtxoReq},
	})

	// Step 1: Request registration.
	regEvent := &IntentRequested{}
	_, err := h.sendEvent(regEvent)
	require.NoError(t, err)

	regState := assertStateType[*IntentSentState](h)
	require.Len(t, regState.Intents.Boarding, 1)

	// Step 2: Server admission watermark. Under the #270 seal-time
	// handshake the FSM stays in IntentSentState until the quote
	// arrives — RoundJoined is consumed at the actor layer for
	// re-keying, not here.
	joinEvent := &RoundJoined{RoundID: testRoundIDTr("round-001")}
	_, err = h.sendEvent(joinEvent)
	require.NoError(t, err)

	assertStateType[*IntentSentState](h)
}

func TestBoardingFlowMultipleIntentsAccumulation(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.withState(&Idle{})

	// We send multiple intent packages to verify that
	// PendingRoundAssembly correctly accumulates boarding intents
	// as they arrive, rather than replacing or dropping previous
	// ones.
	for i := 0; i < 3; i++ {
		intent := h.newTestBoardingIntent()
		event := &IntentPackage{Intents: Intents{
			Boarding: []BoardingIntent{
				intent,
			},
		}}

		_, err := h.sendEvent(event)
		require.NoError(t, err)

		state := assertStateType[*PendingRoundAssembly](h)
		require.Len(t, state.Boarding, i+1)
	}
}

func TestBoardingFlowPendingToRoundJoined(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	h.withState(&PendingRoundAssembly{
		Boarding: []BoardingIntent{intent},
		VTXOs:    []types.VTXORequest{vtxoReq},
	})

	// Step 1: PendingRoundAssembly → IntentSentState.
	regEvent := &IntentRequested{}
	_, err := h.sendEvent(regEvent)
	require.NoError(t, err)

	intentSent := assertStateType[*IntentSentState](h)
	require.Len(t, intentSent.Intents.Boarding, 1)

	// Step 2: server admission ack (RoundJoined) must park the FSM
	// in IntentSentState under the #270 handshake — the state
	// machine advances only once the quote is evaluated.
	integrationRoundID := testRoundIDTr("round-integration-001")
	_, err = h.sendEvent(&RoundJoined{RoundID: integrationRoundID})
	require.NoError(t, err)
	assertStateType[*IntentSentState](h)

	// Step 3: a server-issued quote flips us into QuoteReceivedState.
	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}
	quote := &ClientQuote{
		QuoteID:        quoteID,
		SealPass:       1,
		OperatorFeeSat: 1_000,
	}
	_, err = h.sendEvent(&JoinRoundQuoteReceived{
		RoundID: integrationRoundID,
		Quote:   quote,
	})
	require.NoError(t, err)
	assertStateType[*QuoteReceivedState](h)

	// Step 4: QuoteAccepted drives the transition to
	// RoundJoinedState and emits the JoinRoundAccept outbox.
	_, err = h.sendEvent(&QuoteAccepted{
		RoundID: integrationRoundID,
		QuoteID: quoteID,
	})
	require.NoError(t, err)

	joinedState := assertStateType[*RoundJoinedState](h)
	require.Equal(t, integrationRoundID, joinedState.RoundID)
	require.NotNil(t, joinedState.Quote)
}

func TestBoardingFlowRoundJoinedToPartialSigsSent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockWalletForMuSig2()

	intent := h.newTestBoardingIntent()
	vtxoReq := h.newTestVTXORequestForIntent(intent)

	h.withState(&RoundJoinedState{
		RoundID: testRoundIDTr("round-integration-002"),
		Intents: Intents{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		},
	})

	// Step 1: RoundJoined → CommitmentTxReceived.
	vtxtTree := h.newTestVTXOTreeForIntents([]types.VTXORequest{vtxoReq})
	commitEvent := h.newCommitmentTxBuiltEvent(
		testRoundIDTr("round-integration-002"),
		[]BoardingIntent{intent}, vtxtTree,
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
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	h.withState(&RoundJoinedState{
		RoundID: testRoundIDTr("round-fail-001"),
		Intents: Intents{
			Boarding: []BoardingIntent{intent},
			VTXOs:    []types.VTXORequest{vtxoReq},
		},
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

// TestForfeitSignaturesCollectingState tests the forfeit signature collection
// flow which is used when VTXOs are being refreshed in a batch swap round.
func TestForfeitSignaturesCollectingState(t *testing.T) {
	t.Parallel()

	t.Run("single_forfeit_collection", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockWalletForBoardingSigning()
		h.setupMockRoundStoreForCheckpoint()

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()
		serverForfeitScript := h.forfeitScript()

		// Build a valid forfeit tx structure for validation.
		forfeitTx := h.newTestForfeitTx(
			vtxoOutpoint, connectorOutpoint, serverForfeitScript,
		)

		roundID := testRoundIDTr("round-forfeit-001")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		// Send forfeit signature response.
		sig := testutils.TestSchnorrSignature(t, "forfeit")
		event := &ForfeitSignatureResponse{
			VTXOOutpoint: vtxoOutpoint,
			RoundID:      roundID.String(),
			ForfeitTx:    forfeitTx,
			Signature:    sig,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// Should transition to InputSigSentState. In the new flow,
		// forfeit collection happens AFTER VTXO tree signing, so
		// completing forfeits goes directly to InputSigSentState.
		inputSigState := assertStateType[*InputSigSentState](h)
		require.Equal(t, roundID, inputSigState.RoundID)
		require.Len(t, inputSigState.ForfeitedVTXOs, 1)
		require.Equal(t, vtxoOutpoint, inputSigState.ForfeitedVTXOs[0])

		// Should emit SubmitVTXOForfeitSigsToServer and
		// SubmitForfeitSigRequest (boarding input signatures).
		forfeitType := "*round.SubmitVTXOForfeitSigsToServer"
		h.assertOutboxContainsType(forfeitType)
		h.assertOutboxContainsType("*round.SubmitForfeitSigRequest")
		h.assertOutboxContainsType("*round.CancelTimeoutReq")
	})

	t.Run("forfeit_only_collection_skips_empty_boarding_sig_submit",
		func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			h.setupMockWalletForBoardingSigning()
			h.setupMockRoundStoreForCheckpoint()

			vtxoOutpoint := h.newTestOutpoint()
			connectorOutpoint := h.newTestOutpoint()
			serverForfeitScript := h.forfeitScript()

			forfeitTx := h.newTestForfeitTx(
				vtxoOutpoint, connectorOutpoint,
				serverForfeitScript,
			)

			roundID := testRoundIDTr("round-forfeit-only-001")
			connectorInfo := &ConnectorLeafInfo{
				LeafIndex:         0,
				ConnectorOutpoint: connectorOutpoint,
				ConnectorPkScript: []byte{
					0x51, 0x20,
				},
				ConnectorAmount: 546,
				VTXOAmount:      50000,
			}
			state := h.newForfeitCollectingState(
				roundID,
				Intents{},
				map[wire.OutPoint]*ConnectorLeafInfo{
					vtxoOutpoint: connectorInfo,
				},
			)
			h.withState(state)

			sig := testutils.TestSchnorrSignature(t, "forfeit-only")
			event := &ForfeitSignatureResponse{
				VTXOOutpoint: vtxoOutpoint,
				RoundID:      roundID.String(),
				ForfeitTx:    forfeitTx,
				Signature:    sig,
			}

			transition, err := h.sendEvent(event)
			require.NoError(t, err)
			require.NotNil(t, transition)

			inputSigState := assertStateType[*InputSigSentState](h)
			require.Equal(t, roundID, inputSigState.RoundID)
			require.Len(t, inputSigState.ForfeitedVTXOs, 1)
			require.Equal(
				t, vtxoOutpoint,
				inputSigState.ForfeitedVTXOs[0],
			)

			h.assertOutboxContainsType(
				"*round.SubmitVTXOForfeitSigsToServer",
			)
			h.assertOutboxContainsType("*round.CancelTimeoutReq")

			for _, msg := range h.outboxMessages {
				require.NotEqual(
					t, "*round.SubmitForfeitSigRequest",
					fmt.Sprintf("%T", msg),
				)
			}
		})

	t.Run("multiple_forfeits_wait_for_all", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)
		h.setupMockWalletForBoardingSigning()
		h.setupMockRoundStoreForCheckpoint()

		intent := h.newTestBoardingIntent()
		vtxoOutpoint1 := h.newTestOutpoint()
		vtxoOutpoint2 := h.newTestOutpoint()
		connectorOutpoint1 := h.newTestOutpoint()
		connectorOutpoint2 := h.newTestOutpoint()
		serverForfeitScript := h.forfeitScript()

		roundID := testRoundIDTr("round-forfeit-002")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint1: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint1,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
				vtxoOutpoint2: {
					LeafIndex:         1,
					ConnectorOutpoint: connectorOutpoint2,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		// Send first forfeit - should stay in state.
		forfeitTx1 := h.newTestForfeitTx(
			vtxoOutpoint1, connectorOutpoint1, serverForfeitScript,
		)
		sig1 := testutils.TestSchnorrSignature(t, "forfeit")
		event1 := &ForfeitSignatureResponse{
			VTXOOutpoint: vtxoOutpoint1,
			RoundID:      roundID.String(),
			ForfeitTx:    forfeitTx1,
			Signature:    sig1,
		}

		_, err := h.sendEvent(event1)
		require.NoError(t, err)

		// Should still be in ForfeitSignaturesCollectingState.
		//nolint:ll
		collectingState := assertStateType[*ForfeitSignaturesCollectingState](
			h,
		)
		require.Len(t, collectingState.CollectedForfeits, 1)

		// Send second forfeit - should transition.
		forfeitTx2 := h.newTestForfeitTx(
			vtxoOutpoint2, connectorOutpoint2, serverForfeitScript,
		)
		sig2 := testutils.TestSchnorrSignature(t, "forfeit")
		event2 := &ForfeitSignatureResponse{
			VTXOOutpoint: vtxoOutpoint2,
			RoundID:      roundID.String(),
			ForfeitTx:    forfeitTx2,
			Signature:    sig2,
		}

		transition, err := h.sendEvent(event2)
		require.NoError(t, err)
		require.NotNil(t, transition)

		// Now should transition to InputSigSentState.
		inputSigState := assertStateType[*InputSigSentState](h)
		require.Len(t, inputSigState.ForfeitedVTXOs, 2)
	})

	t.Run("duplicate_forfeit_ignored", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()
		serverForfeitScript := h.forfeitScript()

		// We need 2 expected forfeits to stay in state after first.
		vtxoOutpoint2 := h.newTestOutpoint()
		connectorOutpoint2 := h.newTestOutpoint()

		roundID := testRoundIDTr("round-forfeit-003")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
				vtxoOutpoint2: {
					LeafIndex:         1,
					ConnectorOutpoint: connectorOutpoint2,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		forfeitTx := h.newTestForfeitTx(
			vtxoOutpoint, connectorOutpoint, serverForfeitScript,
		)
		sigDup := testutils.TestSchnorrSignature(t, "forfeit")
		event := &ForfeitSignatureResponse{
			VTXOOutpoint: vtxoOutpoint,
			RoundID:      roundID.String(),
			ForfeitTx:    forfeitTx,
			Signature:    sigDup,
		}

		// First response.
		_, err := h.sendEvent(event)
		require.NoError(t, err)

		//nolint:ll
		collectingState := assertStateType[*ForfeitSignaturesCollectingState](
			h,
		)
		require.Len(t, collectingState.CollectedForfeits, 1)

		// Duplicate response - should be ignored.
		_, err = h.sendEvent(event)
		require.NoError(t, err)

		//nolint:ll
		collectingState = assertStateType[*ForfeitSignaturesCollectingState](
			h,
		)
		require.Len(t, collectingState.CollectedForfeits, 1)
	})

	t.Run("unexpected_forfeit_error", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()

		roundID := testRoundIDTr("round-forfeit-004")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		// Send forfeit for unknown VTXO.
		unknownOutpoint := h.newTestOutpoint()
		sigUnknown := testutils.TestSchnorrSignature(t, "forfeit")
		event := &ForfeitSignatureResponse{
			VTXOOutpoint: unknownOutpoint,
			RoundID:      roundID.String(),
			Signature:    sigUnknown,
		}

		_, err := h.sendEvent(event)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unexpected forfeit")
	})

	t.Run("boarding_failed_transitions", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()

		roundID := testRoundIDTr("round-forfeit-005")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		event := &BoardingFailed{
			Reason:      "server disconnected",
			Recoverable: true,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Equal(t, "server disconnected", failedState.Reason)
		require.True(t, failedState.Recoverable)
		h.assertOutboxContainsType("*round.CancelTimeoutReq")
	})

	t.Run("timeout_transitions_to_failed", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()

		roundID := testRoundIDTr("round-forfeit-timeout")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		event := &ForfeitCollectionTimedOut{
			RoundID: roundID,
		}

		transition, err := h.sendEvent(event)
		require.NoError(t, err)
		require.NotNil(t, transition)

		failedState := assertStateType[*ClientFailedState](h)
		require.Contains(t, failedState.Reason, "forfeit signature")
		require.Contains(t, failedState.Reason, "timeout")
		require.True(t, failedState.Recoverable)
		h.assertOutboxContainsType("*round.CancelTimeoutReq")
	})

	t.Run("forfeit_amount_mismatch_fails", func(t *testing.T) {
		t.Parallel()

		h := newTestHarness(t)

		intent := h.newTestBoardingIntent()
		vtxoOutpoint := h.newTestOutpoint()
		connectorOutpoint := h.newTestOutpoint()
		serverForfeitScript := h.forfeitScript()

		// Create forfeit tx with 40000 sats (mismatch).
		forfeitTx := wire.NewMsgTx(2)
		forfeitTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: vtxoOutpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
		forfeitTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: connectorOutpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
		forfeitTx.AddTxOut(&wire.TxOut{
			Value:    40000, // Wrong amount - expected 50000.
			PkScript: serverForfeitScript,
		})
		forfeitTx.AddTxOut(&wire.TxOut{
			Value:    0,
			PkScript: arkscript.AnchorPkScript,
		})

		// Create state expecting 50000 sats.
		roundID := testRoundIDTr("round-forfeit-amount")
		state := h.newForfeitCollectingState(
			roundID,
			Intents{Boarding: []BoardingIntent{intent}},
			map[wire.OutPoint]*ConnectorLeafInfo{
				vtxoOutpoint: {
					LeafIndex:         0,
					ConnectorOutpoint: connectorOutpoint,
					ConnectorPkScript: []byte{0x51, 0x20},
					ConnectorAmount:   546,
					VTXOAmount:        50000,
				},
			},
		)
		h.withState(state)

		sigPenalty := testutils.TestSchnorrSignature(t, "forfeit")
		event := &ForfeitSignatureResponse{
			VTXOOutpoint: vtxoOutpoint,
			RoundID:      roundID.String(),
			ForfeitTx:    forfeitTx,
			Signature:    sigPenalty,
		}

		_, err := h.sendEvent(event)
		require.Error(t, err)
		require.Contains(t, err.Error(), "forfeit tx penalty output")
		require.Contains(t, err.Error(), "amount")
	})
}

// TestForfeitMappingsCarriedThroughSigningStates verifies that ForfeitMappings
// is carried through NoncesSent, NoncesAggregated, and PartialSigsSent states
// so that forfeit signature collection can happen after VTXO tree signing.
func TestForfeitMappingsCarriedThroughSigningStates(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockWalletForMuSig2()

	intent := h.newTestBoardingIntent()
	vtxoOutpoint := h.newTestOutpoint()
	connectorOutpoint := h.newTestOutpoint()

	// Build forfeit mappings for a refresh round.
	forfeitMappings := map[wire.OutPoint]*ConnectorLeafInfo{
		vtxoOutpoint: {
			LeafIndex:         0,
			ConnectorOutpoint: connectorOutpoint,
			ConnectorPkScript: []byte{
				0x51,
				0x20,
			},
			ConnectorAmount: 546,
			VTXOAmount:      50000,
		},
	}

	// Start from CommitmentTxValidatedState with ForfeitMappings populated.
	roundID := testRoundID("round-carry-001")
	validatedState := h.newCommitmentTxValidatedState(
		roundID, []BoardingIntent{intent},
	)
	validatedState.ForfeitMappings = forfeitMappings
	h.withState(validatedState)

	// Generate nonces.
	_, err := h.sendEvent(&GenerateNonces{})
	require.NoError(t, err)
	h.clearOutbox()

	// Check NoncesSentState has ForfeitMappings.
	noncesSentState := assertStateType[*NoncesSentState](h)
	require.Len(t, noncesSentState.ForfeitMappings, 1)
	require.Contains(t, noncesSentState.ForfeitMappings, vtxoOutpoint)

	// Send aggregated nonces.
	aggEvent := h.newNoncesAggregatedEvent(
		roundID, noncesSentState.VTXOTreePaths[0],
	)
	_, err = h.sendEvent(aggEvent)
	require.NoError(t, err)
	h.clearOutbox()

	// Check NoncesAggregatedState has ForfeitMappings.
	noncesAggState := assertStateType[*NoncesAggregatedState](h)
	require.Len(t, noncesAggState.ForfeitMappings, 1)
	require.Contains(t, noncesAggState.ForfeitMappings, vtxoOutpoint)
}

// TestInputSigSentStateEmitsForfeitConfirmed verifies that when
// BoardingConfirmed is received in InputSigSentState, ForfeitConfirmedToVTXO
// messages are emitted for all forfeited VTXOs.
func TestInputSigSentStateEmitsForfeitConfirmed(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.setupMockVTXOStoreForSave()

	intent := h.newTestBoardingIntent()
	vtxoOutpoint1 := h.newTestOutpoint()
	vtxoOutpoint2 := h.newTestOutpoint()

	roundID := testRoundID("round-forfeit-confirm")
	state := h.newInputSigSentState(roundID, []BoardingIntent{intent})
	state.ForfeitedVTXOs = []wire.OutPoint{vtxoOutpoint1, vtxoOutpoint2}
	h.withState(state)

	event := &BoardingConfirmed{
		TxID:          state.CommitmentTx.UnsignedTx.TxHash(),
		BlockHeight:   101,
		Confirmations: 6,
	}

	_, err := h.sendEvent(event)
	require.NoError(t, err)

	_ = assertStateType[*ConfirmedState](h)

	// Should emit ForfeitConfirmedToVTXO for each forfeited VTXO.
	forfeitConfirmCount := 0
	for _, msg := range h.outboxMessages {
		if forfeitMsg, ok := msg.(*ForfeitConfirmedToVTXO); ok {
			forfeitConfirmCount++
			require.Equal(t, int32(101), forfeitMsg.BlockHeight)
		}
	}
	require.Equal(t, 2, forfeitConfirmCount)
}

// TestStatePropertiesForfeitState verifies ForfeitSignaturesCollectingState
// properties.
func TestStatePropertiesForfeitState(t *testing.T) {
	t.Parallel()

	state := &ForfeitSignaturesCollectingState{
		RoundID: testRoundID("round-001"),
	}

	require.False(t, state.IsTerminal())
	require.Equal(t, "ForfeitSignaturesCollecting", state.String())
}

// TestForfeitCollectionStateImmutability verifies that when a forfeit
// signature response is received, the FSM creates a new state object rather
// than mutating the existing one. This is important for FSM correctness.
func TestForfeitCollectionStateImmutability(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	vtxoOutpoint1 := h.newTestOutpoint()
	vtxoOutpoint2 := h.newTestOutpoint()
	connectorOutpoint1 := h.newTestOutpoint()
	connectorOutpoint2 := h.newTestOutpoint()
	serverForfeitScript := h.forfeitScript()

	roundID := testRoundIDTr("round-immut-001")
	state := h.newForfeitCollectingState(
		roundID,
		Intents{Boarding: []BoardingIntent{intent}},
		map[wire.OutPoint]*ConnectorLeafInfo{
			vtxoOutpoint1: {
				LeafIndex:         0,
				ConnectorOutpoint: connectorOutpoint1,
				ConnectorPkScript: []byte{0x51, 0x20},
				ConnectorAmount:   546,
				VTXOAmount:        50000,
			},
			vtxoOutpoint2: {
				LeafIndex:         1,
				ConnectorOutpoint: connectorOutpoint2,
				ConnectorPkScript: []byte{0x51, 0x20},
				ConnectorAmount:   546,
				VTXOAmount:        50000,
			},
		},
	)

	// Save original state's CollectedForfeits map reference.
	originalMap := state.CollectedForfeits
	originalMapLen := len(originalMap)
	h.withState(state)

	// Send first forfeit - should NOT mutate original state's map.
	forfeitTx1 := h.newTestForfeitTx(
		vtxoOutpoint1, connectorOutpoint1, serverForfeitScript,
	)
	event1 := &ForfeitSignatureResponse{
		VTXOOutpoint: vtxoOutpoint1,
		RoundID:      "round-immut-001",
		ForfeitTx:    forfeitTx1,
		Signature:    testutils.TestSchnorrSignature(t, "forfeit"),
	}

	_, err := h.sendEvent(event1)
	require.NoError(t, err)

	// The ORIGINAL state's map should NOT have been modified.
	require.Len(
		t, originalMap, originalMapLen,
		"original state's CollectedForfeits map was mutated",
	)

	// The NEW state should have the collected forfeit.
	newState := assertStateType[*ForfeitSignaturesCollectingState](h)
	require.Len(t, newState.CollectedForfeits, 1)

	// Verify the new state is a different object by checking it has the
	// updated forfeit while the original doesn't.
	_, hasInNew := newState.CollectedForfeits[vtxoOutpoint1]
	require.True(t, hasInNew, "new state should have the forfeit")

	_, hasInOriginal := originalMap[vtxoOutpoint1]
	require.False(t, hasInOriginal, "original map should not have forfeit")
}

// TestRefreshOnlyRoundValidation verifies that rounds containing only refresh
// requests (no boarding intents) can successfully pass commitment validation.
func TestRefreshOnlyRoundValidation(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Create a state with NO boarding intents (refresh-only round).
	emptyIntents := Intents{Boarding: []BoardingIntent{}}
	roundID := testRoundIDTr("round-refresh-001")

	// Create a VTXT tree. For refresh-only rounds, this would contain
	// VTXOs for the refreshed amounts, but we need a minimal valid tree.
	vtxtTree := h.newMinimalVTXOTree()

	// Bind the tree to a commitment tx with no boarding inputs.
	commitmentTx := h.bindTreeToCommitment(nil, vtxtTree)

	state := &CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		TxID:         commitmentTx.UnsignedTx.TxHash(),
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents:     emptyIntents,
		ClientTrees: make(map[SignerKey]*tree.Tree),
		SweepDelay:  1008,
	}
	h.withState(state)

	// No forfeit mappings for this simple test - just validating the
	// round can proceed without boarding intents.
	event := &CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
	}

	transition, err := h.sendEvent(event)
	require.NoError(t, err)
	require.NotNil(t, transition)

	// Should transition to CommitmentTxValidatedState, not fail.
	validatedState := assertStateType[*CommitmentTxValidatedState](h)
	require.Equal(t, roundID, validatedState.RoundID)

	// BoardingInputIndices should be empty (no boarding inputs).
	require.Empty(t, validatedState.BoardingInputIndices)

	// Should emit GenerateNonces internal event.
	assertTransitionEmitsInternalEvent[*GenerateNonces](h, transition)
}
