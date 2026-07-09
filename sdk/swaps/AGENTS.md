# sdk/swaps

## Purpose

High-level client SDK for Lightning-to-Ark (receive) and Ark-to-Lightning
(pay) atomic swaps via virtual HTLCs (vHTLCs). Orchestrates two durable
FSM-driven flows using the Loop FSM engine, coordinating with a remote
swap server and the local Ark daemon to fund, claim, or refund on-chain
vHTLCs. Persists every state transition in an isolated SQLite database.
Also handles same-Ark (in-Ark) vHTLC settlement where sender and receiver
settle a vHTLC inside the same Ark instance without bridging through
Lightning.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/sdk/swaps.<Symbol>`.

- `SwapClient` — top-level entry point. Constructed via `NewSwapClient`
  (no persistence) or `NewSwapClientWithStore` (SQLite-backed). Holds
  an `OutSwapEventReceiver` overridable via
  `SetOutSwapEventReceiver`.
- `PaySession` — Ark-to-Lightning pay FSM:
  `Created → SwapCreated → FundingInitiated → VHTLCFunded →
  WaitingForClaim → Completed` (or `Expired` / `RefundInitiated →
  Refunded` / `NeedsIntervention` / `Failed`).
- `ReceiveSession` — Lightning-to-Ark receive FSM:
  `Created → InvoiceCreated → HTLCEventAccepted → VHTLCFunded →
  ClaimInitiated → Completed` (or `Expired` / `NeedsIntervention` /
  `Failed`). `HTLCEventAccepted` is a durable checkpoint persisted
  after the server mailbox event is validated so funding detection
  resumes without re-driving mailbox delivery.
- `MailboxOutSwapEventReceiver` — mailbox-backed receiver. Pulls
  out-swap HTLC events from a `mailbox/pb` edge keyed by a per-session
  mailbox ID derived from the client identity key and payment hash.
  Implements both `OutSwapEventReceiver` and
  `IncomingVHTLCEventReceiver`. Drives `AckUpTo` only after the
  caller durably accepts the event.
- `ReceiveAuthKey` — interface combining
  `keychain.SingleKeyMessageSigner` and `sphinx.SingleKeyECDH`. Used
  to sign receive invoices and decode the forwarded final-hop onion.
  Backed by `daemonReceiveAuthKey`, which delegates to `DaemonConn`
  RPCs rather than holding a raw private key in the SDK.
- `IncomingVHTLCNotification` — unified type carrying either a
  Lightning-backed `OutSwapHtlcEvent` or a same-Ark `InArkHtlcEvent`,
  plus `AckCursor` and an `Ack` hook.
- `InArkHtlcEvent` — same-Ark vHTLC event (payment hash, amount,
  sender pubkey, vHTLC config, optional indexed outpoint/amount).
- `OutSwapHtlcNotification` — wraps a mailbox `OutSwapHtlcEvent`
  with `AckCursor` and `Ack`.
- `IncomingVHTLCEventReceiver` — interface for receivers that
  handle both Lightning-backed and same-Ark vHTLC events; implemented
  by `MailboxOutSwapEventReceiver`.
- `SettlementType` — `SettlementTypeLightning`, `SettlementTypeInArk`
  (returned in `InSwapConfig` identifying how the server bridges
  payment).
- `Store` — isolated SQLite persistence. Runs its own migration table
  (`swap_client_schema_migrations`) separate from the main daemon DB.
- `SwapServerConn` / `GRPCSwapServerConn` — remote swap-server gRPC
  (`RequestChannelID`, `CreateInSwap`).
- `DaemonConn` — wallet operations (OOR sends, VTXO lookups, key
  queries, receive-auth signing/ECDH) provided by the Ark daemon.
  Includes `ReceiveAuthKey`, `SignReceiveAuthMessage[Compact]`, and
  `ReceiveAuthECDH` for payment-scoped auth.
- `InvoiceCreator` — BOLT-11 invoice building; `CreateInvoiceWithKey`
  for invoices signed with a `ReceiveAuthKey`.
- `PayState` / `ReceiveState` — typed FSM enums with `IsTerminal()` /
  `String()`. `ReceiveState` includes `ReceiveStateHTLCEventAccepted`.
- `VHTLCConfig`, `InSwapConfig`, `RouteHint` — server-negotiation
  DTOs. `SwapSummary` — flat list view for persisted sessions.
- `OutSwapMailboxID` — derives a per-receive mailbox ID from the
  client identity key and invoice payment hash.
- Error sentinels (exported): `ErrSwapExpired`, `ErrSwapRefunded`,
  `ErrSwapSummaryNotFound`. Internal classifiers
  (`interventionError`, `failureError`, `retryableActionError`) live
  in `errors.go`.

## Relationships

- **Depends on**: `lib/arkscript` (vHTLC policy + claim/refund
  tapscript paths), `sdk/ark` (type aliases), `swaprpc` (gRPC stubs),
  `mailbox/pb` (edge pull/ack), `serverconn` (`CompoundMailboxID`,
  `PubKeyMailboxID`), `db/migrate` + `db/sqlc`, `sdk/swaps/sqlc`,
  `loop/fsm` (FSM engine), `lightning-onion` (Sphinx ECDH).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (`pay` /
  `receive` commands).

## Sends / Receives

Both FSMs tick via `loopfsm.StateMachine.SendEvent(ctx, OnAdvance,
nil)`. Pay calls `DaemonConn.SendOORWithPolicy` to fund and
`SendOORWithCustomInputs` to refund. Receive calls
`SendOORWithCustomInputs` to claim via the preimage spend path.

Receive waits for either a Lightning-backed out-swap event or a
same-Ark vHTLC event: if `outEvents` implements
`IncomingVHTLCEventReceiver`, `WaitIncomingVHTLC` is called;
otherwise the flow falls back to `WaitOutSwapHtlc` and converts the
result into an `IncomingVHTLCNotification`.

## Invariants

- `mutateAndPersist` is the only way to change session state — it
  snapshots before mutation and rolls back on store failure. **Never
  write `s.state = …` directly outside this wrapper.**
- OOR session IDs must be persisted before transitioning; failure
  wraps in `newRetryableActionError` so the FSM retries rather than
  advancing past a durable boundary.
- The store is optional — both `NewSwapClient` and
  `NewSwapClientWithStore` are valid; `persist()` is a no-op when
  `store == nil`.
- Amount mismatch on a live vHTLC triggers `RefundInitiated` (pay) or
  `Failed` (receive) **immediately** — never `NeedsIntervention`.
- `NeedsIntervention` is reserved for anomalous server behavior
  (e.g., vHTLC spent without a matching preimage).
- `PaySession` / `ReceiveSession` are NOT goroutine-safe; `Wait`,
  `Claim`, `WaitForFunding`, and `State` must not be called
  concurrently.
- Preimage extraction uses a multi-strategy scan of finalized
  checkpoint PSBTs (final witness, condition witness, taproot spend
  sig) to tolerate indexer-version differences. Only accepted when
  `SHA256(preimage) == paymentHash`.
- Mailbox `Ack` must be called only after the caller has validated
  and durably persisted the event. `AckCursor` is `eventSeq + 1`.
- `ReceiveAuthKey` signing/ECDH is always delegated to the daemon;
  the SDK never holds the raw private key for receive-auth.
- Error sentinels (`ErrSwapExpired`, `ErrSwapRefunded`,
  `ErrSwapSummaryNotFound`) are exported; callers use `errors.Is`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
