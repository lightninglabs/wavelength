// Package coordinator joins durable vHTLC recovery rows with the generic
// unroll registry.
//
// The parent vhtlcrecovery package owns pure recovery data types and is
// imported by db. This child package may import unroll without creating an
// import cycle, so it owns the runtime handoff from recovery intent to
// per-target unroll execution.
package coordinator
