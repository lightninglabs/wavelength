# sdk/swaps

## Purpose

Atomic swap SDK for Lightning-to-Ark and Ark-to-Lightning transfers coordinated
via virtual HTLC (vHTLC) channels. Provides durable per-session FSM-based state
machines with SQL-backed persistence, resumable cross-chain coordination, and
deterministic taproot script derivation.

## Key Types

- `SwapClient` — Main public API facade. Wraps `SwapServerConn` (swap server),
  `DaemonConn` (local Ark daemon), `InvoiceCreator` (Lightning invoices), and
  `Store` (isolated session persistence). Owns reconciliation FSMs and timeout
  management for both pay and receive flows. Configurable poll intervals,
  grace periods, and retry limits. Construct via `NewSwapClient` (full wiring)
  or `NewSwapClientWithStore` (store-only, for list/inspect without connections).

- `SwapSummary` — Stable list view for persisted swap sessions: Direction
  (pay/receive), PaymentHash, State, Pending (bool), AmountSat, FeeSat,
  MaxFeeSat, VHTLCOutpoint, VHTLCAmountSat, FundingSessionID, ClaimSessionID,
  RefundSessionID, TerminalReason, CreatedAt, UpdatedAt, Deadline,
  RefundLocktime.

- `PayState` (uint8) — Ark-to-Lightning client-side FSM states:
  - `PayStateCreated` → `PayStateSwapCreated` → `PayStateFundingInitiated` →
    `PayStateVHTLCFunded` → `PayStateWaitingForClaim` → `PayStateCompleted`
    (success path)
  - `PayStateRefundInitiated` → `PayStateRefunded` (timeout recovery path)
  - `PayStateExpired`, `PayStateNeedsIntervention`, `PayStateFailed` (terminal
    error states)

- `ReceiveState` (uint8) — Lightning-to-Ark client-side FSM states:
  - `ReceiveStateCreated` → `ReceiveStateInvoiceCreated` →
    `ReceiveStateVHTLCFunded` → `ReceiveStateClaimInitiated` →
    `ReceiveStateCompleted` (success path)
  - `ReceiveStateExpired`, `ReceiveStateNeedsIntervention`, `ReceiveStateFailed`
    (terminal error states)

- `paySession` — Durable per-session state for pay flows: PaymentHash,
  RequestedAmount, ServerAmount, FeeSat, MaxFeeSat, ServerVHTLCPkScript,
  ServerClaimScript, ServerUnilateralClaimDelay, ServerRefundLocktime,
  ClientVHTLCPrivKey, ServerPubKey, InvoiceStr, VHTLCOutpoint,
  FundingSessionID, FundingResumeAttempts, ClaimPreimage, CurrentPayState,
  TerminalReason, CreatedAt, UpdatedAt.

- `ReceiveSession` — Durable per-session state for receive flows:
  PaymentHash, RequestedAmount, FeeSat, Invoice, Preimage, RouteHints,
  ServerVHTLCPkScript, ServerClaimScript, ServerUnilateralClaimDelay,
  ServerRefundLocktime, ClientVHTLCPrivKey, ServerPubKey, ExpirySeconds,
  VHTLCOutpoint, VHTLCAmount, ClaimSessionID, ClaimResumeAttempts,
  CurrentReceiveState, TerminalReason, CreatedAt, UpdatedAt.

- `SwapServerConn` — Interface for swap server communication:
  - `RequestChannelID(ctx, vhtlcPubkey, expirySeconds)` →
    (RouteHint, VHTLCConfig, error)
  - `CreateInSwap(ctx, invoice, maxFeeSat, clientVhtlcPubkey)` →
    (InSwapConfig, error)
  - `Close()` error

- `DaemonConn` — Interface for local Ark daemon wallet/OOR operations
  (capability boundary for swap FSMs):
  - `BlockHeight(ctx)` → uint32
  - `SendOORWithPolicy(ctx, amountSat, recipientPolicyTemplate)` →
    (sessionID string, error)
  - `SendOORWithCustomInputs(ctx, recipientPubKey, amountSat, inputs)` →
    (sessionID string, error)
  - `IdentityPubKey(ctx)`, `OperatorPubKey(ctx)` → (*btcec.PublicKey, error)
  - `ListLiveVTXOs(ctx)`, `ListSpentVTXOs(ctx)` → ([]VTXOInfo, error)
  - `FindLiveVTXOByPkScript(ctx, pkScript)`,
    `FindSpentVTXOByPkScript(ctx, pkScript)` → (*VTXOInfo, error)
  - `GetIndexedOORSession(ctx, pkScript, sessionTxID)` →
    (*IndexedOORSessionInfo, error)
  - `AllocateOORReceiveScript(ctx, label)` → (*OORReceiveInfo, error)

- `InvoiceCreator` — Interface for Lightning invoice generation:
  `CreateInvoice(ctx, amountSat, memo, routeHint, expiry, preimage)` →
  (*invoices.Invoice, Hash, error).

- `VHTLCConfig` — Server-provided vHTLC parameters: RefundLocktime (absolute
  block height), UnilateralClaimDelay, UnilateralRefundDelay,
  UnilateralRefundWithoutReceiverDelay (all relative blocks), SwapServerPubkey.

- `InSwapConfig` — Server response for Ark-to-Lightning swap creation:
  PaymentHash, VHTLCPkScript, ClaimScript, VHTLCAmount, RefundLocktime,
  ExpirySeconds, SwapServerPubkey.

- `PayResult` — Outcome of successful `PayViaLightning`: PaymentHash, Preimage,
  FundingSessionID, FeeSat.

- `ReceiveResult` — Outcome of successful `ReceiveViaLightning`: Invoice,
  Preimage, PaymentHash, VTXOOutpoint, AmountSat.

- `RouteHint` — Lightning route hint for invoices: NodeID, ChannelID,
  FeeBaseMsat, FeePropPpm, CltvExpiryDelta.

- `Store` — Isolated SQLite-backed persistence for swap sessions. Provides
  read/write on `paySession` and `ReceiveSession` with TLV-based row
  serialization. Uses `golang-migrate` for schema versioning.
  `LatestMigrationVersion = 1`.

- `SqliteStoreConfig` — Configuration for the isolated swap SQLite database:
  DatabaseFileName (default `"swaps.db"`), SkipMigrations.

- Error sentinels:
  - `ErrSwapExpired` — Swap deadline elapsed (terminal, no refund available yet)
  - `ErrSwapRefunded` — Pay swap timed out and refund completed (terminal)
  - `interventionError` — Anomalous state; wrapped with reason and cause for
    operator inspection (terminal)
  - `failureError` — Unrecoverable non-intervention failure (terminal)
  - `retryableActionError` — External action may have succeeded but durable
    metadata persistence failed; signals FSM to retry the transition

## Relationships

- **Depends on**: `sdk/ark` (DaemonConn adapter, VTXOInfo, OORReceiveInfo,
  CustomOORInput, IndexedOORSessionInfo), `lib/arkscript` (policy template ops
  for vHTLC script derivation), `lnd/lntypes` + `lnd/invoices` (preimage,
  hash, invoice types), `btcd/btcec/v2` (key ops), database/sql + SQLite
  (session persistence), `golang-migrate` (schema management).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (swap CLI commands).

## Sends / Receives

### Pay Flow (Ark → Lightning)

1. **← API**: `PayViaLightning(ctx, routes, amountSat, maxFeeSat)` or
   `StartPayViaLightning(ctx, …, targetState)` to stop mid-flow.
2. **→ SwapServerConn**: `RequestChannelID(vhtlcPubkey, expirySeconds)` —
   get route hint + VHTLCConfig, derive vHTLC pkScript.
3. **→ SwapServerConn**: `CreateInSwap(invoice, maxFeeSat, pubkey)` →
   `InSwapConfig`; persisted as `paySession` at `PayStateSwapCreated`.
4. **→ DaemonConn**: `SendOORWithPolicy(amountSat, vHTLCPolicy)` — OOR session
   ID persisted at `PayStateFundingInitiated`.
5. **→ DaemonConn**: `FindLiveVTXOByPkScript(vHTLCPkScript)` — poll until
   vHTLC indexed (`PayStateVHTLCFunded`).
6. **→ DaemonConn**: `GetIndexedOORSession(vHTLCPkScript, sessionID)` — poll
   for server claim spend + preimage (`PayStateWaitingForClaim` →
   `PayStateCompleted`).
7. On timeout: `PayStateRefundInitiated` → submit/observe refund spend →
   `PayStateRefunded` (returns `ErrSwapRefunded`).

### Receive Flow (Lightning → Ark)

1. **← API**: `ReceiveViaLightning(ctx, amountSat, expirySeconds, memo)` or
   `StartReceiveViaLightning(ctx, …, targetState)`.
2. **→ SwapServerConn**: `RequestChannelID(vhtlcPubkey, expirySeconds)` — get
   route hint + VHTLCConfig.
3. **→ InvoiceCreator**: `CreateInvoice(…, routeHint, expiry, preimage)` —
   invoice + preimage generated; persisted at `ReceiveStateInvoiceCreated`.
4. **→ DaemonConn**: `FindLiveVTXOByPkScript(vHTLCPkScript)` — poll until
   server funds vHTLC (`ReceiveStateVHTLCFunded`).
5. **→ DaemonConn**: `AllocateOORReceiveScript(label)` + `SendOORWithCustomInputs`
   — sweep vHTLC with preimage; OOR claim session ID persisted at
   `ReceiveStateClaimInitiated`.
6. **→ DaemonConn**: `GetIndexedOORSession(…)` — poll until claim indexed
   (`ReceiveStateCompleted`).

### Resume / List

- `ResumePayViaLightning(ctx, paymentHash)` / `ResumeReceiveViaLightning` —
  load session from store, re-enter FSM at last persisted state, reconcile to
  terminal.
- `ListSwapSummaries(ctx, pendingOnly)` — scan store, return summarized
  sessions; usable with a store-only `SwapClient` (no live connections
  required).

## Invariants

- Swap session state in the SQLite store is the single source of truth;
  in-memory FSMs are ephemeral and rebuilt from the store on resume.
- OOR session IDs (FundingSessionID, ClaimSessionID, RefundSessionID) are
  persisted durably before being passed to external daemon calls. Resume
  never re-issues the same OOR transfer for the same session.
- vHTLC script derivation is deterministic from (hash, server pubkey, refund
  locktime, timelocks) — same inputs always produce the same pkScript.
- Pay swaps refund only after `VHTLCConfig.RefundLocktime` (absolute block
  height); `refundLocktimeBuffer` blocks before that deadline is enforced so
  the client never attempts a funding or refund too close to the window edge.
- Receive swaps claim with the preimage generated locally at invoice creation
  time; the preimage is never mutated after `ReceiveStateInvoiceCreated`.
- Swap deadlines (invoice lifetime, pay swap expiry window) are never extended
  and gate every state transition that depends on time.
- Resume operations are idempotent: re-entering a terminal session returns the
  same terminal result without additional external effects.
- Error classification (ErrSwapExpired, interventionError, failureError reason)
  is persisted with the session so the same classification survives restart.
- `fundingExpiryBuffer` and `refundLocktimeBuffer` ensure the client aborts if
  the server deadline has less than the safety margin remaining.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
