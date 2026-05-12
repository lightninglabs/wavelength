// Package walletdk provides an opinionated wallet SDK over a darepod runtime.
//
// The package is intentionally a facade over the daemon and its RPC services.
// Start embeds the daemon in-process, connects to it over bufconn, and exposes
// a small wallet-shaped API for onboarding, balances, Lightning-to-Ark
// receives, Ark-to-Lightning sends, and swap accounting. Advanced callers can
// still fetch the underlying daemon and swap RPC clients for lower-level work.
//
// Swap send and receive operations require building with the swapruntime tag so
// the daemon-owned swap executor is registered on the embedded RPC server.
package walletdk
