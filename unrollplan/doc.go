// Package unrollplan provides pure planning for unilateral-exit execution.
//
// # Mental model
//
// Given an immutable `recovery.Proof` (the DAG of transactions the user must
// broadcast to reach a target outpoint plus a final sweep), the planner
// answers three questions at each block height:
//
//  1. Which proof transactions are ready to broadcast right now?
//     (all in-proof parents are confirmed)
//  2. Which transactions are blocked, and on what?
//     (the set of unconfirmed parent txids)
//  3. Has the target's CSV delay matured; do we need to broadcast the sweep;
//     is the sweep already broadcast or confirmed; is the session "done"?
//
// The planner owns no state. Callers hand in a durable `State` struct on
// every `Plan` call; the planner re-derives the frontier from first
// principles against the immutable proof. This makes crash recovery trivial
// (restore State from disk, call Plan, continue) and makes the planner
// amenable to property-based testing.
//
// # No I/O
//
// Like `lib/recovery`, this package is deliberately synchronous and
// I/O-free. It does not broadcast, does not watch the chain, does not
// schedule retries. The actor that wires this planner into a daemon (later
// PRs in the stack) is responsible for those concerns.
//
// # Validation symmetry
//
// `State.Validate` is the single source of truth for "is this state
// self-consistent with the proof graph?". Every call to `Plan` runs it.
// The same invariants are mirrored in `lib/recovery.validateSessionState` so
// a state that passes one layer is guaranteed to pass the other — callers
// can rely on this when choosing where to deserialize state.
//
// # On-disk form
//
// `State` is TLV-encoded via state_codec.go. Optional fields use `fn.Option`
// instead of nilable pointers so a caller cannot accidentally confuse
// "absent" with "zero value". The codec is canonical (sorted txid lists,
// single-value optionals) and carries a version byte for forward-migration.
package unrollplan
