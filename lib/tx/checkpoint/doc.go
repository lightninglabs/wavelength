package checkpoint

// Package checkpoint contains helpers for constructing and validating Ark
// checkpoint transactions.
//
// Checkpoints are taproot transactions that spend one or more VTXOs into a
// new on-chain output with defined closure semantics. They are used as the
// on-chain "checkpoint" for out-of-round transfers and, later, for other Ark
// lifecycle operations.
