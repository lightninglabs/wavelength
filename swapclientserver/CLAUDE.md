# swapclientserver

## Purpose

Optional daemon-side gRPC subserver (built with the `swapruntime` tag) that
exposes a `SwapClientService` RPC API over the existing daemon listener.
Translates control-plane RPC calls into `sdk/swaps` operations, owns the
process-local background worker registry, and resumes persisted pending swaps
on daemon startup. Does not implement swap FSM transitions, mailbox receive
handling, or swap server protocol behavior — those responsibilities stay in
`sdk/swaps`.

## Key Types

- `swapClientService` — Core gRPC service implementation (unexported). Owns
  the `swapRuntimeClient` adapter, daemon-owned `swaps.Store`, root context
  for background workers, active-worker deduplication map, and subscriber set
  for `SubscribeSwaps` streams.
- `swapRuntimeClient` — Narrow interface (unexported) between the daemon
  subserver and `sdk/swaps`. Methods: `StartPayViaLightning`,
  `StartReceiveViaLightning`, `ResumePayViaLightning`,
  `ResumeReceiveViaLightning`, `GetSwapSummary`, `ListSwapSummaries`.
  Production code uses `swapClientAdapter` wrapping `swaps.SwapClient`; tests
  provide a small fake without running real swap FSMs.
- `paySwapSession` / `receiveSwapSession` — Minimal interfaces (unexported) for
  swap FSM handles. `PaymentHash()` is the durable worker key; `Wait(ctx)`
  drives the FSM until terminal state or context cancel.
- `Register(ctx, grpcServer, rpcServer, cfg) (func(), error)` — Exported
  entry point called from `cmd/darepod` under the `swapruntime` build tag.
  Opens the daemon swap store, dials swapdk-server, creates an in-process Ark
  SDK facade via `sdk/ark.WrapDaemonServer`, wires `sdk/swaps.SwapClient`,
  registers `SwapClientServiceServer` on the daemon gRPC server, and resumes
  all persisted pending swaps. Returns a cleanup function that stops workers
  and closes stores/connections.

## Relationships

- **Depends on**: `sdk/swaps` (swap FSM and durable store), `sdk/ark`
  (`WrapDaemonServer` for in-process Ark SDK facade), `darepod` (Config,
  RPCServer, SwapSubsystem logger), `rpc/swapclientrpc` (generated gRPC
  stubs), `swaprpc` (swap server proto).
- **Depended on by**: `cmd/darepod` (registers via `Config.RPCServiceRegistrars`
  under `swapruntime` build tag).
- **Sends**: RPC calls to `sdk/swaps` via `swapRuntimeClient`; no actor-bus
  messages.
- **Receives**: ← API: `StartPay`, `StartReceive`, `ResumeSwap`, `ListSwaps`,
  `GetSwap`, `SubscribeSwaps`.

## Invariants

- Built only with `//go:build swapruntime`; the package is absent from default
  daemon builds and config-file `swap` fields are inert without it.
- One background goroutine per payment hash — `markActive` prevents duplicate
  workers for the same hash. Workers use `rootCtx` (not the individual RPC
  context) so CLI disconnects do not cancel in-flight swaps.
- Swap FSM state and persistence are owned by `sdk/swaps`; the daemon layer
  reads summaries but never writes swap state directly.
- `SubscribeSwaps` channels are best-effort: slow subscribers may miss
  updates, but can recover current state via `ListSwaps` or `GetSwap`.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Swap FSM internals.
- [sdk/ark/CLAUDE.md](../sdk/ark/CLAUDE.md) — In-process Ark SDK facade.
- [darepod/CLAUDE.md](../darepod/CLAUDE.md) — Daemon orchestrator and RPCServiceRegistrar wiring.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
