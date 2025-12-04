// Package chainresolver provides client-side on-chain resolution capabilities
// for Ark protocol VTXOs.
//
// This package implements the client's perspective of chain resolution, which
// differs fundamentally from the server's ChainResolver. While the server
// focuses on fraud detection and punishment, the client focuses on:
//
//   - VTXO Monitoring: Track user's VTXOs for confirmations and spending
//   - Unroll Initiation: Broadcast VTXT path to claim VTXOs on-chain
//   - Defensive Monitoring: Detect unexpected spends of user's VTXOs
//   - CSV Timeout Recovery: Unilaterally sweep VTXOs after timeout
//   - Boarding UTXO Tracking: Monitor boarding deposits
//
// # Architecture
//
// The package follows the actor pattern consistent with the rest of the Ark
// client implementation:
//
//   - ClientResolverActor: Main actor handling VTXO monitoring and unroll
//   - VTXOMonitorActor: Per-VTXO sub-actor tracking confirmations and spends
//
// # Key Differences from Server ChainResolver
//
// The client ChainResolver operates from the user's perspective:
//
//   - Only knows about the user's own VTXOs (not all batches)
//   - Cannot punish fraud (no forfeit txs, no checkpoint chains)
//   - Uses CSV timeout path for unilateral recovery
//   - Monitors for unexpected spends as a defensive measure
//
// # Usage Example
//
//	// Create a client resolver actor.
//	clientResolver := chainresolver.NewClientResolverActor(
//	    chainSourceRef, system, signer, log,
//	)
//	resolverRef := chainresolver.ClientResolverKey.Spawn(
//	    system, "client-resolver", resolver,
//	)
//
//	// Monitor a VTXO
//	req := &chainresolver.MonitorVTXORequest{
//	    VTXOOutpoint: vtxoOutpoint,
//	    VTXOOutput:   vtxoOutput,
//	    TreePath:     userTreePath,
//	    ExitDelay:    144, // CSV timeout
//	    NotifyActor:  fn.Some(notifyRef),
//	}
//	future := resolverRef.Ask(ctx, req)
//
// # Thread Safety
//
// All actor types in this package are designed to be used within the actor
// system's concurrency model. Direct field access should only occur within
// the actor's Receive method.
package chainresolver
