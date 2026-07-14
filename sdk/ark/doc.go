// Package ark provides a consumer-facing Go SDK for interacting with a
// waved runtime. Callers can connect to a remote daemon over gRPC, embed a
// daemon in-process and talk to it over an injected in-memory listener, or
// wrap an already-running in-process daemon RPC server behind a private
// bufconn transport.
//
// The package intentionally treats waved as the canonical Ark runtime. This
// SDK owns transport, lifecycle, and Go ergonomics; it does not reimplement
// wallet, round, OOR, or persistence logic outside the daemon.
// Higher-level Ark policy, OOR, and indexed-lookup convenience methods also
// live here so future layers such as sdk/swaps can depend on sdk/ark instead
// of rebuilding waverpc adapter logic themselves.
//
// Readiness is staged. Callers should use GetInfo to learn which parts of the
// runtime are ready: WalletReady gates wallet-dependent RPCs, ServerConnected
// reports whether mailbox ingress is currently running, and ServerInfo stays
// nil until operator terms have been cached during bootstrap. Round-oriented
// callers should wait until WalletReady is true, ServerConnected is true, and
// ServerInfo is non-nil before attempting round-sensitive operations. The
// cached ServerInfo snapshot is currently refreshed only during bootstrap, so
// callers should treat it as the latest known terms for the current session.
//
// Pre-1.0, some SDK methods intentionally return waverpc-generated protobuf
// types directly. Those passthrough shapes are not yet stable and may change
// as richer SDK-owned models are introduced. Higher-level callers such as a
// future sdk/swaps package should treat those responses as a temporary
// compatibility layer rather than a long-term public model commitment.
//
// Client is safe for concurrent use. Embedded clients expose Wait so callers
// can block on the in-process daemon's terminal run error without waiting for
// Close. Remote clients return an already-closed Wait channel because there
// is no in-process runtime to supervise.
//
// See docs/sdk_layered_architecture.md for the layered architecture, error
// categorization goals, and waverpc versioning expectations that this
// package builds on.
package ark
