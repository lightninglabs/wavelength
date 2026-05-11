# swapclientserver

## Purpose

Daemon-side control-plane subserver for the optional swap runtime (build tag
`swapruntime`). Translates `swapclientrpc` gRPC calls into `sdk/swaps`
operations, owns the daemon-local worker registry, and resumes persisted
pending swaps when the daemon starts. Does not implement swap FSM transitions,
mailbox receive-event handling, or swap server protocol behavior — those
responsibilities stay in `sdk/swaps`.

## Key Types

- `Register(ctx, grpcServer, rpcServer, cfg) (cleanup func(), err error)` —
  Single entry point called by `cmd/darepod` when built with `swapruntime`.
  Creates the swap store, wires an `sdk/swaps.SwapClient`, registers the gRPC
  server, and calls `resumePending` to restart in-progress sessions. Returns a
  cleanup function that cancels the root context and stops all background
  workers.
- `swapClientService` — Internal unexported type implementing
  `swapclientrpc.SwapClientServiceServer`. Holds the root context, swap store,
  and active/subscriber maps. RPC handlers delegate to the `swapRuntimeClient`
  interface; background workers drive each session to termination.
- `swapRuntimeClient` — Narrow interface over `sdk/swaps.SwapClient` (8
  methods: `StartPayViaLightning`, `StartReceiveViaLightning`,
  `ResumePayViaLightning`, `ResumeReceiveViaLightning`, `GetSwapSummary`,
  `ListSwapSummaries`). Tests supply a small fake without running real swap
  FSMs.
- `paySwapSession` / `receiveSwapSession` — Internal interfaces for background
  worker control (`Wait`, `PaymentHash`, `Invoice`).

## Relationships

- **Depends on**: `darepod` (config, `RPCServer`, wallet access via
  `daemonInvoiceGenerator`), `sdk/swaps` (swap FSM and store), `sdk/ark`
  (Ark client facade for in-process daemon invoice generation),
  `rpc/swapclientrpc` (generated gRPC stubs).
- **Depended on by**: `cmd/darepod` (via `RPCServiceRegistrar` hook in
  `Config.RPCServiceRegistrars`; the `swapruntime.go`/`swapruntime_stub.go`
  build-tag pair wires or stubs the registration).
- **Sends**: → `sdk/swaps`: `StartPayViaLightning`, `StartReceiveViaLightning`,
  `ResumePayViaLightning`, `ResumeReceiveViaLightning`, `GetSwapSummary`,
  `ListSwapSummaries`.
- **Receives**: ← API (`swapclientrpc`): `StartPay`, `StartReceive`,
  `ResumeSwap`, `ListSwaps`, `GetSwap`, `SubscribeSwaps`.

## Invariants

- Build tag `swapruntime` gates all files; the stub in `cmd/darepod` provides
  a no-op registrar when the tag is absent so the daemon binary builds without
  swap support.
- Each payment hash is owned by exactly one background worker at a time;
  `markActive`/`markInactive` serialize concurrent `StartPay`,
  `StartReceive`, and `ResumeSwap` calls.
- Background workers use `rootCtx` (not the RPC request context) so a CLI
  disconnect does not cancel an admitted swap.
- `resumePending` is called once during `Register`; it re-creates workers
  for every session not yet in a terminal state according to the swap store.
- `SubscribeSwaps` subscribers receive coarse `SwapSummary` updates on each
  payment-hash state change; slow subscribers are dropped via `removeSubscriber`
  to prevent the publish channel from blocking.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
