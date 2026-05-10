# swapclientserver

## Purpose

Daemon-side swap client subserver (build tag: `swapruntime`). Translates
`swapclientrpc` control-plane RPC calls into `sdk/swaps` operations, manages
one process-local background worker per payment hash, resumes all persisted
pending swap sessions at daemon start, and publishes coarse swap-state updates
to streaming subscribers. Swap FSM transitions, durable state machine logic,
OOR claim/refund behavior, and timeout handling remain in `sdk/swaps`.

## Key Types

- `Register` — Public entry point called from `cmd/darepod` (swapruntime build
  only). Opens the daemon-owned swap store, constructs the `sdk/swaps` client,
  registers the `SwapClientService` gRPC subserver on the existing daemon
  listener, resumes all persisted pending sessions, and returns a cleanup
  function that stops workers and closes the store during daemon shutdown.
- `swapClientService` — unexported service struct implementing
  `swapclientrpc.SwapClientServiceServer`. Owns the `swapRuntimeClient`
  interface, the swap store, daemon-root context, active-worker dedup map
  (`active map[string]struct{}`), and best-effort subscriber channels for
  `SubscribeSwaps`. Production code uses `swapClientAdapter` as the runtime
  client; unit tests supply a small fake.
- `swapRuntimeClient` — Narrow interface over the subset of `sdk/swaps` the
  subserver needs: `StartPayViaLightning`, `StartReceiveViaLightning`,
  `ResumePayViaLightning`, `ResumeReceiveViaLightning`, `GetSwapSummary`,
  `ListSwapSummaries`. Kept narrow so the daemon layer is unit-testable without
  running real swap FSMs.
- `swapClientAdapter` — Thin adapter wiring `sdk/swaps.SwapClient` to
  `swapRuntimeClient`. All state transitions, persistence, and claim/refund
  logic remain in the SDK.
- `paySwapSession` / `receiveSwapSession` — Narrow per-session interfaces
  (`PaymentHash`, `Wait`) used by daemon background workers. Production
  implementations are `sdk/swaps.PaySession` / `ReceiveSession`; tests supply
  deterministic blocking fakes.

## Relationships

- **Depends on**: `darepod` (`RPCServer`, `Config`, `SwapConfig`,
  `SwapSubsystem` logger key), `sdk/swaps` (`SwapClient`, `Store`,
  `SwapSummary`, `PayResult`, `ReceiveResult`), `sdk/ark` (in-process Ark SDK
  facade wired by `newSwapClientService`), `rpc/swapclientrpc` (generated gRPC
  service and proto types).
- **Depended on by**: `cmd/darepod` (conditionally, when built with `swapruntime`
  tag — calls `swapclientserver.Register` after gRPC server is ready).
- **Sends**: n/a — delegates all swap-state mutations to `sdk/swaps`.
- **Receives**:
  - ← API (`swapclientrpc.SwapClientService`): `StartPay`, `StartReceive`,
    `ResumeSwap`, `ListSwaps`, `GetSwap`, `SubscribeSwaps`.

## Invariants

- Only built and linked when the `swapruntime` build tag is present; a stub in
  `cmd/darepod/swapruntime_stub.go` satisfies the same call site when the tag
  is absent.
- One background worker per payment hash: `markActive` / `markInactive` use a
  `sync.Mutex`-guarded `active` map to prevent two goroutines from driving the
  same payment-hash FSM concurrently.
- Workers use the daemon-root context (`rootCtx`), not the individual RPC
  context, so CLI disconnects do not cancel an in-progress swap.
- `resumePending` is called once during `Register`, before the gRPC server is
  ready to accept new requests, so pending swaps resume before new ones can
  arrive.
- Receive-auth signing and ECDH are delegated to `darepod` via the Ark SDK
  facade; the subserver does not persist its own receive-auth key material.
- Subscriber channels are buffered and best-effort: slow subscribers are never
  blocked; clients recover current state via `GetSwap` / `ListSwaps`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Durable swap FSM and store.
