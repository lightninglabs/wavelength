# sdk/swaps

## Purpose

High-level client SDK for Lightning-to-Ark (receive) and Ark-to-Lightning
(pay) atomic swaps via virtual HTLCs (vHTLCs). Orchestrates two durable
FSM-driven flows using the Loop FSM engine, coordinating with a remote swap
server and the local Ark daemon to fund, claim, or refund on-chain vHTLCs.
Persists every state transition in an isolated SQLite database. Also handles
same-Ark (in-Ark) vHTLC settlement, where sender and receiver settle a vHTLC
inside the same Ark instance without bridging through Lightning.

## Key Types

- `SwapClient` — Top-level entry point. Constructed via `NewSwapClient`
  (no persistence) or `NewSwapClientWithStore` (SQLite-backed). Drives
  both pay and receive flows. Holds an `OutSwapEventReceiver` that can be
  overridden via `SetOutSwapEventReceiver`.
- `PaySession` — Owns one Ark-to-Lightning pay flow. FSM states:
  `Created → SwapCreated → FundingInitiated → VHTLCFunded →
  WaitingForClaim → Completed` (or `Expired / RefundInitiated →
  Refunded / NeedsIntervention / Failed`).
- `ReceiveSession` — Owns one Lightning-to-Ark receive flow. FSM states:
  `Created → InvoiceCreated → HTLCEventAccepted → VHTLCFunded →
  ClaimInitiated → Completed` (or `Expired / NeedsIntervention / Failed`).
  The `HTLCEventAccepted` state is a new durable checkpoint persisted
  after the server mailbox event is validated, so funding detection can
  resume without re-driving mailbox delivery.
- `MailboxOutSwapEventReceiver` — Mailbox-backed event receiver (new).
  Pulls out-swap HTLC events from a `mailbox/pb` edge keyed by a
  per-session mailbox ID derived from the client identity key and payment
  hash. Implements both `OutSwapEventReceiver` and
  `IncomingVHTLCEventReceiver`. Drives acknowledgement via `AckUpTo`
  only after the caller durably accepts the event.
- `ReceiveAuthKey` — Interface (new) combining
  `keychain.SingleKeyMessageSigner` and `sphinx.SingleKeyECDH`. Used to
  sign receive invoices and decode the forwarded final-hop onion.
  Backed by `daemonReceiveAuthKey`, which delegates signing/ECDH to the
  `DaemonConn` RPCs rather than holding a raw private key in the SDK.
- `IncomingVHTLCNotification` — Unified notification type (new) carrying
  either a Lightning-backed `OutSwapHtlcEvent` or a same-Ark
  `InArkHtlcEvent`, along with an `AckCursor` and `Ack` hook.
- `InArkHtlcEvent` — Same-Ark vHTLC event (new): payment hash, amount,
  sender pubkey, vHTLC config, and optional indexed outpoint/amount from
  the server.
- `OutSwapHtlcNotification` — Wraps one mailbox `OutSwapHtlcEvent` with
  an `AckCursor` and `Ack` hook.
- `IncomingVHTLCEventReceiver` — Interface (new) for receivers that can
  handle both Lightning-backed and same-Ark vHTLC events. Implemented by
  `MailboxOutSwapEventReceiver`.
- `SettlementType` — Enum (`SettlementTypeLightning`, `SettlementTypeInArk`)
  returned in `InSwapConfig` identifying how the swap server bridges payment.
- `Store` — Isolated SQLite persistence for swap sessions. Runs its own
  migration table (`swap_client_schema_migrations`) separate from the
  main daemon DB.
- `SwapServerConn` / `GRPCSwapServerConn` — Interface/impl for remote
  swap-server gRPC calls (`RequestChannelID`, `CreateInSwap`).
- `DaemonConn` — Interface for wallet operations (OOR sends, VTXO
  lookups, key queries, receive-auth signing/ECDH) provided by the Ark
  daemon. Now includes `ReceiveAuthKey`, `SignReceiveAuthMessage`,
  `SignReceiveAuthMessageCompact`, and `ReceiveAuthECDH` for payment-scoped
  auth key operations.
- `InvoiceCreator` — BOLT-11 invoice building; includes `CreateInvoiceWithKey`
  for building invoices signed with a `ReceiveAuthKey`.
- `PayState` / `ReceiveState` — Typed FSM state enums with `IsTerminal()`
  and `String()`. `ReceiveState` gains `ReceiveStateHTLCEventAccepted`.
- `VHTLCConfig`, `InSwapConfig`, `RouteHint` — Server negotiation DTOs.
- `SwapSummary` — Flat list view for persisted sessions.
- Error sentinels: `ErrSwapExpired`, `ErrSwapRefunded`,
  `ErrSwapSummaryNotFound` (all now exported). Internal error classifiers
  `interventionError`, `failureError`, `retryableActionError` and their
  constructors consolidated in `errors.go`.
- `OutSwapMailboxID` — Derives a per-receive mailbox ID from the client's
  identity key and the invoice payment hash.

## Relationships

- **Depends on**: `lib/arkscript` (vHTLC policy construction, claim/refund
  tapscript paths), `sdk/ark` (type aliases: `CustomOORInput`, `VTXOInfo`,
  `IndexedOORSessionInfo`, `ReceiveInfo`), `swaprpc` (generated gRPC stubs),
  `mailbox/pb` (mailbox edge pull/ack), `serverconn`
  (`CompoundMailboxID`, `PubKeyMailboxID`), `db/migrate` + `db/sqlc`
  (migration infrastructure), `sdk/swaps/sqlc` (internal generated query
  adapter), `github.com/lightninglabs/loop/fsm` (FSM engine),
  `github.com/lightningnetwork/lightning-onion` (Sphinx ECDH for onion
  decoding).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (CLI `pay` and
  `receive` commands).

## Sends / Receives

Both FSMs use `loopfsm.StateMachine.SendEvent(ctx, OnAdvance, nil)` per
tick. The pay flow calls `DaemonConn.SendOORWithPolicy` to fund the vHTLC
and `DaemonConn.SendOORWithCustomInputs` to refund. The receive flow calls
`DaemonConn.SendOORWithCustomInputs` to claim the funded vHTLC using the
preimage spend path.

The receive flow can wait for either a Lightning-backed out-swap event or a
same-Ark vHTLC event from the mailbox: if `outEvents` implements
`IncomingVHTLCEventReceiver`, `WaitIncomingVHTLC` is called; otherwise the
flow falls back to `WaitOutSwapHtlc` and converts the result into an
`IncomingVHTLCNotification`.

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
- Mailbox acknowledgement (`Ack`) must be called only after the caller has
  validated and durably persisted the event. `AckCursor` is `eventSeq + 1`.
- `ReceiveAuthKey` signing/ECDH is always delegated to the daemon; the SDK
  never holds the raw private key for receive-auth.
- `ErrSwapExpired`, `ErrSwapRefunded`, and `ErrSwapSummaryNotFound` are
  exported sentinels; callers use `errors.Is` for matching.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
