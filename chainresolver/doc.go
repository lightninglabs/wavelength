// Package chainresolver implements the on-chain VTXO unrolling pipeline.
//
// When a VTXO cannot be refreshed cooperatively (due to expiry, user request,
// or fraud detection), it must be "unrolled" onto the Bitcoin blockchain by
// broadcasting the virtual transaction tree (VTXT) that was pre-signed during
// the round.
//
// Three triggers exist for on-chain resolution:
//
//  1. Expiry-driven: The VTXO FSM emits an ExpiringNotification when a VTXO is
//     critically close to batch expiry and collaborative refresh has failed.
//
//  2. User-initiated: The user explicitly requests to put a VTXO on-chain via
//     the wallet RPC.
//
//  3. Fraud-reactive: A prior OOR sender publishes their VTXO on-chain; the
//     current owner must publish checkpoint transactions to claim their VTXO.
//
// The package is organized around two components:
//
//   - Coordinator: A single long-lived actor that receives inbound messages,
//     manages per-VTXO resolver FSMs, translates outbox messages into
//     chainsource calls, and handles crash recovery.
//
//   - Resolver FSM: A per-VTXO finite state machine that tracks the progress
//     of broadcasting tree levels and checkpoint transactions, handling
//     confirmations and CSV delays.
package chainresolver
