# cmd/darepod

## Purpose

Daemon entry point. Parses flags, initializes configuration, and starts the
`darepod.Server`.

Flag registration is split into small `register*Flags` helpers in `main.go`,
including `registerPprofFlags`, `registerMetricsFlags` (Prometheus
`metrics.listen`), `registerFeeEstimationFlags` (mempool.space fee-provider
flags for the lnd backend), `registerArkServerFlags`, `registerSwapRuntimeFlags`,
and `registerBitcoindFlags`, plus the standalone SQLite durability flags
(`db.sqlite.synchronous`, `db.sqlite.nofullfsync`) registered directly in
`newRootCmd`.

The walletdkrpc build tag (`walletdkrpc.go`) also appends
`swapwallet.ErrorMappingInterceptor` to `cfg.UnaryServerInterceptors`,
mapping walletdkrpc sentinel errors to gRPC status codes.

## Relationships

- **Depends on**: `darepod` (Server orchestrator), `swapwallet` (walletdkrpc
  build tag only).
- **Depended on by**: nothing (binary entry point).
