// Package recovery models the data plane of unilateral-exit recovery for one
// VTXO target outpoint.
//
// # Mental model
//
// A "recovery proof" is the set of transactions a user must broadcast, in
// dependency order, to unilaterally materialize an on-chain output they own
// inside an Ark tree (or OOR lineage) — and, after a CSV timeout, spend it.
// Conceptually the proof is a DAG:
//
//	roots (self-funded by the user)
//	  │
//	  ▼
//	tree / checkpoint intermediates
//	  │
//	  ▼
//	target node (creates the spendable outpoint)
//	  │
//	  ▼
//	(CSV delay elapses)
//	  │
//	  ▼
//	sweep (spends target outpoint to a destination)
//
// This package is deliberately narrow: it owns the graph (`Proof`), the
// per-session state machine (`Session`), and the durable projection of that
// state (`SessionState`) plus a TLV codec in state_codec.go / proof_codec.go.
// It does NOT:
//
//   - broadcast transactions
//   - talk to a chain backend
//   - schedule retries
//   - spawn goroutines
//
// All of those concerns live in downstream consumers (the planner in
// `unrollplan`, and the actor wiring that follows in later PRs in the stack).
// Keeping recovery I/O-free and synchronous makes the data model amenable to
// property-based testing and lets consumers pick their own reliability
// mechanics.
//
// # Layering
//
// Every node's position is precomputed by a Kahn-style topological layering
// so consumers never have to recurse over the DAG themselves; they iterate
// layer-by-layer from roots to the target and ask the session which nodes at
// each layer are ready, in flight, awaiting confirmation, or blocked.
//
// # Invariants worth knowing
//
//   - CSV delay is a raw block count bounded by MaxCSVDelay (BIP-68 height-
//     mode limit, 65535 blocks). Any caller who sources the delay from a
//     BIP-68-encoded sequence must decode the block count first.
//   - NewProof rejects: nil nodes, duplicate txids, a target not in the
//     graph, an out-of-bounds target output index, unreachable nodes
//     (nodes that cannot be connected to the target via the parents map),
//     and cycles.
//   - Session is goroutine-safe under an RWMutex. MarkConfirmed enforces the
//     full topological invariant (parents confirmed before children) so that
//     a reorg-aware caller cannot accidentally corrupt the session.
//   - SessionState validation is symmetric with the Session state machine —
//     a persisted state that would have been rejected by MarkConfirmed is
//     also rejected by NewSessionFromState.
package recovery
