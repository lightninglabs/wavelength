package round

import (
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
)

// ClientTransitionEntry is a type alias for the round FSM transition entry.
type ClientTransitionEntry = protofsm.TransitionEntry[
	ClientState, ClientEvent, ClientOutMsg,
]

// ClientStateTransitions is a type alias for the round FSM state transitions.
type ClientStateTransitions = protofsm.StateTransitions[
	ClientState, ClientEvent, ClientOutMsg,
]

// ClientTransitionTable is a type alias for the round FSM transition table.
type ClientTransitionTable = protofsm.TransitionTable[
	ClientState, ClientEvent, ClientOutMsg,
]

// BoardingClientTransitions defines all valid state transitions for the
// client round interaction FSM. This serves as both documentation and a
// validation target for tests.
//
//nolint:ll
var BoardingClientTransitions = ClientTransitionTable{
	MachineName: "BoardingClient",
	States: []ClientStateTransitions{
		// Idle: Initial state, waiting for intent packages.
		{
			FromState: &Idle{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &IntentPackage{},
					ToState:     &PendingRoundAssembly{},
					Description: "Intent package received, start assembly",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Boarding failed before any progress",
					IsTerminal:  true,
				},
			},
		},

		// PendingRoundAssembly: Collecting intents for the round.
		{
			FromState: &PendingRoundAssembly{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &IntentPackage{},
					ToState:     &PendingRoundAssembly{},
					Description: "Additional intents received",
				},
				{
					Event:       &RegistrationRequested{},
					ToState:     &AwaitingRegistrationBuildState{},
					Description: "Intents confirmed, building registration",
					EmitsOutbox: []ClientOutMsg{&BuildRegistrationReq{}},
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Boarding failed during assembly",
					IsTerminal:  true,
				},
			},
		},

		// AwaitingRegistrationBuildState: Waiting for registration
		// build to complete.
		{
			FromState: &AwaitingRegistrationBuildState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &BuildRegistrationSucceeded{},
					ToState:     &RegistrationSentState{},
					Description: "Registration built, sending to server",
					EmitsOutbox: []ClientOutMsg{&JoinRoundRequest{}},
				},
				{
					Event:       &BuildRegistrationFailed{},
					ToState:     &ClientFailedState{},
					Description: "Registration build failed",
					IsTerminal:  true,
				},
			},
		},

		// RegistrationSentState: Waiting for server acceptance.
		{
			FromState: &RegistrationSentState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &RoundJoined{},
					ToState:     &RoundJoinedState{},
					Description: "Server accepted registration",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Server rejected registration",
					IsTerminal:  true,
				},
			},
		},

		// RoundJoinedState: Waiting for commitment tx from server.
		{
			FromState: &RoundJoinedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &CommitmentTxBuilt{},
					ToState:     &CommitmentTxReceivedState{},
					Description: "Received commitment tx and VTXT",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Round failed waiting for commitment",
					IsTerminal:  true,
				},
			},
		},

		// CommitmentTxReceivedState: Validating commitment tx and VTXT.
		{
			FromState: &CommitmentTxReceivedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &CommitmentTxValidated{},
					ToState:     &CommitmentTxValidatedState{},
					Description: "Commitment tx and VTXT validated",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Validation failed",
					IsTerminal:  true,
				},
			},
		},

		// CommitmentTxValidatedState: Generate and send nonces.
		{
			FromState: &CommitmentTxValidatedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &GenerateNonces{},
					ToState:     &NoncesSentState{},
					Description: "Generated nonces, sending to server",
					EmitsOutbox: []ClientOutMsg{
						&SubmitNoncesRequest{},
					},
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Nonce generation failed",
					IsTerminal:  true,
				},
			},
		},

		// NoncesSentState: Waiting for aggregated nonces from server.
		{
			FromState: &NoncesSentState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &NoncesAggregated{},
					ToState:     &NoncesAggregatedState{},
					Description: "Received aggregated nonces",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Nonce aggregation failed",
					IsTerminal:  true,
				},
			},
		},

		// NoncesAggregatedState: Generate and send partial signatures.
		{
			FromState: &NoncesAggregatedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &GeneratePartialSigs{},
					ToState:     &PartialSigsSentState{},
					Description: "Generated partial sigs, sending",
					EmitsOutbox: []ClientOutMsg{
						&SubmitPartialSigRequest{},
					},
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Partial signature generation failed",
					IsTerminal:  true,
				},
			},
		},

		// PartialSigsSentState: Waiting for operator to sign.
		{
			FromState: &PartialSigsSentState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:   &OperatorSigned{},
					ToState: &AwaitingBoardingSignaturesState{},
					Description: "Received VTXT sigs, " +
						"signing boarding inputs",
					EmitsOutbox: []ClientOutMsg{
						&SignBoardingInputsReq{},
					},
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Operator signing failed",
					IsTerminal:  true,
				},
			},
		},

		// AwaitingBoardingSignaturesState: Waiting for boarding
		// input signing.
		{
			FromState: &AwaitingBoardingSignaturesState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:   &SignBoardingInputsSucceeded{},
					ToState: &AwaitingRoundCheckpointState{},
					Description: "Boarding inputs signed," +
						" checkpointing",
					EmitsOutbox: []ClientOutMsg{
						&CommitRoundStateReq{},
					},
				},
				{
					Event:       &SignBoardingInputsFailed{},
					ToState:     nil,
					Description: "Boarding signing failed (fatal)",
				},
			},
		},

		// AwaitingRoundCheckpointState: Waiting for round persistence.
		{
			FromState: &AwaitingRoundCheckpointState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &CommitRoundStateSucceeded{},
					ToState:     &InputSigSentState{},
					Description: "Checkpoint saved, sending sigs",
					EmitsOutbox: []ClientOutMsg{
						&SubmitForfeitSigRequest{},
						&RegisterConfirmationRequest{},
						&RoundCheckpointedNotification{},
					},
				},
				{
					Event:       &CommitRoundStateFailed{},
					ToState:     nil,
					Description: "Checkpoint failed (fatal)",
				},
			},
		},

		// InputSigSentState: Waiting for commitment tx confirmation.
		{
			FromState: &InputSigSentState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &BoardingConfirmed{},
					ToState:     &AwaitingSaveVTXOsState{},
					Description: "Commitment tx confirmed, saving VTXOs",
					EmitsOutbox: []ClientOutMsg{
						&SaveVTXOsReq{},
					},
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Commitment tx failed to confirm",
					IsTerminal:  true,
				},
				{
					Event:       &RecoveryInitiated{},
					ToState:     &RecoveryInitiatedState{},
					Description: "CSV timeout, recovering funds",
					IsTerminal:  true,
				},
			},
		},

		// AwaitingSaveVTXOsState: Waiting for VTXO persistence.
		{
			FromState: &AwaitingSaveVTXOsState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &SaveVTXOsSucceeded{},
					ToState:     &ConfirmedState{},
					Description: "VTXOs saved, round complete",
					EmitsOutbox: []ClientOutMsg{
						&VTXOCreatedNotification{},
						&RoundCompletedNotification{},
					},
					IsTerminal: true,
				},
				{
					Event:       &SaveVTXOsFailed{},
					ToState:     &ClientFailedState{},
					Description: "VTXO persistence failed",
					IsTerminal:  true,
				},
			},
		},

		// ConfirmedState: Terminal success state. On RoundComplete,
		// transition back to Idle to allow processing new boarding
		// addresses and intents.
		{
			FromState: &ConfirmedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &RoundComplete{},
					ToState:     &Idle{},
					Description: "Round complete, return to idle",
				},
			},
		},

		// ClientFailedState: Recoverable failure state. Recovery
		// transitions to Idle and re-dispatches the IntentPackage,
		// so the net destination is PendingRoundAssembly.
		{
			FromState: &ClientFailedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &IntentPackage{},
					ToState:     &Idle{},
					Description: "Recover from failure with new intents",
				},
				{
					Event:       &BoardingFailed{},
					ToState:     &ClientFailedState{},
					Description: "Self-loop, already failed",
					IsTerminal:  true,
				},
			},
		},

		// RecoveryInitiatedState: Terminal recovery state.
		{
			FromState: &RecoveryInitiatedState{},
			Transitions: []ClientTransitionEntry{
				{
					Event:       &RecoveryInitiated{},
					ToState:     &RecoveryInitiatedState{},
					Description: "Self-loop, recovery in progress",
					IsTerminal:  true,
				},
			},
		},
	},
}
