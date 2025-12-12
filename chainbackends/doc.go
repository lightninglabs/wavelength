// Package chainbackends provides concrete implementations of the
// chainsource.ChainBackend interface.
//
// The ChainBackend interface is defined in the chainsource package and
// provides a blockchain data abstraction layer for the Ark actor system. This
// package contains pluggable backend implementations that can be used depending
// on deployment requirements.
//
// # Available Backends
//
// LNDBackend: Full-node backend that wraps lnd's chain notification and fee
// estimation interfaces. Suitable for production deployments where lnd is
// available. Provides real-time notifications via lnd's chainntnfs package.
//
// # Usage
//
// Backends are instantiated and passed to the ChainSource actor during
// initialization. For in-process lnd:
//
//	backend := chainbackends.NewLNDBackend(
//		notifier, feeEstimator, broadcaster,
//	)
//	chainSource := chainsource.NewChainSourceActor(
//		backend, system, ctx,
//	)
//
// For remote lnd via lndclient:
//
//	backend := chainbackends.NewLNDBackendFromLndClient(lndServices)
//	chainSource := chainsource.NewChainSourceActor(
//		backend, system, ctx,
//	)
//
// # Architecture
//
// The separation between interface (chainsource) and implementations
// (chainbackends) provides several benefits:
//
//  1. Pluggability: Easy to swap backends without changing actor code
//  2. Testing: Mock backends can be created for testing
//  3. Dependencies: The actor system doesn't depend on lnd or HTTP libraries
//  4. Extensibility: New backends can be added without modifying core code
//
// # Implementation Requirements
//
// Backend implementations must:
//   - Implement all methods of chainsource.ChainBackend
//   - Be safe for concurrent use
//   - Handle context cancellation properly
//   - Clean up resources in Stop()
//   - Document any limitations or unsupported operations
//
// # Backend Selection Criteria
//
// LNDBackend is recommended when:
//   - Running a full node with lnd
//   - Real-time notifications are required
//   - Low latency is critical
//   - Full Bitcoin protocol support is needed
package chainbackends
