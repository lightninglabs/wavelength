// Package chainbackends provides concrete implementations of the
// chainsource.ChainBackend interface.
//
// The ChainBackend interface is defined in arkmachine/actors/chainsource and
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
// ExplorerBackend: Light client backend using HTTP APIs from block explorers
// (such as mempool.space). Suitable for lightweight deployments without a full
// node. Uses polling with exponential backoff for eventual consistency.
//
// # Usage
//
// Backends are instantiated and passed to the ChainSource actor during
// initialization:
//
//	// Using LND backend
//	backend := chainbackends.NewLNDBackend(notifier, feeEstimator, wallet)
//	chainSource := chainsource.NewChainSourceActor(backend, system)
//
//	// Using Explorer backend
//	backend := chainbackends.NewExplorerBackend("https://mempool.space/api")
//	chainSource := chainsource.NewChainSourceActor(backend, system)
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
//  - Implement all methods of chainsource.ChainBackend
//  - Be safe for concurrent use
//  - Handle context cancellation properly
//  - Clean up resources in Stop()
//  - Document any limitations or unsupported operations
//
// # Backend Selection Criteria
//
// Choose LNDBackend when:
//  - Running a full node with lnd
//  - Real-time notifications are required
//  - Low latency is critical
//  - Full Bitcoin protocol support is needed
//
// Choose ExplorerBackend when:
//  - Running a light client without a full node
//  - Eventual consistency is acceptable
//  - Network bandwidth/storage is limited
//  - Simplified deployment is preferred
package chainbackends
