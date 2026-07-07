//go:build walletdkrpc && swapruntime

// Package swapwallet implements the daemon-side walletdkrpc subserver that
// exposes a simplified, swap-vocabulary-free wallet API on top of the
// running darepod daemon.
//
// The package composes the existing swap subsystem (swapclientserver, gated
// by the swapruntime build tag), the in-process Ark SDK facade, the
// daemon-managed signer (RPCServer.SignReceiveAuthMessage and friends),
// the cooperative-leave RPC (RPCServer.LeaveVTXOs), the wallet actor
// (CreateBoardingAddressRequest), and the unified ledger history surface
// (RPCServer.ListTransactions) into one small RPC service:
//
//   - Send dispatches outbound payments: BOLT-11 invoices are routed through
//     the swap subsystem (which may settle as a same-Ark p2p vHTLC or a real
//     Lightning hop, transparently); onchain destinations are routed through
//     LeaveVTXOs cooperative exits. The caller never sees swap vocabulary.
//   - Recv asks the daemon to open an out-swap and returns a BOLT-11 invoice
//     signed with a daemon-managed payment-scoped auth key.
//   - List merges swap, OOR, boarding, and exit history into the flat
//     WalletEntry shape sorted by updated-at descending.
//   - Deposit returns a fresh boarding onchain address.
//   - Balance and Status surface a unified, summary-level view.
//   - SubscribeWallet streams normalized WalletEntry updates.
//
// The package composes the full swap lifecycle in-process. Its runtime drives
// a synchronous resume-on-startup sweep before the gRPC server accepts calls,
// keeps wallet-local pending entries from hanging forever, and runs a monitor
// loop that fans normalized updates to SubscribeWallet subscribers. Background
// goroutines are anchored to the daemon root context, never to RPC-call
// contexts, so a CLI disconnect can never cancel in-flight work.
//
// The walletdkrpc build tag depends on swapruntime: building walletdkrpc
// without swapruntime is a deliberate compile error because the subserver
// composes the daemon-owned swap subsystem.
//
// # V1 LIMITATIONS
//
// Canonical-id stability: only SEND-invoice and RECV operations carry a
// stable WalletEntry.id across the pending → terminal lifecycle. The
// swap subserver's row keys by Lightning payment_hash, which the wallet
// layer also uses as the canonical id, so the projection is a no-op.
//
// EXIT and DEPOSIT operations DO surface a row at submit time and a
// separate confirmed ledger row later, but the two rows do NOT share an
// id in v1 because there is no daemon-side notification hook that links
// (a) an exit's queued outpoints to its eventual sweep txid, or (b) a
// deposit's boarding address to its eventual boarding txid. Cooperative
// leave rows can still complete while the original runtime-local row is
// alive: once the source VTXO is terminally forfeited, the pending EXIT
// row is shown as COMPLETE. After restart, however, the daemon cannot
// recreate the original counterparty/note from durable state alone. A v2
// canonical-id projection lands when the daemon exposes the missing
// hooks; the right home for that link is the daemon-side persistence
// (leave job, deposit address record), not a process-local map. Callers
// that need to correlate EXIT/DEPOSIT pending → confirmed across restart
// in v1 should track the WalletEntry.Counterparty (truncated bech32
// address or txid) and the persisted ledger txid via separate queries.
//
// Onchain SEND sweep semantics: a bounded onchain send (amt_sat > 0)
// lands exactly amt_sat at the destination and returns the remainder as
// a change VTXO under the seal-time fee handshake, so it does not
// overpay. Only a sweep_all send drains WHOLE selected VTXOs, so only
// then does SendResponse.actual_amount_sat exceed any single requested
// figure; it carries the real outflow and SHOULD be echoed to the user
// before treating the send as confirmed. The "drain the wallet" intent
// is structurally distinct in v1: SendRequest.sweep_all must be set
// explicitly and amt_sat must be zero; a typo'd amt_sat=0 without
// sweep_all is rejected up front at the wallet layer and again at the
// CLI.
//
// # CANONICAL ACTIVITY LOG
//
// The runtime dual-writes every emitted WalletEntry into a persisted
// canonical activity log (the daemon-side db.ActivityPersistenceStore,
// injected via Deps.ActivityStore): activity_entries is the current-state
// projection and activity_events is the append-only transition log. Writes
// happen project-then-emit at the swap monitor loop, the cooperative-leave
// submit, the credit poll, the forced unilateral exit, and the deadline
// overlay, plus a one-time startup backfill from the collectors below.
//
// The RPC read path now reads the store: List(ACTIVITY) pages activity_entries
// by the immutable (created_at_unix, canonical_id) keyset cursor, and
// SubscribeWallet's include_existing snapshot goes through the same
// store-backed List. deriveActivity (the live merge) is retained only for the
// store-less/test build and to seed the startup backfill. Because the store is
// ordered by the immutable created_at keyset, the feed is newest-by-creation,
// not newest-by-update.
//
// Consequences of the store-backed read that are tracked, not yet closed:
//   - Producers without an ongoing projector — confirmed boarding DEPOSIT and
//     daemon-side sweep/EXIT rows derived from ListTransactions — reach the
//     store only via the startup backfill, so a newly-confirmed one appears in
//     List after the next restart rather than immediately.
//   - The synthetic boarding-unconfirmed DEPOSIT row is derive-path-only: it
//     is ephemeral live state (recomputed from GetBalance, no durable id) and
//     is deliberately NOT projected, so on a store build an unconfirmed
//     boarding deposit surfaces via Balance rather than as an activity row
//     until it confirms.
//
// An on-chain-send / cooperative-leave EXIT row now carries a stable canonical
// id: the daemon returns its leave-job id (SendOnChainResponse.send_job_id, a
// deterministic hash of the consumed outpoints) and the wallet uses it as the
// row id, so a single handle represents a multi-input sweep and stays the same
// across the round seal. The id is deterministic — reproducible from the same
// inputs — but the row itself is wallet-local (in-memory) and its terminal
// status is NOT reconciled across a restart yet: completion currently fires
// only when the derive/backfill pass matches the retained consumed outpoint
// (kept in vtxo_outpoint) against a forfeited VTXO. A DEPOSIT is still keyed by
// txid:vout pending its own stable-id work. That DEPOSIT id, projecting the
// backfill-only producers on an ongoing basis, and live cross-restart terminal
// reconciliation under the id (projecting forfeit/settlement into the store
// rather than relying on the startup backfill) are tracked separately (C2).
package swapwallet
