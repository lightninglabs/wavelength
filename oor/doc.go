package oor

// Package oor implements the server-side out-of-round (OOR) transfer
// coordinator.
//
// The coordinator is structured as a protofsm-driven state machine per
// transfer session, wrapped by an actor that:
// - routes Submit/Finalize requests to the correct session FSM; and
// - executes side effects via an explicit outbox interface.
//
// State flow (human-readable):
//
//   SubmitOORRequest
//       |
//       v
//   IdleState
//       |
//       | emit LockInputsReq
//       v
//   RequestedState
//       |
//       | InputsLockSucceededEvent + emit CoSignReq
//       v
//   ValidatedState
//       |
//       | OperatorSignedEvent
//       v
//   CoSignedState  (point-of-no-return: no unlock)
//       |
//       | FinalizeRequestedEvent + emit ValidateFinalizeReq
//       v
//   AwaitingFinalCheckpointsState
//       | \
//       |  \ FinalizeFailedEvent
//       |   \
//       |    v
//       |   FailedState
//       |
//       | FinalizeValidatedEvent + emit FinalizeReq
//       v
//   AwaitingFinalCheckpointsState
//       |
//       | FinalizeSucceededEvent
//       v
//   FinalizedState
//
// Failure paths:
// - SignFailedEvent in ValidatedState -> FailedState + UnlockInputsReq.
// - SignFailedEvent in CoSignedState -> FailedState (no unlock).
//
// Package vocabulary is documented on:
// - SubmitOORRequest / FinalizeOORRequest
// - ValidateFinalizeReq
//
// The OOR transfer flow has a strict point-of-no-return: once the operator has
// co-signed the checkpoint transaction(s), the server must not release the
// input VTXO locks. Instead, the session must be restart-safe and support
// idempotent submit retries that return the same co-signed PSBT bytes.
