// Package walletdk provides an opinionated wallet SDK over a waved runtime.
//
// The package is intentionally a facade over the daemon and its RPC services.
// Start embeds the daemon in-process, connects to it over bufconn, and exposes
// a small wallet-shaped API for onboarding, balances, deposits, sends,
// receives, and unified activity history. The client also exposes raw gRPC
// escape hatches, including btcsuite btcwallet's native walletrpc service for
// underlying on-chain wallet operations. Connect exposes the same API against
// an external daemon.
//
// Embedded wallet payment operations require building with the walletdkrpc and
// swapruntime tags so the daemon-owned wallet executor is registered.
package walletdk
