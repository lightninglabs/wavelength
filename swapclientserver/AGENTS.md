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
  subserver uses for all RPC handlers and worker restarts. Methods:
  `QuotePayViaLightning`, `StartPayViaLightning`, `StartReceiveViaLightning`,
  `ResumePayViaLightning`, `ResumeReceiveViaLightning`, `GetSwapSummary`,
  `ListSwapSummaries`. Credit operations (`CreateCredit`, `RedeemCredit`,
  `ListCredits`) are optional and reached via runtime type assertion, not
  part of the interface. Keeps the subserver unit-testable without running
  real swap FSMs.
- `swapClientAdapter` — Thin production adapter that forwards calls to
  `*swaps.SwapClient`.
- `paySwapSession` / `receiveSwapSession` — Minimal session interfaces
  (`PaymentHash`, `Wait`, and `Invoice` for receive) that the daemon
  goroutines drive. Production implementations are `sdk/swaps.PaySession` and
  `*receiveSessionAdapter`.
- `receiveSessionAdapter` — Adds method accessors over
  `sdk/swaps.ReceiveSession` so both production code and tests share the same
  interface without exposing struct fields.
- `creditServerBridge` / `creditDaemonBridge` — Adapt the subserver and the
  in-process Ark/daemon facade to the `credit` package's `CreditServer` /
  `CreditDaemon` interfaces, so the credit durable-actor subsystem reuses this
  package's account-key resolution, payment-hash dedup, and worker registry
  instead of duplicating them.
- `Register(ctx, grpcServer, rpcServer, cfg)` — Top-level entry point called
  by a `swapruntime`-tagged `darepod` binary. Opens the daemon-owned SQLite
  swap store, dials `swapdk-server`, creates an in-process Ark SDK facade over
  `darepod.RPCServer`, wraps it with `daemonWithLiveOperatorKey` (see
  Invariants) before wiring `swaps.NewSwapClientWithStore`, installs a
  `MailboxOutSwapEventReceiver` (empty mailbox ID — receiver derives the
  per-swap mailbox from client identity + payment hash) on the
  `SwapClient` so out-swap HTLC events flow over the mailbox transport, calls
  `installVTXOForfeitParticipantSigner` to register the daemon's in-swap
  forfeit signer with `darepod.RPCServer`, publishes
  `cfg.Swap.Backend`/`CreditServer`/`CreditDaemon` bridges, registers the gRPC
  subserver, calls `resumePending` (unless `cfg.Swap.SuppressResume`), and
  returns a cleanup function.
- `liveOperatorDaemonConn` — Wraps `swaps.DaemonConn` so `OperatorPubKey`
  always reads live from the daemon's direct Ark transport
  (`rpcServer.OperatorPubKey`) instead of the cached `GetInfo` snapshot,
  ensuring newly-created swap vHTLC policies see operator-key rotations
  before OOR funding is submitted.

## RPC Methods

| RPC | Description |
|-----|-------------|
| `QuotePay` | Preview a pay swap without creating durable state or a worker |
| `StartPay` | Persist a pay swap, start or reuse its daemon worker, return summary |
| `StartReceive` | Persist a receive swap, start or reuse its daemon worker, return invoice + summary |
| `CreateCredit` | Start a durable server-owned credit funding operation |
| `RedeemCredit` | Materialize available credits back into an Ark output |
| `ListCredits` | Return the server-authoritative credit account snapshot |
| `ResumeSwap` | Manual wake-up for a persisted swap (idempotent if worker already active) |
| `ListSwaps` | List persisted swap summaries; optionally filter to pending only |
| `GetSwap` | Fetch one persisted summary by hex payment hash |
| `SubscribeSwaps` | Stream coarse summary updates; optionally emit existing rows first |

## Relationships

- **Depends on**: `sdk/swaps` (swap FSM, `SwapClient`, `Store`, session,
  credit types), `sdk/ark` (`WrapDaemonServer`, in-process Ark facade),
  `darepod` (`RPCServer`, `Config`, `SwapConfig`, `SwapSubsystem`,
  `SetVTXOForfeitParticipantSigner`, `OperatorPubKey`), `credit`
  (`CreditServer`/`CreditDaemon` interfaces bridged for the credit actor
  subsystem), `vtxo`/`lib/types` (forfeit-participant-signature request and
  result types for the installed VTXO forfeit signer), `rpc/swapclientrpc`
  (generated gRPC stubs + proto types).
- **Depended on by**: `cmd/darepod` (calls `swapclientserver.Register` when
  built with the `swapruntime` tag), `cmd/darepocli/darepoclicommands`
  (swap RPC CLI commands under `swapruntime`).
- **Sends**: daemon-root context to `sdk/swaps` session workers via
  `ResumePayViaLightning` / `ResumeReceiveViaLightning` — CLI disconnect does
  not cancel the worker because the subserver uses `rootCtx`, not the RPC
  context.
- **Receives**: ← API: `QuotePay`, `StartPay`, `StartReceive`, `CreateCredit`,
  `RedeemCredit`, `ListCredits`, `ResumeSwap`, `ListSwaps`, `GetSwap`,
  `SubscribeSwaps` from gRPC callers.

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
- `Register` calls `resumePending` synchronously before returning (unless
  `cfg.Swap.SuppressResume`, in which case a higher subserver drives
  `ResumePending` itself) so the daemon gRPC server begins accepting calls
  with all prior sessions already driven by a worker.
- Swap state, persistence, and protocol behavior are never duplicated in this
  layer — they stay in `sdk/swaps`. This package is a worker registry and RPC
  facade only.
- `idempotency_key` on `StartPay` / `StartReceive` is explicitly reserved and
  returns `Unimplemented` to guard against accidental duplicate-start
  assumptions; by contrast `CreateCredit` / `RedeemCredit` require a
  caller-supplied `idempotency_key` today.
- `CreateCredit`/`RedeemCredit`/`ListCredits` type-assert `s.client` for the
  optional credit methods and return `Unimplemented` if the underlying
  `swapRuntimeClient` does not support credits.
- `Register` calls `installVTXOForfeitParticipantSigner`, which registers a
  closure with `darepod.RPCServer.SetVTXOForfeitParticipantSigner` that
  routes forfeit-round participant-signature requests through
  `swapServer.SignInSwapForfeit` (via
  `swaps.ForfeitSignaturePayloadFromVTXORequest`). This lets the daemon's
  VTXO forfeit path collect the swap counterparty's signature without the
  daemon core depending on `sdk/swaps` directly. `newSwapClientService`
  fails registration (and unwinds partially-opened resources) if this wiring
  errors.
- `QuotePay`/`StartPay` only enforce the operator VTXO-minimum preflight
  (`validatePayInvoiceAmount`, via `vtxoMinAmountSat`) when the request is
  not credit-eligible (`max_credit_sat == 0`); a nonzero `max_credit_sat`
  defers the amount check to the server-side credit quote so a sub-dust,
  credit-assisted pay is not rejected locally.
- When a caller omits `max_fee_sat` (leaves it 0) on `QuotePay`/`StartPay`,
  `effectiveMaxFeeSat` derives a default cap of ~1% of the decoded invoice
  amount (`defaultInSwapMaxFeePPM`), floored at
  `defaultInSwapMaxFeeFloorSat` (10 sat), so a normal payment is not
  rejected by an implicit 0 sat cap. A caller-supplied nonzero value is
  always honored verbatim. `wrapInSwapFeeError` rewrites the resulting
  fee-cap rejection to name the effective cap and how to raise it.
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
