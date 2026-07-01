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
  Implements `OutSwapEventReceiver`, `IncomingVHTLCEventReceiver`, and
  `OutSwapForfeitSignatureReceiver` (`WaitOutSwapForfeitSignature`).
  Drives `AckUpTo` only after the caller durably accepts the event.
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
- `Store` — isolated SQLite persistence via the shared
  `db.OpenSQLiteDatabase` helper (pragma/DSN plumbing lives in
  `db`, not here). Runs its own migration table
  (`swap_client_schema_migrations`) separate from the main daemon DB.
  `LatestMigrationVersion` is `3` (adds credit/requested-amount
  columns to `receive_swap` and a `preimage` column to `pay_swap`).
- `SwapServerConn` / `GRPCSwapServerConn` — remote swap-server gRPC.
  Covers route negotiation (`RequestChannelID`, `AcknowledgeOutSwapHTLC`),
  in-swap creation/quoting (`CreateInSwap`, `QuoteInSwap`, and the
  optional credit-aware `CreateInSwapWithCredits` /
  `QuoteInSwapWithCredits` widened interfaces), cooperative refund
  (`AuthorizeInSwapRefund`), and vHTLC-refresh forfeit signing
  (`SignInSwapForfeit`, `SubmitOutSwapForfeitSignature`). The credit
  surface (`CreateCredit`, `RedeemCredit`, `ListCredits`) lives on the
  separate `creditServerConn` interface in `credits.go`, which
  `GRPCSwapServerConn` also satisfies.
- `DaemonConn` — wallet operations (OOR sends, VTXO lookups, key
  queries, receive-auth signing/ECDH, forfeit signing) provided by the
  Ark daemon. Includes `ReceiveAuthKey`, `SignReceiveAuthMessage[Compact]`,
  `ReceiveAuthECDH` for payment-scoped auth, and `SignVTXOForfeit` for
  connector-bound vHTLC-refresh signatures.
- `InvoiceCreator` — BOLT-11 invoice building; `CreateInvoiceWithKey`
  and the multi-hop `CreateInvoiceWithKeyRouteHintPath` for invoices
  signed with a `ReceiveAuthKey`.
- `PayState` / `ReceiveState` — typed FSM enums with `IsTerminal()` /
  `String()`. `ReceiveState` includes `ReceiveStateHTLCEventAccepted`.
  A `paySession` that reaches `PayStateCompleted` directly from
  `createSwap` (credit-only pay, no vHTLC) is treated as success by
  `runUntil`, not as a terminal error.
- `VHTLCConfig`, `InSwapConfig`, `InSwapQuote`, `RouteHint` —
  server-negotiation DTOs. `OutSwapQuote.RouteHintPath` is the full
  private hop path (final element is the swap server's virtual
  channel) — replaces the old single-hop `RouteHint` field.
  `SwapSummary` — flat list view for persisted sessions; now also
  carries `Preimage`, `CreditQuote`, `RequestedAmountSat`,
  `AttachedCreditSat`, `AvailableCreditSat`, `DustLimitSat`.
- `CreditOperation` / `CreditSnapshot` / `CreditLedgerEntry` /
  `CreditRedemption` / `CreditQuote` — server-authoritative credit
  account types returned by `SwapClient.CreateCredit`,
  `RedeemCredit`, and `ListCredits` (see `credits.go`). Credits are
  always keyed by the wallet's identity pubkey
  (`SwapClient.creditAccountKey`); the SDK never maintains its own
  credit balance.
- `SettlementTypeCredit` / `SettlementTypeMixed` — added alongside
  `SettlementTypeLightning` / `SettlementTypeInArk`. Credit-only pays
  fund nothing on Ark (`InSwapConfig.AmountSat == 0`,
  `InSwapConfig.Preimage` set directly by the server); mixed pays
  fund only `CreditQuote.ArkFundingSat` on top of a reserved credit.
- `ForfeitSignaturePayload` / `ForfeitParticipantSignature` — the
  exact transcript and participant signature for one vHTLC-refresh
  forfeit input, shared between in-swap (pay) and out-swap (receive)
  refresh signing. See `forfeit_refresh.go`.
- `OutSwapMailboxID` — derives a per-receive mailbox ID from the
  client identity key and invoice payment hash.
- Error sentinels (exported): `ErrSwapExpired`, `ErrSwapRefunded`,
  `ErrSwapSummaryNotFound`. Internal classifiers
  (`interventionError`, `failureError`, `retryableActionError`) live
  in `errors.go`.

## Relationships

- **Depends on**: `lib/arkscript` (vHTLC policy + claim/refund
  tapscript paths, and decoding the payment hash out of a vHTLC
  policy template for forfeit signing), `sdk/ark` (type aliases),
  `vtxo` (`ForfeitParticipantSignRequest`/`Descriptor` — the exact
  connector-bound signing request shape mirrored by
  `ForfeitSignaturePayload`), `daemonrpc`
  (`SignVTXOForfeitRequest`/`Response`), `swaprpc` (gRPC stubs),
  `mailbox/pb` (edge pull/ack), `serverconn` (`CompoundMailboxID`,
  `PubKeyMailboxID`), `serverconn/mailboxpull` (retry/backoff pull
  loop), `db` (`OpenSQLiteDatabase`, shared pragma config),
  `db/migrate` + `db/sqlc`, `sdk/swaps/sqlc`, `loop/fsm` (FSM engine),
  `lightning-onion` (Sphinx ECDH), `google.golang.org/grpc/codes`
  and `status` (classifying `AcknowledgeOutSwapHTLC` errors as
  terminal vs. retryable).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (`pay` /
  `receive` commands).

## Sends / Receives

Both FSMs tick via `loopfsm.StateMachine.SendEvent(ctx, OnAdvance,
nil)`. Pay calls `DaemonConn.SendOORWithPolicy` to fund and
`SendOORWithCustomInputs` to refund. Receive calls
`SendOORWithCustomInputs` to claim via the preimage spend path.
Credit-only pays (`SettlementTypeCredit`) skip OOR funding entirely —
`createSwap` transitions straight from `PayStateSwapCreated` to
`PayStateCompleted` using the `Preimage` the server returned inline.

Receive waits for either a Lightning-backed out-swap event or a
same-Ark vHTLC event: if `outEvents` implements
`IncomingVHTLCEventReceiver`, `WaitIncomingVHTLC` is called;
otherwise the flow falls back to `WaitOutSwapHtlc` and converts the
result into an `IncomingVHTLCNotification`. After a receive event is
accepted, `ackAcceptedHTLCEvent` first calls
`SwapServerConn.AcknowledgeOutSwapHTLC` (skipped for
`SettlementTypeInArk`) and only then clears the local pending-ack
cursor — the server ACK is the durable boundary, not the mailbox ack.

**Forfeit-signature relay (vHTLC refresh).** While a `ReceiveSession`
is funding, claiming, or completing
(`receiveTargetNeedsForfeitResponder`), `runUntil` spawns a goroutine
running `respondToOutSwapForfeitSignatureRequests`, which loops on
`OutSwapForfeitSignatureReceiver.WaitOutSwapForfeitSignature` (the
mailbox-backed implementation lives in `out_swap_mailbox.go`). Each
delivered `ForfeitSignaturePayload` is validated against the
session's remembered vHTLC fields
(`validateOutSwapForfeitSignaturePayload`), converted to a
`daemonrpc.SignVTXOForfeitRequest`
(`SignVTXOForfeitRequestFromPayload`), signed locally via
`DaemonConn.SignVTXOForfeit`, and the resulting
`ForfeitParticipantSignature` is pushed back with
`SwapServerConn.SubmitOutSwapForfeitSignature` before the mailbox
event is acked. The goroutine is bound to `runUntil`'s context and
stops via `defer stopResponder()` when the target state is reached or
the call returns.

The mirror-image in-swap (pay) transport,
`SwapServerConn.SignInSwapForfeit`, is implemented by
`GRPCSwapServerConn` but is not yet driven by any `paySession` code
path — pay-side vHTLC-refresh orchestration is not wired up.

**Credits.** `SwapClient.CreateCredit` / `RedeemCredit` /
`ListCredits` (in `credits.go`) are thin pass-throughs to a
`creditServerConn` (an optional interface `SwapServerConn`
implementations may satisfy) keyed by
`daemon.IdentityPubKey(ctx).SerializeCompressed()`. There is no local
credit ledger or FSM in this package — `CreditSnapshot` is always the
server's answer for the current call.

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
  This now also applies to
  `OutSwapForfeitSignatureNotification.Ack`: it must run only after
  the daemon signature is submitted to the server via
  `SubmitOutSwapForfeitSignature`, never before.
- `ReceiveAuthKey` signing/ECDH is always delegated to the daemon;
  the SDK never holds the raw private key for receive-auth. The same
  delegation rule applies to `SignVTXOForfeit` — the SDK builds and
  validates the transcript but never signs forfeit inputs itself.
- `AcknowledgeOutSwapHTLC` failures are classified by gRPC status
  code: `InvalidArgument`/`NotFound`/`PermissionDenied` are terminal
  (`isTerminalOutSwapHTLCAckError` → `newFailureError`);
  everything else, including `FailedPrecondition` (the server's
  "not published yet" transient state), is retryable. Do not widen
  this set without confirming the server's error contract.
- Before claiming, `claimFundedVHTLC` calls
  `reconcileLiveReceiveFunding` to re-read the live vHTLC row by
  `vhtlcPkScript`: a cooperative vHTLC refresh preserves the policy
  script but moves the claimable amount/outpoint, so a resumed or
  delayed receive session must follow the indexer's current row, not
  the one remembered at funding time. This is a best-effort refresh
  (logged and skipped on lookup failure), not a hard precondition.
- Onion validation (`validateOnionPayload`) now handles multi-part
  out-swap events: `event.Parts` (each with its own `OnionBlob` and
  `AmountMsat`) is validated per-part (address, total, per-part
  amount ≤ invoice amount) and the parts' `amountToForward` must sum
  exactly to the invoice amount. An event with no `Parts` is treated
  as one legacy single-part payment forwarding the full amount.
- `SettlementTypeCredit` in-swap configs/quotes never satisfy the
  `AmountSat > 0` check — `validateInSwapQuote` /
  `validateInSwapPreview` special-case `SettlementTypeCredit` to
  allow `AmountSat == 0` and require a payment-hash-matching
  `Preimage` instead; `SettlementTypeMixed` requires a non-nil
  `CreditQuote` and checks `AmountSat` against
  `CreditQuote.ArkFundingSat`, not the full invoice amount.
- Error sentinels (`ErrSwapExpired`, `ErrSwapRefunded`,
  `ErrSwapSummaryNotFound`) are exported; callers use `errors.Is`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
