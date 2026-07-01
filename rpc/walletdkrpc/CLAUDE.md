# rpc/walletdkrpc

## Purpose

Generated gRPC stubs for `walletdkrpc.WalletService` — the highest-level
RPC surface in the daemon's API stack. The service is a small, flat,
swap-vocabulary-free wallet API that lives ABOVE `daemonrpc` and
`swapclientrpc` and composes them; the seven verbs map 1:1 to what a
user does day-to-day.

Proto source: `rpc/walletdkrpc/wallet.proto`.

## Service Methods (the 7 wallet verbs + supporting)

| Method | Purpose |
|--------|---------|
| `Create` | Initialize a new wallet (proxies `GenSeed` + `InitWallet`); supports `recover_state`/`recovery_window` to rebuild Ark state from chain/indexer data after mnemonic import |
| `Unlock` | Unlock an existing wallet (proxies `UnlockWallet`) |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `send_intent_id`; response may carry a `CreditPreview` when server credits apply |
| `Send` | Dispatch a prepared send; consumes `send_intent_id` |
| `Recv` | Inbound Lightning invoice (offchain receive); response may carry a `CreditReceive` when backed by server credits instead of a client-claimable vHTLC |
| `List` | Unified wallet view; `ListView` selects activity/vtxos/onchain |
| `Balance` | Flat balance (confirmed / pending_in / pending_out / credit_available / credit_reserved) |
| `Exit` | Cooperative leave by default; unilateral unroll only with the exact force-ack |
| `GetExitPlan` | Previews unilateral-exit readiness per VTXO: CPFP fee-input requirements and a funding address when a shortfall exists |
| `SweepWallet` | Previews or broadcasts a backing-wallet sweep (funds left after CPFP/change/unroll) to a caller-supplied address; never sweeps boarding outputs |
| `Deposit` | Fresh boarding address (used by `recv --onchain`) |
| `Status` | Daemon + wallet readiness summary |
| `ExitStatus` | Phase of an exit job (proxies `GetUnrollStatus`) |
| `SubscribeWallet` | Streams normalized `WalletEntry` updates |

## Key Messages

- `WalletEntry` — Flat activity row. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding. `id` is the stable canonical id (Lightning
  payment_hash for SEND-invoice / RECV). `failure_code`
  (`EntryFailureCode`, optional/presence-tracked) is a stable
  machine-readable classification set ONLY on FAILED entries — absence is
  the canonical "no failure" signal; `failure_reason` remains the
  human-readable supplement.
- `EntryKind` — User-visible category: SEND, RECV, DEPOSIT, EXIT.
- `EntryStatus` — Collapsed FSM: PENDING, COMPLETE, FAILED.
- `SendRail` — now includes `SEND_RAIL_CREDIT` and `SEND_RAIL_MIXED`
  alongside the original IN_ARK/LIGHTNING/ONCHAIN rails, reflecting sends
  that use or blend sat-native server credit.
- `ListView` — Selects which slice of state to return: ACTIVITY
  (default), VTXOS, ONCHAIN.
- `ListResponse.body` — Oneof discriminating the typed response per
  view: `ActivityList`, `VTXOInventory`, `OnchainHistory`.
- `WalletVTXO` — Wallet-facing flat VTXO shape (no chain depth, no
  forfeiting lifecycle detail).
- `OnchainTx` — Wallet-facing flat on-chain row (no debit/credit
  accounts, no round/session correlators).
- `ExitJobStatus` — Wallet-facing projection of
  `daemonrpc.UnrollJobStatus`.
- `ExitPlanEntry` / `GetExitPlanResponse` — per-outpoint exit feasibility
  (`can_start`, `funding_address`, `funding_shortfall_sat`, fee-UTXO
  counts) evaluated against a running wallet allocation so a batch verdict
  reflects simultaneous funding, not one-at-a-time.
- `SweepWalletRequest`/`SweepWalletResponse` — sweep preview/broadcast
  with `WalletSweepInput` line items, estimated fee, and `can_broadcast`.
- `CreditPreview` / `CreditReceive` — sat-native server credit usage
  surfaced from `PrepareSend`/`Recv` (`must_use_credit`,
  `credit_applied_sat`, `credit_shortfall_sat`, `credit_topup_sat`,
  `ark_funding_sat`; `operation_id`/`amount_sat`/`payment_hash` for
  receive).
- `EntryFailureCode` — stable failure taxonomy for `WalletEntry`
  (`TIMED_OUT`, `EXPIRED`, `REFUNDED`, `NEEDS_INTERVENTION`, `FAILED`).

`failure_reasons.go` (hand-written, not generated) defines the parallel
wallet-rejection-reason wire contract (`FailureDomain = "walletdk"`,
`Reason*` constants) carried in a failed RPC's `google.rpc.ErrorInfo`
detail; the daemon-side mapper and SDK reconstructor both import these
constants so they cannot drift. Existing `Reason*` values must not be
renamed; add new ones as needed.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**:
  - `swapwallet` (implements the service server-side; consumes the
    generated stubs).
  - `cmd/darepocli/darepoclicommands` (the seven top-level CLI verbs
    dial `walletdkrpc.WalletService`).
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same stubs).
  - `swapwallet/credit_projector.go` (projects server credit state into
    `CreditPreview`/`CreditReceive`/`BalanceResponse.credit_*`).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The walletdkrpc layer is the highest-level RPC surface; new wallet
  verbs land HERE first and admin proxies pull from `daemonrpc`.
  Internal correlators MUST NOT leak from `daemonrpc` into walletdkrpc
  responses.
- `Create`, `Unlock`, `Exit`, and `ExitStatus` are admin-shape proxies
  that work BEFORE the swap subsystem is live; the server-side
  implementation (in `swapwallet/admin.go`) MUST NOT depend on the
  swap runtime being started.
- `ListView` defaults (UNSPECIFIED) to ACTIVITY so callers that omit
  the field keep getting the merged WalletEntry stream.
- `ListResponse.body` is a oneof; agents see a tagged union per view
  rather than a polymorphic blob.
- `Status` and `Deposit` are kept in the proto for programmatic
  callers (and `recv --onchain` plumbs through `Deposit`); they are
  NOT surfaced as top-level CLI verbs.
- `WalletEntry.failure_code` is presence-tracked (`optional`):
  `ENTRY_FAILURE_CODE_UNSPECIFIED` must never be sent on the wire; a
  non-failed entry omits the field entirely.
- `failure_reasons.go` `Reason*` string constants are a wire contract —
  never rename an existing value; both daemon and SDK sides match on the
  literal string.

## Deep Docs

- [docs/walletdkrpc_build.md](../../docs/walletdkrpc_build.md) — Build
  modes, make targets, what the walletdkrpc tag enables.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  implementation.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — Embedded SDK
  facade.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
