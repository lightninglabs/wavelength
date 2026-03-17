package oor

// Package oor implements client-side coordination for out-of-round (OOR) Ark
// transfers.
//
// The package is built around deterministic state machines and explicit
// side-effect boundaries so mobile clients can safely survive restarts while
// still driving OOR transfers to completion.
//
// Core goals:
//   - deterministic transfer package construction (stable Ark txid/session id)
//   - crash-safe resume based on persisted state + replayed outbox work
//   - strict separation between pure FSM transitions and effect execution
//
// The FSM emits OutboxEvent values for all I/O (signing, transport, local
// persistence, retries). The durable actor executes outbox work and feeds the
// resulting Event back into the FSM.
//
// # Outgoing Transfer FSM
//
// State flow (happy path):
//
//	Idle
//	  -- StartTransferEvent -->
//	AwaitingArkSignatures
//	  -- ArkSignedEvent -->
//	AwaitingSubmitAccepted
//	  -- SubmitAcceptedEvent -->
//	AwaitingCheckpointSignatures
//	  -- CheckpointsSignedEvent -->
//	AwaitingFinalizeAccepted
//	  -- FinalizeAcceptedEvent -->
//	AwaitingLocalVTXOUpdate
//	  -- InputsMarkedSpentEvent -->
//	Completed
//
// Outbox work emitted on this path:
//   - RequestArkSignatures
//   - SendSubmitPackageRequest
//   - RequestCheckpointSignatures
//   - SendFinalizePackageRequest
//   - MarkInputsSpentRequest
//
// Point-of-no-return:
//   - SubmitAcceptedEvent means the operator has co-signed checkpoint PSBTs.
//     After this point, resume logic must preserve byte-level consistency for
//     checkpoint artifacts and session identity.
//
// Retry model:
//   - Retryable OutboxErrorEvent keeps the current state and emits
//     ScheduleRetryRequest.
//   - Non-retryable OutboxErrorEvent transitions to Failed.
//
// # Incoming Transfer FSM
//
// The receive FSM is separate from the sender FSM so incoming notifications
// can be validated, materialized, and acked independently.
//
// State flow (happy path):
//
//	ReceiveResolving or ReceiveIdle
//	  -- IncomingTransferEvent -->
//	ReceiveNotified
//	  -- IncomingMetadataResolvedEvent -->
//	ReceiveNotified (materialization request emitted)
//	  -- IncomingHandledEvent -->
//	ReceiveAwaitingAck
//	  -- IncomingAckSentEvent -->
//	ReceiveCompleted
//
// Outbox work emitted on this path:
//   - IncomingTransferNotification
//   - QueryIncomingMetadataRequest
//   - MaterializeIncomingVTXOsRequest
//   - SendIncomingAckRequest
//
// # Durability And Idempotency Notes
//
// Session identity:
//   - SessionID is derived from Ark txid and validated throughout transitions.
//
// Replay safety:
//   - FSM states intentionally tolerate duplicate and out-of-order
//     deliveries by ignoring events not handled by the current state.
//   - Outbox requests should be implemented idempotently because durable replay
//     can re-emit them after process restart.
//
// Local persistence:
//   - Local spent-marking and incoming materialization are modeled as outbox
//     work so failures can be retried with the same deterministic state.
//
// These properties let callers choose transport/adaptor implementations without
// weakening crash recovery guarantees.
