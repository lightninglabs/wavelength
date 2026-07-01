# swapclientserver

## Purpose

Optional daemon-side swap client subserver, built only with the `swapruntime`
build tag. Translates `swapclientrpc` control-plane RPC calls into
`sdk/swaps` operations, owns the process-local worker registry for pay and
receive sessions, resumes all persisted pending swaps when the daemon starts,
and bridges the daemon's account identity / OOR / VTXO surface into the
`credit` durable-actor subsystem. Swap FSM transitions, mailbox receive-event
handling, and swap server protocol behavior remain entirely inside
`sdk/swaps` and `swapdk-server`.

## Key Types

- `swapClientService` — Private gRPC server implementation of
  `swapclientrpc.SwapClientServiceServer`. Owns `rootCtx` (daemon lifetime,
  not per-RPC), the process-local `active` worker map, the `subscribers`
  map for `SubscribeSwaps` streaming, and `daemonConn` (the in-process
  `swaps.DaemonConn` also handed to the credit bridge).
- `swapRuntimeClient` — Narrow interface over `sdk/swaps.SwapClient` that the
  subserver uses for all RPC handlers and worker restarts. Required methods:
  `QuotePayViaLightning`, `StartPayViaLightning`, `StartReceiveViaLightning`,
  `ResumePayViaLightning`, `ResumeReceiveViaLightning`, `GetSwapSummary`,
  `ListSwapSummaries`. Credit-aware variants
  (`QuotePayViaLightningWithCredits`, `StartPayViaLightningWithCredits`,
  `CreateCredit`, `RedeemCredit`, `ListCredits`) are optional: `quotePay` /
  `startPay` / the credit RPC handlers type-assert for them and fall back to
  the plain methods (or `Unimplemented`) when a test double does not
  implement them. Keeps the subserver unit-testable without running real
  swap FSMs.
- `swapClientAdapter` — Thin production adapter that forwards calls to
  `*swaps.SwapClient`, including the credit-aware methods above.
- `paySwapSession` / `receiveSwapSession` — Minimal session interfaces
  (`PaymentHash`, `Wait`, and `Invoice` for receive) that the daemon
  goroutines drive. Production implementations are `sdk/swaps.PaySession` and
  `*receiveSessionAdapter`.
- `receiveSessionAdapter` — Adds method accessors over
  `sdk/swaps.ReceiveSession` so both production code and tests share the same
  interface without exposing struct fields.
- `liveOperatorDaemonConn` / `daemonWithLiveOperatorKey` — Wraps
  `swaps.DaemonConn` so `OperatorPubKey` always fetches live from the direct
  Ark transport (`rpcServer.OperatorPubKey`) instead of the cached
  `GetInfo` snapshot, so newly-created swap vHTLC policies see operator-key
  rotations before OOR funding is submitted. All other `DaemonConn` methods
  pass through to the embedded connection.
- `creditServerBridge` / `creditDaemonBridge` (`credit_bridge.go`) — Adapt
  the subserver to `credit.CreditServer` and `credit.CreditDaemon`.
  `creditServerBridge` routes `CreateCredit`/`ListCredits`/`RedeemCredit`/
  `StartPay` through the corresponding `swapClientService` gRPC handlers, so
  the credit actor reuses the daemon's account-key resolution, payment-hash
  dedup, and worker registry. `creditDaemonBridge` exposes
  `IdentityPubKey`, `DustLimit`, `SendOOR` (idempotency-keyed pubkey OOR),
  `AllocateReceiveScript`, and `FindLiveVTXOByPkScript` by calling
  `swaps.DaemonConn` and `darepod.RPCServer` directly. `Register` publishes
  both bridges on `cfg.Swap.CreditServer` / `cfg.Swap.CreditDaemon`;
  `darepod`'s `credit_registry.go` reads them to construct the credit
  durable-actor subsystem, so credit only exists when the swap runtime is
  built in.
- `ensureSwapDBDir` — Platform seam for creating the swap-store directory,
  split across `fs_native.go` (`//go:build swapruntime && (!js || !wasm)`,
  real `os.MkdirAll`) and `fs_wasm.go` (`//go:build swapruntime && js &&
  wasm`, no-op because the SQLite driver maps the filename to OPFS, which has
  no host directories).
- `Register(ctx, grpcServer, rpcServer, cfg)` — Top-level entry point called
  by a `swapruntime`-tagged `darepod` binary. Opens the daemon-owned SQLite
  swap store, dials `swapdk-server`, creates an in-process Ark SDK facade over
  `darepod.RPCServer`, wraps it with `daemonWithLiveOperatorKey`, wires
  `swaps.NewSwapClientWithStore`, installs a `MailboxOutSwapEventReceiver`
  (empty mailbox ID — receiver derives the per-swap mailbox from client
  identity + payment hash) and the VTXO forfeit-participant signer
  (`installVTXOForfeitParticipantSigner`, delegates to
  `swapServer.SignInSwapForfeit`) on the daemon, publishes the credit
  bridges on `cfg.Swap`, registers the gRPC subserver, calls
  `resumePending`, and returns a cleanup function.

## RPC Methods

| RPC | Description |
|-----|-------------|
| `QuotePay` | Preview a pay swap (amount, fee, settlement type, credit quote) without persisting state or starting a worker |
| `StartPay` | Persist a pay swap, start or reuse its daemon worker, return summary |
| `StartReceive` | Persist a receive swap, start or reuse its daemon worker, return invoice + summary (including credit-assisted plan fields) |
| `ResumeSwap` | Manual wake-up for a persisted swap (idempotent if worker already active) |
| `ListSwaps` | List persisted swap summaries; optionally filter to pending only |
| `GetSwap` | Fetch one persisted summary by hex payment hash |
| `SubscribeSwaps` | Stream coarse summary updates; optionally emit existing rows first |
| `CreateCredit` | Start a server-owned credit funding operation (Lightning receive or Ark top-up) for the daemon identity account |
| `RedeemCredit` | Materialize available credit back into an Ark output via OOR |
| `ListCredits` | Return the server-authoritative credit snapshot (balances, operations, ledger entries) |

## Relationships

- **Depends on**: `sdk/swaps` (swap FSM, `SwapClient`, `Store`, session,
  credit types), `sdk/ark` (`WrapDaemonServer`, in-process Ark facade),
  `credit` (`CreditServer`, `CreditDaemon`, domain types bridged in
  `credit_bridge.go`), `vtxo` (`ForfeitParticipantSignRequest`), `lib/types`
  (`ForfeitParticipantSig`), `daemonrpc` (`GetInfoRequest`, `SendOORRequest`,
  `Output`), `darepod` (`RPCServer`, `Config`, `SwapConfig`,
  `SwapSubsystem`), `rpc/swapclientrpc` (generated gRPC stubs + proto types).
- **Depended on by**: `cmd/darepod` (calls `swapclientserver.Register` when
  built with the `swapruntime` tag), `cmd/darepocli/darepoclicommands`
  (swap RPC CLI commands under `swapruntime`), `darepod`'s
  `credit_registry.go` (reads `cfg.Swap.CreditServer` /
  `cfg.Swap.CreditDaemon`, populated by `Register`, to construct the credit
  durable-actor subsystem).
- **Sends**: daemon-root context to `sdk/swaps` session workers via
  `ResumePayViaLightning` / `ResumeReceiveViaLightning` — CLI disconnect does
  not cancel the worker because the subserver uses `rootCtx`, not the RPC
  context. Also sends idempotency-keyed pubkey OOR transfers to `darepod`'s
  `RPCServer.SendOOR` on behalf of `creditDaemonBridge.SendOOR`, and forfeit
  co-signing requests to the swap server via
  `swapServer.SignInSwapForfeit` from the daemon's
  `VTXOForfeitParticipantSigner` hook.
- **Receives**: ← API: `QuotePay`, `StartPay`, `StartReceive`, `ResumeSwap`,
  `ListSwaps`, `GetSwap`, `SubscribeSwaps`, `CreateCredit`, `RedeemCredit`,
  `ListCredits` from gRPC callers. ← `credit` actor: `CreateCredit`,
  `ListCredits`, `RedeemCredit`, `StartPay` calls through
  `creditServerBridge`, and `IdentityPubKey`/`DustLimit`/`SendOOR`/
  `AllocateReceiveScript`/`FindLiveVTXOByPkScript` calls through
  `creditDaemonBridge`.

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
  assumptions. `CreateCredit` / `RedeemCredit`, by contrast, require a
  caller-supplied `idempotency_key` and reject an empty one with
  `InvalidArgument` — the credit path relies on the caller's key for dedup
  rather than reserving it.
- `SetOutSwapEventReceiver` must run before any receive worker is started:
  `SwapClient` captures the receiver into the per-swap worker at start time,
  so a late install would leave already-running workers using whatever
  receiver was previously installed. `Register` therefore installs the
  mailbox receiver immediately after `NewSwapClientWithStore`, before
  `resumePending` revives persisted sessions.
- `StartPay` and `StartReceive` no longer reject sub-dust/sub-VTXO-minimum
  amounts synchronously: a credit-eligible pay (`max_credit_sat > 0`) or a
  credit-assisted receive can be smaller than the operator's VTXO floor, so
  the amount preflight now lives in `QuotePay` and only runs when
  `max_credit_sat == 0` (`validatePayInvoiceAmount`); the swap server's own
  credit quote decides whether an otherwise-too-small amount is admissible.
  `validateReceiveAmount` is retained but currently has no caller in this
  package — the receive floor is enforced server-side once credits can cover
  the gap.
- A caller-supplied `max_fee_sat == 0` on `QuotePay` / `StartPay` is not
  forwarded as a literal zero-fee cap: `effectiveMaxFeeSat` replaces it with
  a proportional default (`defaultInSwapMaxFeeSat`, ~1% of the decoded
  invoice amount in `defaultInSwapMaxFeePPM`, floored at
  `defaultInSwapMaxFeeFloorSat` sat) so a routine payment is not rejected by
  an implicit 0 sat cap. `wrapInSwapFeeError` rewrites the resulting
  "exceeds max fee" rejection into a message naming the effective cap and the
  `--max_fee` override.
- `daemonWithLiveOperatorKey` must wrap the Ark facade before it is handed to
  `swaps.NewSwapClientWithStore`: `OperatorPubKey` reads must bypass the
  cached `GetInfo` snapshot so a freshly-created vHTLC policy always funds
  against the current operator key, not a stale one from daemon startup.
- The credit bridges (`cfg.Swap.CreditServer` / `cfg.Swap.CreditDaemon`) must
  be published before `Register` returns: `darepod`'s credit registry treats
  a nil `CreditServer` or `CreditDaemon` as "credits unavailable" and skips
  constructing the credit subsystem entirely (expected in builds without the
  `swapruntime` tag, a bug if it happens under it).

## Deep Docs

- [docs/daemon_cli_guide.md](../docs/daemon_cli_guide.md) — Daemon setup and
  CLI reference.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
