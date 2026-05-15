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
// The package owns the full swap lifecycle in-process. Its runtime drives a
// synchronous resume-on-startup sweep before the gRPC server accepts calls,
// enforces a wallet-level deadline watcher that transitions stuck entries
// to FAILED, and runs a monitor loop that fans normalized updates to
// SubscribeWallet subscribers. Background goroutines are anchored to the
// daemon root context, never to RPC-call contexts, so a CLI disconnect can
// never cancel in-flight work.
//
// The walletrpc build tag depends on swapruntime: building walletrpc without
// swapruntime is a deliberate compile error because the subserver composes
// the daemon-owned swap subsystem.
package swapwallet
