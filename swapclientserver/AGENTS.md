# swapclientserver

## Purpose

Optional daemon-side swap client subserver, built only with the `swapruntime`
build tag. Translates `swapclientrpc` control-plane RPC calls into
`sdk/swaps` operations, owns the process-local worker registry for pay and
receive sessions, and resumes all persisted pending swaps when the daemon
starts. Swap FSM transitions, mailbox receive-event handling, and swap server
protocol behavior remain entirely inside `sdk/swaps` and `swapdk-server`.

## Key Types

- `swapClientService` — Private gRPC server implementation of
  `swapclientrpc.SwapClientServiceServer`. Owns `rootCtx` (daemon lifetime,
  not per-RPC), the process-local `active` worker map, and the `subscribers`
  map for `SubscribeSwaps` streaming.
- `swapRuntimeClient` — Narrow interface over `sdk/swaps.SwapClient` that the
  subserver uses for all RPC handlers and worker restarts. Methods: `StartPayViaLightning`,
  `StartReceiveViaLightning`, `ResumePayViaLightning`,
  `ResumeReceiveViaLightning`, `GetSwapSummary`, `ListSwapSummaries`. Keeps
  the subserver unit-testable without running real swap FSMs.
- `swapClientAdapter` — Thin production adapter that forwards calls to
  `*swaps.SwapClient`.
- `paySwapSession` / `receiveSwapSession` — Minimal session interfaces
  (`PaymentHash`, `Wait`, and `Invoice` for receive) that the daemon
  goroutines drive. Production implementations are `sdk/swaps.PaySession` and
  `*receiveSessionAdapter`.
- `receiveSessionAdapter` — Adds method accessors over
  `sdk/swaps.ReceiveSession` so both production code and tests share the same
  interface without exposing struct fields.
- `creditServerBridge` / `creditDaemonBridge` (`credit_bridge.go`) — Adapters
  satisfying `credit.CreditServer` and `credit.CreditDaemon` respectively.
  `creditServerBridge` routes `CreateCredit`/`ListCredits`/`RedeemCredit`/
  `StartPay` back through the same `swapClientService` gRPC handlers the
  credit actor's underlying payment hash dedup and worker registry, so no
  swap logic is duplicated for the credit path. `creditDaemonBridge` exposes
  daemon-level primitives (`IdentityPubKey`, `DustLimit`, `SendOOR`,
  `AllocateReceiveScript`, `FindLiveVTXOByPkScript`) the credit durable actor
  needs but that don't belong on the swap RPC surface.
- `Register(ctx, grpcServer, rpcServer, cfg)` — Top-level entry point called
  by a `swapruntime`-tagged `darepod` binary. Opens the daemon-owned SQLite
  swap store, dials `swapdk-server`, creates an in-process Ark SDK facade over
  `darepod.RPCServer`, wires `swaps.NewSwapClientWithStore`, installs a
  `MailboxOutSwapEventReceiver` (empty mailbox ID — receiver derives the
  per-swap mailbox from client identity + payment hash) on the
  `SwapClient` so out-swap HTLC events flow over the mailbox transport,
  publishes `cfg.Swap.Backend`/`CreditServer`/`CreditDaemon` so the daemon's
  `credit` durable-actor subsystem and the `walletdkrpc` registrar can reach
  this subserver without a gRPC hop, registers the gRPC subserver, calls
  `resumePending` (unless `cfg.Swap.SuppressResume`), and returns a cleanup
  function.

## RPC Methods

| RPC | Description |
|-----|-------------|
| `QuotePay` | Preview a pay swap (fee, credit eligibility) without starting a worker or persisting state |
| `StartPay` | Persist a pay swap, start or reuse its daemon worker, return summary |
| `StartReceive` | Persist a receive swap, start or reuse its daemon worker, return invoice + summary |
| `ResumeSwap` | Manual wake-up for a persisted swap (idempotent if worker already active) |
| `ListSwaps` | List persisted swap summaries; optionally filter to pending only |
| `GetSwap` | Fetch one persisted summary by hex payment hash |
| `SubscribeSwaps` | Stream coarse summary updates; optionally emit existing rows first |
| `CreateCredit` | Create a credit operation (Lightning-receive or Ark-topup funded) for the `credit` subsystem |
| `RedeemCredit` | Debit reserved credit and route the redemption out via OOR |
| `ListCredits` | Snapshot of finalized/reserved/available credit balances and in-flight operations |

## Relationships

- **Depends on**: `sdk/swaps` (swap FSM, `SwapClient`, `Store`, session
  types), `sdk/ark` (`WrapDaemonServer`, in-process Ark facade), `darepod`
  (`RPCServer`, `Config`, `SwapConfig`, `SwapSubsystem`), `rpc/swapclientrpc`
  (generated gRPC stubs + proto types), `credit` (`CreditServer`/
  `CreditDaemon` interfaces the bridges implement, plus request/state enum
  mapping in `credit_bridge.go`).
- **Depended on by**: `cmd/darepod` (calls `swapclientserver.Register` when
  built with the `swapruntime` tag), `cmd/darepocli/darepoclicommands`
  (swap RPC CLI commands under `swapruntime`), `sdk/walletdk` (registers the
  subserver under the `walletdkrpc`+`swapruntime` build).
- **Sends**: daemon-root context to `sdk/swaps` session workers via
  `ResumePayViaLightning` / `ResumeReceiveViaLightning` — CLI disconnect does
  not cancel the worker because the subserver uses `rootCtx`, not the RPC
  context.
- **Receives**: ← API: `QuotePay`, `StartPay`, `StartReceive`, `ResumeSwap`,
  `ListSwaps`, `GetSwap`, `SubscribeSwaps`, `CreateCredit`, `RedeemCredit`,
  `ListCredits` from gRPC callers; ← `credit` durable actor (in-process, via
  `creditServerBridge`): `CreateCredit`, `ListCredits`, `RedeemCredit`,
  `StartPay`.

## Invariants

- Worker ownership is process-local and mutex-guarded: at most one goroutine
  drives a given payment hash at any time. `markActive` is the admission gate;
  `markInactive` releases it on goroutine exit.
- The daemon uses `rootCtx` (not the individual RPC contexts) for all
  `ResumePayViaLightning` / `ResumeReceiveViaLightning` calls. A CLI
  disconnect does not cancel an admitted swap.
- `SubscribeSwaps` subscribers are best-effort, buffered (16), and
  non-blocking. Slow subscribers may miss a terminal-state update; they can
  recover current state with `GetSwap` or `ListSwaps`.
- `Register` calls `resumePending` synchronously before returning so the
  daemon gRPC server begins accepting calls with all prior sessions already
  driven by a worker.
- Swap state, persistence, and protocol behavior are never duplicated in this
  layer — they stay in `sdk/swaps`. This package is a worker registry and RPC
  facade only.
- `idempotency_key` on `StartPay` / `StartReceive` is explicitly reserved and
  returns `Unimplemented` to guard against accidental duplicate-start
  assumptions.
- `QuotePay`/`StartPay` skip the VTXO-minimum preflight check only when
  `max_credit_sat > 0`: a credit-eligible pay can legitimately fund a
  sub-dust amount through the credit subsystem instead of a vHTLC, and the
  server credit quote (not this preflight) is the authority on whether
  credits cover it. A plain Lightning pay (`max_credit_sat == 0`) still
  must clear the operator's VTXO floor.
- `SetOutSwapEventReceiver` must run before any receive worker is started:
  `SwapClient` captures the receiver into the per-swap worker at start time,
  so a late install would leave already-running workers using whatever
  receiver was previously installed. `Register` therefore installs the
  mailbox receiver immediately after `NewSwapClientWithStore`, before
  `resumePending` revives persisted sessions.

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Daemon setup and
  CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
