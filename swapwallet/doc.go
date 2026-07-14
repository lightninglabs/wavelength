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
// An on-chain-send / cooperative-leave EXIT row keys by the daemon's
// leave-job id (SendOnChainResponse.send_job_id); see the CANONICAL
// ACTIVITY LOG note. A DEPOSIT keys by an address-scoped id
// (deposit-<address>) once the daemon records it on-chain: the confirmed
// boarding-deposit ledger row carries the allocated boarding address
// (TransactionHistoryEntry.boarding_address), and every UTXO paid to that
// address is SUMMED into one deposit-<address> row (sumDepositsByAddress),
// so a reused boarding address shows its total received rather than
// hiding funds behind one UTXO. Generating an address does NOT create a
// row — allocating an address is not a pending deposit — so the row
// appears only from the point the daemon records an incoming UTXO. The
// Deposit RPC still returns that same deposit-<address> id so a caller can
// correlate. An older daemon that does not populate boarding_address falls
// back to per-UTXO txid:vout deposit rows (no summing, still correct). The
// confirmed deposit is applied by the derive-and-project pass; the
// reconciler (see CANONICAL ACTIVITY LOG) re-runs that pass on a periodic
// tick, so it lands in the store within a tick rather than only at the
// next startup backfill.
//
// Before confirmation, the in-process daemon address registry is correlated
// with zero-conf wallet UTXOs. When one boarding address accounts for the
// aggregate boarding_unconfirmed_sat, activity can therefore use that same
// deposit-<address> id immediately. Older embeddings and ambiguous balances
// spanning multiple addresses keep the synthetic boarding-unconfirmed
// aggregate fallback. A unilateral EXIT row likewise still keys by the
// consumed VTXO outpoint with no durable link to its eventual sweep txid.
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
// overlay, plus a one-time startup backfill and a periodic reconciler pass
// (reprojectActivity), both from the collectors below.
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
//   - Producers without a per-event projector — confirmed boarding DEPOSIT and
//     daemon-side sweep/EXIT rows derived from ListTransactions — are landed by
//     the periodic reconciler (reconcileInterval) re-deriving and re-projecting
//     them, so a newly-confirmed one appears in List within a reconcile tick
//     rather than only after a restart. The tick is coarse, so there is a
//     bounded delay; a per-block/confirmation hook (cheaper, lower latency) is
//     a deferred optimization.
//   - The zero-conf DEPOSIT row is ephemeral live state and is deliberately
//     NOT projected. Its normal id is deposit-<address>; the synthetic
//     boarding-unconfirmed id remains only as an ambiguous aggregate fallback.
//     The store-backed List path overlays the row onto the first page so
//     Balance and activity both expose the pending deposit without stranding a
//     durable PENDING row after confirmation. Because it has no event_seq, it
//     is not part of resumable SubscribeWallet replay.
//
// An on-chain-send / cooperative-leave EXIT row now carries a stable canonical
// id: the daemon returns its leave-job id (SendOnChainResponse.send_job_id, a
// deterministic hash of the consumed outpoints) and the wallet uses it as the
// row id, so a single handle represents a multi-input sweep and stays the same
// across the round seal. The id is deterministic — reproducible from the same
// inputs — and the periodic reconciler lands its terminal transition into the
// store live: each pass matches the retained consumed outpoint (kept in
// vtxo_outpoint) against a forfeited VTXO and upserts the row to COMPLETE.
// Its COMPLETE transition is restart-survivable: the PENDING row is persisted
// at submit (canonical_id = send_job_id, the retained consumed outpoint in
// vtxo_outpoint, plus counterparty/note/amount), and on startup the runtime
// re-tracks the store's PENDING EXIT rows (rehydrateWalletLocalPending) so the
// same forfeit correlation lands COMPLETE under the stable id after a restart.
// Deferred: a per-block/confirmation reconcile trigger would lower latency, but
// a block epoch fires before the wallet processes that block into the derive
// sources, so the coarse, ordering-independent periodic reconciler is kept.
package swapwallet
