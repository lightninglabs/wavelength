# rpc/walletrpc

## Purpose

Generated gRPC stubs for `walletrpc.WalletService` — the highest-level
RPC surface in the daemon's API stack. The service is a small, flat,
swap-vocabulary-free wallet API that lives ABOVE `daemonrpc` and
`swapclientrpc` and composes them; the seven verbs map 1:1 to what a
user does day-to-day.

Proto source: `rpc/walletrpc/wallet.proto`.

## Services

### WalletService (7 wallet verbs + supporting)

| Method | Purpose |
|--------|---------|
| `Create` | Initialize a new wallet (proxies `GenSeed` + `InitWallet`) |
| `Unlock` | Unlock an existing wallet (proxies `UnlockWallet`) |
| `Send` | Outbound payment; destination oneof picks invoice vs onchain |
| `Recv` | Inbound Lightning invoice (offchain receive) |
| `List` | Unified wallet view; `ListView` selects activity/vtxos/onchain |
| `Balance` | Flat balance (confirmed / pending_in / pending_out) |
| `Exit` | Unilateral exit / unroll (proxies `Unroll`) |
| `Deposit` | Fresh boarding address (used by `recv --onchain`) |
| `Status` | Daemon + wallet readiness summary |
| `ExitStatus` | Phase of an exit job (proxies `GetUnrollStatus`) |
| `SubscribeWallet` | Streams normalized `WalletEntry` updates |

### WalletInspectionService (technical drill-down)

| Method | Purpose |
|--------|---------|
| `InspectActivity` | Returns full correlated trace for one activity entry |

`InspectActivity` accepts an activity `id` and optional ledger row limit,
and returns: the friendly `WalletEntry`, a correlated `ActivitySwapTrace`
snapshot, a list of `ActivityLedgerTrace` rows with internal accounting
details, a list of `ActivityVTXOTrace` rows for VTXO movements, and
plain-text caveat notes. Implemented in `swapwallet.InspectionService`.

## Key Messages

- `WalletEntry` — Flat activity row. Every internal correlator
  (session_id, round_id, settlement_type, mailbox subtype) is dropped
  before responding. `id` is the stable canonical id (Lightning
  payment_hash for SEND-invoice / RECV). Now includes optional `request`
  (`WalletEntryRequest` oneof) and `progress` (`WalletEntryProgress`).
- `WalletEntryRequest` — Oneof: `LightningInvoiceRequest` (invoice +
  payment_hash), `OnchainAddressRequest` (destination address), or
  `ArkAddressRequest`. Exposes the payment request that originated the entry.
- `WalletEntryProgress` — Phase enum plus metadata: `phase`
  (`WalletEntryPhase`), `phase_label`, `payment_hash`, `txid`,
  `confirmation_height`, `vtxo_outpoint`.
- `WalletEntryPhase` — 10-state lifecycle enum: `REQUEST_CREATED`,
  `WAITING_FOR_PAYMENT`, `PAYMENT_DETECTED`, `SETTLING`, `CONFIRMED`,
  `REFUNDING`, `REFUNDED`, `FAILED`, `WAITING_FOR_CONFIRMATION`.
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
- `ActivitySwapTrace` — Full swap FSM snapshot for inspection: state,
  direction, pending flag, amounts/fees, session IDs (funding/claim/
  refund), vHTLC details, terminal reason, deadlines.
- `ActivityLedgerTrace` — Internal ledger row with `role` field
  (activity_row, spent_input, change_output, materialized_output,
  vhtlc_tx, swap_session) and `hidden_from_activity` flag.
- `ActivityVTXOTrace` — VTXO movement row with outpoint, amount, role,
  ownership flag, source (ledger/swap), session ID, output index.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**:
  - `swapwallet` (implements the service server-side; consumes the
    generated stubs).
  - `cmd/darepocli/darepoclicommands` (the seven top-level CLI verbs
    dial `walletrpc.WalletService`).
  - `sdk/walletdk` (gomobile-friendly SDK wraps the same stubs).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The walletrpc layer is the highest-level RPC surface; new wallet
  verbs land HERE first and admin proxies pull from `daemonrpc`.
  Internal correlators MUST NOT leak from `daemonrpc` into walletrpc
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
- `WalletInspectionService` is a separate service from `WalletService`;
  clients must dial it independently. Its RPCs expose internal
  correlators (session IDs, ledger row roles) that are intentionally
  hidden from the user-facing `WalletService.List` response.
- `ActivityLedgerTrace.hidden_from_activity` marks rows suppressed from
  the friendly activity feed (internal OOR legs). Inspection clients
  MUST NOT assume these rows indicate errors.

## Deep Docs

- [docs/walletrpc_build.md](../../docs/walletrpc_build.md) — Build
  modes, make targets, what the walletrpc tag enables.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side
  implementation.
- [sdk/walletdk/CLAUDE.md](../../sdk/walletdk/CLAUDE.md) — Embedded SDK
  facade.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
