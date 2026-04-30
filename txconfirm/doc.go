// Package txconfirm provides a generic shared actor for ensuring that
// transactions are confirmed on-chain.
//
// # Overview
//
// Any subsystem that needs "get this transaction confirmed and tell me when
// it happens" can use this package. txconfirm is intentionally
// subsystem-neutral — no unroll/, vtxo/, payment/, or round/ semantics leak
// in. Callers submit a signed transaction via EnsureConfirmedReq together
// with a subscriber that receives a terminal TxConfirmed or TxFailed
// notification, and cancel their interest with CancelInterestReq when they
// no longer care.
//
// The actor deduplicates by txid: two callers asking to confirm the same
// transaction share a single confirmation watch, a single broadcast
// attempt, and (when applicable) a single anchor-paying CPFP child. Each
// caller still gets its own terminal notification.
//
// # Architecture
//
// The package is split into two layers:
//
//   - TxBroadcasterActor (actor.go) is the message-driven orchestrator.
//     It holds a tracked-tx map keyed by txid, runs a protofsm lifecycle
//     per txid, and handles the fan-out of chainsource callbacks
//     (confirmation events, block epochs) back into per-txid state
//     transitions.
//
//   - CPFPBroadcaster (broadcaster.go) is an actor-free helper that
//     handles the actual broadcast mechanics: direct submission for
//     transactions without anchors, CPFP child construction and package
//     submission for anchor parents, fee estimation, fee-input
//     selection, and fee-bump replacement-floor enforcement. Callers can
//     use CPFPBroadcaster standalone from outside the actor if they
//     need the broadcast primitives without the tracking harness.
//
// # Lifecycle
//
// Each tracked txid transitions through a protofsm state machine:
//
//	New → Broadcasting → AwaitingConfirmation → FeeBumping → … → Confirmed
//	                                                           \→ Failed
//
// New is the initial state. Broadcasting and FeeBumping are transient
// states the FSM spends time in while submitting to the network.
// AwaitingConfirmation is the steady state between broadcast attempts
// waiting for the chain to confirm or for a fee-bump interval to
// elapse. Confirmed and Failed are terminal; upon entering either the
// actor notifies every subscriber in fan-out order, retaining the
// tracked entry only while a terminal notification still needs retry
// delivery.
//
// # CPFP correctness
//
// For transactions containing an ephemeral anchor output (BIP 431), the
// CPFPBroadcaster attaches a fee-paying child that spends both the
// anchor and a confirmed wallet UTXO, then submits the parent + child
// as a TRUC-compliant package (BIP 331). Correctness of this flow rests
// on five invariants, each guarded by a dedicated code path:
//
//  1. Version gate. Submit rejects non-v3 parents so pattern-based
//     anchor detection never misattaches a CPFP child to a coincidental
//     anyone-can-spend output on a legacy parent.
//
//  2. Replacement floor. Every fee bump runs through
//     applyReplacementFloor before selecting a fee input, which
//     enforces BIP-125 Rule 4 (strictly higher feerate) and Rule 3
//     (strictly higher absolute fee, by at least
//     IncrementalRelayFeeSatPerVByte * packageVSize) against the last
//     successful submission for the same parent txid. Without this, a
//     flat or dipping fee estimator would regenerate byte-identical or
//     lower-fee packages that the mempool rejects.
//
//  3. Fee-input reservation. Each parent txid reserves the wallet
//     UTXO(s) it has committed to across its submission history.
//     Reservations survive block boundaries and are released only when
//     the parent is evicted, preventing two concurrent parents from
//     racing for the same UTXO. A parent IS allowed to re-pick UTXOs
//     from its own reserved set, because TRUC package RBF relies on
//     the new child double-spending the previous child's fee input.
//
//  4. RBF-signaling fee input. The CPFP child's fee input carries
//     sequence MaxTxInSequenceNum - 2 (= 0xfffffffd) as a
//     belt-and-suspenders so even a non-TRUC parent (if one ever
//     slipped past the version gate) would produce a BIP-125-signaling
//     child.
//
//  5. Optional preflight. When the caller enables
//     PreSubmitTestMempoolAccept, every broadcast attempt is first
//     validated against the backend's testmempoolaccept RPC. Rejections
//     abort the submission with the backend's reject reason; backends
//     that do not implement the RPC are downgraded to a soft-miss.
//
// # PSBT finalization
//
// signCPFPChild matches PSBT inputs by PreviousOutPoint rather than by
// positional index. Wallets that reorder finalized inputs (BIP 69) or
// add/remove inputs relative to the supplied PSBT return a clean error
// rather than silently mis-wiring witnesses or panicking on an
// out-of-bounds index.
//
// # Service-key round trip
//
// RegisterConfRequest and UnregisterConfRequest both carry PkScript so
// that chainsource's txid+script keyed service-actor lookup resolves
// symmetrically in both directions. Dropping PkScript on one side would
// leak one conf sub-actor per tracked tx.
//
// # Eviction
//
// Once a tracked txid reaches Confirmed or Failed and every subscriber
// has been notified or cancelled, the actor's evictTerminal helper
// unregisters any remaining chainsource subscriptions, stops the
// per-txid FSM goroutine, releases the parent's fee-input reservations
// in the broadcaster, and drops the entry from the tracking map. Without
// this step a long-lived daemon would accumulate one FSM goroutine and
// one cached *wire.MsgTx per transaction it ever confirmed. A late
// caller that arrives after eviction re-registers with chainsource and,
// if the tx is already confirmed on-chain, receives an immediate
// TxConfirmed notification through the normal path.
package txconfirm
