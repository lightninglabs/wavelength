# rpc/walletdkrpc

## Purpose

Generated gRPC stubs for `walletdkrpc.WalletService` (plus the technical
drill-down `WalletInspectionService`) — the highest-level RPC surface in
the daemon's API stack. The service is a small, flat, swap-vocabulary-free
wallet API that lives ABOVE `waverpc` and `swapclientrpc` and composes
them; seven verbs map 1:1 to what a user does day-to-day, plus supporting
methods. `failure_reasons.go` is hand-written: it defines the wallet
rejection taxonomy shared by the daemon-side error mapper and SDK clients.

Proto source: `rpc/walletdkrpc/wallet.proto`.

## Service Methods (the 7 wallet verbs + supporting)

| Method | Purpose |
|--------|---------|
| `Create` | Initialize a new wallet (proxies `GenSeed` + `InitWallet`) |
| `Unlock` | Unlock an existing wallet (proxies `UnlockWallet`) |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `send_intent_id` |
| `Send` | Dispatch a prepared send; consumes `send_intent_id` |
| `Recv` | Inbound Lightning invoice (offchain receive) |
| `List` | Unified wallet view; `ListView` selects activity/vtxos/onchain |
| `Deposit` | Fresh boarding address (used by `recv --onchain`) |
| `Balance` | Flat balance (confirmed / pending_in / pending_out) |
| `Status` | Daemon + wallet readiness summary |
| `GetExitPlan` | Preview unilateral-exit readiness/funding for one VTXO |
| `SweepWallet` | Preview/broadcast a backing-wallet sweep to an address |
| `Exit` | Cooperative leave by default; unilateral unroll only with the exact force-ack |
| `ExitStatus` | Phase of an exit job (proxies `GetUnrollStatus`) |
| `ExitSummary` | Wallet-wide portfolio of in-progress exits |
| `SubscribeWallet` | Streams normalized `WalletEntry` updates (resumable via cursor) |

`WalletInspectionService.InspectActivity` is a separate service exposing a
technical trace (ledger rows, swap/VTXO correlation) for one `WalletEntry`
id; unlike `List` it may leak internal correlators, so it is kept out of
`WalletService`.

## Key Messages

- `WalletEntry` — Flat activity row. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding. `id` is the stable canonical id (Lightning
  payment_hash for SEND-invoice / RECV).
- `EntryKind` — User-visible category: SEND, RECV, DEPOSIT, EXIT.
- `EntryStatus` — Collapsed FSM: PENDING, COMPLETE, FAILED.
- `ListView` — Selects which slice of state to return: ACTIVITY
  (default), VTXOS, ONCHAIN.
- `ListResponse.body` — Oneof discriminating the typed response per
  view: `ActivityList`, `VTXOInventory`, `OnchainHistory`.
- `WalletVTXO` — Wallet-facing flat VTXO shape (no chain depth, no
  forfeiting lifecycle detail).
- `OnchainTx` — Wallet-facing flat on-chain row (no debit/credit
  accounts, no round/session correlators).
- `ExitJobStatus` — Enum collapsing the underlying unroll job phases to a
  short wallet-facing status; shared by `ExitPlanEntry`,
  `ExitStatusResponse`, and `ExitSummaryItem`.
- `FailureDomain` / `Reason*` constants (`failure_reasons.go`) — the
  `google.rpc.ErrorInfo` domain/reason wire contract for failed wallet
  RPCs; existing reason values MUST NOT be renamed.

## Relationships

- **Depends on**: nothing (proto definitions; `failure_reasons.go` has no
  imports).
- **Depended on by**:
  - `swapwallet` (implements the service server-side; consumes the
    generated stubs).
  - `cmd/wavecli/waveclicommands` (the seven top-level CLI verbs
    plus `sweep-wallet`, `exit plan`/`exit summary`, and `activity
    inspect` dial `WalletService`/`WalletInspectionService`).
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same stubs).
  - `waved` (RPC auth wiring), `rpc/restclient` (REST transport
    adapter over the same service stubs).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The walletdkrpc layer is the highest-level RPC surface; new wallet
  verbs land HERE first and admin proxies pull from `waverpc`.
  Internal correlators MUST NOT leak from `waverpc` into walletdkrpc
  responses (that is what `WalletInspectionService` is for instead).
- `Create`, `Unlock`, `Exit`, and `ExitStatus` are admin-shape proxies
  that work BEFORE the swap subsystem is live; the server-side
  implementation (in `swapwallet/admin.go`) MUST NOT depend on the
  swap runtime being started.
- `ListView` defaults (UNSPECIFIED) to ACTIVITY so callers that omit
  the field keep getting the merged WalletEntry stream.
- `ListResponse.body` is a oneof; agents see a tagged union per view
  rather than a polymorphic blob.
- `failure_reasons.go` values are a wire contract: existing `Reason*`
  constants MUST NOT be renamed since clients match on them; add new
  values instead.

## Deep Docs

- [docs/walletdkrpc_build.md](../../docs/walletdkrpc_build.md) — Build
  modes, make targets, what the walletdkrpc tag enables.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  implementation.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — Embedded SDK
  facade.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
