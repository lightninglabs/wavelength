# swapwallet

## Purpose

Daemon-side `WalletService` gRPC subserver (build tags: `walletrpc` +
`swapruntime`). Composes the swap subsystem, the Ark SDK facade, the
daemon-managed signer, the cooperative-leave RPC, the wallet actor, and the
unified ledger history surface into a single swap-vocabulary-free wallet API.
Callers see Send, Recv, List, Deposit, Balance, Status, and SubscribeWallet
without ever encountering swap or OOR concepts.

## Key Types

- `Service` — gRPC handler implementing `walletrpc.WalletServiceServer`. Pure
  wiring: each method delegates to `router`, `recv`, `history`, or `runtime`.
- `Deps` — Aggregate dependency bag: `RPCServer` (daemon-side RPC handle),
  `SwapBackend` (swap resume/execution), config fields
  (`SwapWalletConfig`), logger.
- `RPCServer` — Interface over `darepod.RPCServer` exposing the specific
  methods `swapwallet` needs (LeaveVTXOs, CreateBoardingAddress, GetBalance,
  SignReceiveAuthMessage, ListTransactions, etc.) without importing darepod.
- `Runtime` — Owns the in-process swap lifecycle: synchronous
  resume-on-startup sweep, wallet-level deadline watcher (overlays FAILED
  status on timed-out entries), subscribe fan-out via per-subscriber channels,
  and pending-entry tracking.
- `Register(ctx, grpcServer, rpcServer, cfg)` — Entry point called by
  `darepod` when both build tags are present. Wires deps, starts the runtime,
  drives the synchronous resume sweep, and registers the `WalletService`
  handler.

## Relationships

- **Depends on**: `darepod` (RPCServer interface), `rpc/walletrpc` (proto
  stubs), `swapclientserver` (swap backend via `darepod.SwapBackend`),
  `sdk/swaps` (swap FSM types), `google.golang.org/grpc`.
- **Depended on by**: `cmd/darepod` (registers the subserver when both
  `walletrpc` and `swapruntime` build tags are present), `sdk/walletdk`
  (consumes via the `WalletServiceClient` gRPC stub).
- **Sends**:
  - → `darepod.RPCServer`: LeaveVTXOs, GetBalance, ListTransactions,
    CreateBoardingAddress, SignReceiveAuthMessage, and other RPCServer methods
    (in-process calls, not gRPC).
  - → swap backend: `ResumePending` (once at startup), then per-receive and
    per-send swap invocations.
- **Receives**:
  - ← `cmd/darepod` (via `Register`): daemon startup wiring.
  - ← API (gRPC): `SendRequest`, `RecvRequest`, `ListRequest`,
    `DepositRequest`, `BalanceRequest`, `StatusRequest`,
    `SubscribeWalletRequest`.

## Invariants

- Build tag guard: `walletrpc` implies `swapruntime`; building walletrpc
  without swapruntime is a deliberate compile error because `swapwallet`
  depends on the swap executor.
- The runtime drives a synchronous `ResumePending` sweep before the gRPC
  server accepts wallet calls, ensuring all prior pending sessions have active
  workers before the first new call arrives.
- Background goroutines (deadline watcher, monitor fan-out) are anchored to
  the daemon root context passed to `Register`, never to RPC-call contexts.
  A CLI disconnect cannot cancel in-flight swap work.
- The deadline watcher overlays the wallet-level FAILED status above the swap
  FSM's own deadline; it never mutates underlying swap state.
- `SubscribeWallet` delivers entries on per-subscriber buffered channels;
  a slow consumer that fills its buffer will drop updates and must reconcile
  via `List` on reconnect.
- `SuppressResume` on `darepod.SwapConfig` is set by `Register` so
  `swapclientserver.Register` skips its own resume sweep and lets `swapwallet`
  own the unified resume lifecycle.

## Deep Docs

- [rpc/walletrpc/CLAUDE.md](../rpc/walletrpc/CLAUDE.md) — Generated proto
  stubs consumed by this package.
- [swapclientserver/CLAUDE.md](../swapclientserver/CLAUDE.md) — Swap subserver
  this package coordinates with for resume and execution.
- [sdk/walletdk/CLAUDE.md](../sdk/walletdk/CLAUDE.md) — SDK that exposes
  the wallet API to host applications.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
