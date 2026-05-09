# sdk/swaps

## Purpose

High-level client SDK for Lightning-to-Ark (receive) and Ark-to-Lightning
(pay) atomic swaps via virtual HTLCs (vHTLCs). Orchestrates two durable
FSM-driven flows using the Loop FSM engine, coordinating with a remote swap
server and the local Ark daemon to fund, claim, or refund on-chain vHTLCs.
Persists every state transition in an isolated SQLite database.

## Key Types

- `SwapClient` — Top-level entry point. Constructed via `NewSwapClient`
  (no persistence) or `NewSwapClientWithStore` (SQLite-backed). Drives
  both pay and receive flows.
- `PaySession` — Owns one Ark-to-Lightning pay flow. FSM states:
  `Created → SwapCreated → FundingInitiated → VHTLCFunded →
  WaitingForClaim → Completed` (or `Expired / RefundInitiated →
  Refunded / NeedsIntervention / Failed`).
- `ReceiveSession` — Owns one Lightning-to-Ark receive flow. FSM states:
  `Created → InvoiceCreated → VHTLCFunded → ClaimInitiated → Completed`
  (or `Expired / NeedsIntervention / Failed`).
- `Store` — Isolated SQLite persistence for swap sessions. Runs its own
  migration table (`swap_client_schema_migrations`) separate from the
  main daemon DB.
- `SwapServerConn` / `GRPCSwapServerConn` — Interface/impl for remote
  swap-server gRPC calls (`RequestChannelID`, `CreateInSwap`).
- `DaemonConn` — Interface for wallet operations (OOR sends, VTXO
  lookups, key queries) provided by the Ark daemon.
- `InvoiceCreator` / `DirectInvoiceCreator` — BOLT-11 invoice building
  for the receive flow.
- `PayState` / `ReceiveState` — Typed FSM state enums with `IsTerminal()`
  and `String()`.
- `VHTLCConfig`, `InSwapConfig`, `RouteHint` — Server negotiation DTOs.
- `SwapSummary` — Flat list view for persisted sessions.
- `ReceiveAuthKey` — Interface (`keychain.SingleKeyMessageSigner` +
  `sphinx.SingleKeyECDH`) used by the receive flow to sign invoices and
  decrypt the forwarded final-hop onion. Backed in production by
  `daemonReceiveAuthKey`, which delegates to `DaemonConn`.
- `MailboxOutSwapEventReceiver` — Mailbox-backed HTLC-event receiver for
  receive sessions. Derives a per-receive mailbox from the client identity
  key and payment hash; pulls events with a configurable wait timeout via
  `WaitOutSwapHtlc(ctx, hash, pubkey)`.
- `OutSwapMailboxID(pubkey, hash)` — Constructs the stable mailbox ID for a
  receive session from a client identity key and payment hash.

## Relationships

- **Depends on**: `lib/arkscript` (vHTLC policy construction, claim/refund
  tapscript paths), `sdk/ark` (type aliases: `CustomOORInput`, `VTXOInfo`,
  `IndexedOORSessionInfo`, `ReceiveInfo`), `swaprpc` (generated gRPC stubs),
  `db/migrate` + `db/sqlc` (migration infrastructure), `sdk/swaps/sqlc`
  (internal generated query adapter), `github.com/lightninglabs/loop/fsm`
  (FSM engine).
- **Depended on by**: `swapclientserver` (daemon RPC subserver wrapping
  FSM operations), `cmd/darepocli/darepoclicommands` (CLI swap commands).

## Sends / Receives

Both FSMs use `loopfsm.StateMachine.SendEvent(ctx, OnAdvance, nil)` per
tick. The pay flow calls `DaemonConn.SendOORWithPolicy` to fund the vHTLC
and `DaemonConn.SendOORWithCustomInputs` to refund. The receive flow calls
`DaemonConn.SendOORWithCustomInputs` to claim the funded vHTLC using the
preimage spend path.

## Invariants

- `mutateAndPersist` is the only way to change session state; it snapshots
  before mutation and rolls back on store failure. Never write
  `s.state = ...` directly outside this wrapper.
- OOR session IDs must be persisted before transitioning; failure wraps in
  `newRetryableActionError` so the FSM retries rather than advancing past
  a durable boundary.
- The store is optional: `NewSwapClient` (no store) and
  `NewSwapClientWithStore` are both valid; all `persist()` calls are
  no-ops when `store == nil`.
- Amount mismatch on a live vHTLC triggers `RefundInitiated` (pay) or
  `Failed` (receive) immediately — never `NeedsIntervention`.
- `NeedsIntervention` is reserved for anomalous server behavior (e.g.,
  vHTLC spent without a matching preimage).
- `PaySession` / `ReceiveSession` are not goroutine-safe; `Wait`, `Claim`,
  `WaitForFunding`, and `State` must not be called concurrently.
- Preimage extraction uses a multi-strategy scan of finalized checkpoint
  PSBTs (final witness, condition witness, taproot spend sig) to tolerate
  different Ark indexer versions. Only accepted when
  `SHA256(preimage) == paymentHash`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
