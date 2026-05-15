// Package walletdk provides an opinionated wallet SDK over a darepod runtime.
//
// The package is intentionally a facade over the daemon and its RPC services.
// Start embeds the daemon in-process, connects to it over bufconn, and exposes
// a small wallet-shaped API for onboarding, balances, deposits, sends,
// receives, and unified activity history. Connect exposes the same API against
// an external daemon.
//
// Embedded wallet payment operations require building with the walletrpc and
// swapruntime tags so the daemon-owned wallet executor is registered.
package walletdk
