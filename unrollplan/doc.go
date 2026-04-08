// Package unrollplan provides pure planning for unilateral-exit execution.
//
// The package owns no I/O. Callers provide an immutable recovery proof plus a
// durable progress snapshot, and the planner returns the next ready txs, the
// blocked frontier, CSV maturity details, and terminal completion state.
package unrollplan
