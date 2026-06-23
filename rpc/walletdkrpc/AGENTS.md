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
| `Create` | Initialize a new wallet (proxies `GenSeed` + `InitWallet`) |
| `Unlock` | Unlock an existing wallet (proxies `UnlockWallet`) |
| `PrepareSend` | Validate + quote an outbound payment; returns a single-use `send_intent_id` |
| `Send` | Dispatch a prepared send; consumes `send_intent_id` |
| `Recv` | Inbound Lightning invoice (offchain receive) |
| `List` | Unified wallet view; `ListView` selects activity/vtxos/onchain |
| `Balance` | Flat balance (confirmed / pending_in / pending_out) |
| `Exit` | Cooperative leave by default; unilateral unroll only with the exact force-ack |
| `Deposit` | Fresh boarding address (used by `recv --onchain`) |
| `Status` | Daemon + wallet readiness summary |
| `ExitStatus` | Phase of an exit job (proxies `GetUnrollStatus`) |
| `SubscribeWallet` | Streams normalized `WalletEntry` updates |

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
- `ExitJobStatus` — Wallet-facing projection of
  `daemonrpc.UnrollJobStatus`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**:
  - `swapwallet` (implements the service server-side; consumes the
    generated stubs).
  - `cmd/darepocli/darepoclicommands` (the seven top-level CLI verbs
    dial `walletdkrpc.WalletService`).
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same stubs).

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

## Deep Docs

- [docs/walletdkrpc_build.md](../../docs/walletdkrpc_build.md) — Build
  modes, make targets, what the walletdkrpc tag enables.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  implementation.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — Embedded SDK
  facade.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
