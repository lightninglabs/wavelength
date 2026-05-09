# swapclientserver

## Purpose

Optional daemon-side swap client subserver, compiled only with the `swapruntime`
build tag. Translates `swapclientrpc` control-plane calls into `sdk/swaps`
operations, owns the daemon-local background-worker registry, and resumes
persisted pending swaps on daemon startup. Deliberately does not implement swap
FSM transitions, mailbox receive-event handling, or swap server protocol
behavior — those responsibilities stay in `sdk/swaps`.

## Key Types

- `swapClientService` — gRPC handler implementing `SwapClientServiceServer`.
  Owns the per-payment-hash worker registry and subscriber set; validates RPC
  requests and translates between proto and SDK summary models.
- `swapRuntimeClient` — Narrow interface over `sdk/swaps.SwapClient`
  (`Start`, `Resume`, `Get` operations) used by all RPC handlers and
  background workers. Test fakes implement this seam without running real FSMs.
- `swapClientAdapter` — Production implementation of `swapRuntimeClient`
  wrapping `sdk/swaps.SwapClient`.
- `paySwapSession` / `receiveSwapSession` — Interfaces driving background
  workers for outgoing pay and incoming receive swaps respectively.

## Relationships

- **Depends on**: `darepod` (config, logger), `sdk/swaps` (swap FSM
  operations and store), `sdk/ark` (type aliases), `rpc/swapclientrpc`
  (generated gRPC stubs).
- **Depended on by**: `cmd/darepod` (registers as gRPC subserver when
  built with the `swapruntime` tag).
- **Sends**:
  - → streaming `SubscribeSwaps` subscribers (Tell): `SwapSummary` updates
    on worker exit.
- **Receives**:
  - ← API: `PaySwap`, `ReceiveSwap`, `ListSwaps`, `GetSwap`,
    `SubscribeSwaps`.

## Invariants

- Built only when the `swapruntime` build tag is set; a stub in
  `cmd/darepod/swapruntime_stub.go` provides the no-op path for default
  builds.
- Workers use `rootCtx` (not individual RPC contexts) so a CLI disconnect
  does not cancel a swap already admitted for background execution.
- Swap FSM transitions, mailbox HTLC receive-event handling, and swap server
  protocol behavior remain delegated to `sdk/swaps`; this package is a
  control-plane adapter only.
- One background worker per payment hash; concurrent start/resume calls for
  the same hash are deduplicated via the `active` map guarded by `mu`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
