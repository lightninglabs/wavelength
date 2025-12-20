package arktx

// Package arktx contains helpers for constructing and validating Ark
// transactions that represent the virtual-chain step following checkpoints.
//
// Canonical output ordering is critical because multiple subsystems rely on
// byte-identical transaction construction (client retries, server validation,
// and persisted snapshots). This package provides a single, shared definition
// of that canonical ordering.
