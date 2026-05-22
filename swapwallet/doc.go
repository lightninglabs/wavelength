//go:build walletrpc && swapruntime

// Package swapwallet implements the daemon-side walletrpc subserver that
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
//   - Recv asks the daemon to open a swap-in and returns a BOLT-11 invoice
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
// The walletrpc build tag depends on swapruntime: building walletrpc without
// swapruntime is a deliberate compile error because the subserver composes
// the daemon-owned swap subsystem.
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
// deposit's boarding address to its eventual boarding txid. The earlier
// in-process intent index was removed once it became clear it was an
// identity-mapping ceremony for the SEND-invoice and RECV paths (the
// swap subsystem already keys those by payment_hash). A v2 canonical-id
// projection lands when the daemon exposes the missing hooks; the
// right home for that link is the daemon-side persistence (leave job,
// deposit address record), not a process-local map. Callers that need
// to correlate EXIT/DEPOSIT pending → confirmed in v1 should track the
// WalletEntry.Counterparty (truncated bech32 address or txid) and the
// persisted ledger txid via separate queries.
//
// Onchain SEND sweep semantics: the cooperative-leave path sweeps WHOLE
// selected VTXOs to the destination, so the recipient may receive more
// than the caller's amt_sat. SendResponse.actual_amount_sat carries the
// real outflow and SHOULD be echoed to the user before treating the
// send as confirmed. The "drain the wallet" intent is structurally
// distinct in v1: SendRequest.sweep_all must be set explicitly and
// amt_sat must be zero; a typo'd amt_sat=0 without sweep_all is rejected
// up front at the wallet layer and again at the CLI.
package swapwallet
