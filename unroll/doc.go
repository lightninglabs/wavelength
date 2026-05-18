// Package unroll drives the unilateral-exit lifecycle for one VTXO: it
// broadcasts and confirms every ancestor transaction required to put the
// target output on chain, waits out its CSV timeout, then builds and
// broadcasts the final timeout-path sweep that hands the funds back to the
// local wallet.
//
// # Architecture
//
// Unilateral exit is not a single transaction, it is a graph of
// transactions — a VTXO may sit several "hops" deep inside a tree created
// by a round plus optional out-of-round (OOR) chains stacked on top. To
// reclaim funds locally the client has to:
//
//  1. Gather every ancestor transaction needed to materialize the target
//     output on chain (the "recovery proof").
//  2. Broadcast each ancestor that is ready, in dependency order,
//     waiting for each to confirm before its children become ready.
//  3. Once the target itself confirms, wait for its relative timelock
//     (CSV) to mature.
//  4. Build, sign (with the client's timeout-path witness), and broadcast
//     a sweep that spends the target to a wallet-owned output.
//  5. Wait for the sweep to confirm.
//
// Any step can fail — a mempool rejection, a reorg, a restart, an operator
// racing us with a cooperative forfeit, etc. The package is built so that
// every piece of ongoing work survives a crash, and so that retries never
// double-spend, burn addresses, or silently lose a job.
//
// # Component Split
//
// The package separates four concerns:
//
//   - [unrollplan.Planner] (external): a pure function that, given a proof
//     graph plus current durable state (confirmed txids, in-flight txids,
//     target confirm height, sweep status), decides what to do next:
//     which transactions are ready to broadcast, which are still blocked
//     by their parents, whether the CSV has matured, whether the sweep
//     should be built. The planner has no IO — it only computes.
//
//   - [VTXOUnrollActor]: one local actor per target outpoint. It owns
//     the FSM session, the recovery proof, the planner, the cached sweep
//     transaction, and the snapshot. All IO — [txconfirm] Asks, chain
//     subscriptions, persistence, registry notifications — runs here.
//
//   - [UnrollRegistryActor]: a thin coordinator on top of the set of
//     per-target actors. It owns spawn, dedup, terminal bookkeeping, and
//     writes a coarse control-plane record per target to the store so the
//     daemon can restore in-flight jobs after restart.
//
//   - Support code: [LocalProofAssembler] + [DescriptorLineageResolver]
//     walk the local VTXO + OOR artifact state into an immutable
//     recovery.Proof; [buildSweepTx] builds and signs the final sweep;
//     [snapshot.go] encodes/decodes the per-actor TLV snapshot.
//
// The FSM itself (see [fsm_types.go], [fsm_logic.go]) models lifecycle
// phases only: Idle → AwaitingMaterialization → AwaitingCSV →
// AwaitingSweepBroadcast → AwaitingSweepConfirmation → Completed (or
// Failed from any non-terminal state). Every transition that needs IO
// emits one or more [OutboxEvent]s; the behavior's routeOutbox then
// translates those into real txconfirm calls. This keeps the FSM testable
// without mocks and keeps IO concerns out of state math.
//
// # Durability
//
// Two rules are load-bearing:
//
//  1. "Persist before broadcast." startSweep calls persistJob
//     before it asks txconfirm to broadcast the sweep. On any retry (same
//     actor lifetime or after restart) the same sweepTx is restored
//     instead of re-derived, so txconfirm's txid-keyed dedup turns the
//     retry into a benign no-op. Without this, a crash between signing
//     and broadcasting would leave the BIP32 key burned on the first try
//     and send a fresh sweep with a different pkScript on the second,
//     racing the original on chain.
//
//  2. "Fail-closed admission." UnrollRegistryActor.handleEnsure calls
//     Store.UpsertRecord synchronously before it returns Created=true. A
//     crash in that window would otherwise orphan the spawned child:
//     RestoreNonTerminal reads only the durable store, so an unpersisted
//     child would be invisible on restart.
//
// # Reorg and External Spend Handling
//
// The actor registers a spend watch on the target outpoint via
// chainsource. If something other than our sweep or a known proof node
// spends the target — an operator cooperative path, a double-spend, a
// reorg-replaced parent — the FSM is driven to Failed with a reason
// string identifying the external spender. Spends by our own sweep or by
// known proof nodes are benign and just advance height.
//
// # Restart Flow
//
// On daemon start, the registry calls RestoreNonTerminal, which lists
// every non-terminal record, spawns a VTXOUnrollActor per target, and
// sends ResumeUnrollRequest. The actor loads its snapshot (proof graph,
// planner state, sweep tx, last height), reconstructs the FSM, and emits
// ReissueInFlightTransactions / ReissueSweepConfirmation outbox events so
// the behavior re-subscribes txconfirm for every tx that was in flight.
// txconfirm's dedup makes every re-submit idempotent, so no duplicate
// broadcasts escape the client.
//
// # Documentation
//
// Per-package docs:
//   - [../unroll/CLAUDE.md] — stable summary of types, relationships,
//     invariants.
//   - [../docs/mailbox_architecture.md] — SQL-backed mailbox transport model.
package unroll
