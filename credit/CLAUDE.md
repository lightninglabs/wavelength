# credit

## Purpose

Client-side credit subsystem: a supervisor/per-operation-actor pair that drives
sub-floor pays (with optional Ark top-up), server-owned Lightning receives, and
credit redemptions against the swap-server credit ledger, as a crash-safe
`protofsm` state machine per operation. "Sub-floor" means below the effective
operator VTXO floor, `max(dust_limit, min_vtxo_amount_sat)`.

## Key Types

- `Registry` — non-durable supervisor actor; admits operations by writing the
  control-plane row, spawns/reaps per-operation children, restores in-flight
  ops on boot, arbitrates auto-redeem.
- `OpActor` — durable per-operation actor (`opBehavior` on the
  Read/Stage/Commit path) that owns one operation's crash-safe FSM execution.
- `State` / `CreditState` (`protofsm.State[CreditEvent, CreditOutMsg,
  *opBehavior]`) — the per-operation FSM state, persisted in
  `credit_operations.state`.
- `CreditTransitions` (`CreditTransitionTable`) — static table documenting
  every valid state transition, mirrored by hand alongside the live
  `ProcessEvent` dispatch in transitions.go.
- `CreditServer` / `CreditDaemon` / `Store` — external surfaces: the
  swap-server credit/pay RPCs, the wallet/daemon (OOR send, receive-script
  allocation, VTXO lookup, and `VTXOFloor` — the live effective operator
  minimum, which must be positive or an error), and the durable control-plane
  store.
- `StartCreditPayRequest` / `StartCreditReceiveRequest` / `RedeemRequest` —
  admission messages for the three operation kinds (`KindPay`, `KindReceive`,
  `KindRedeem`).
- `LegacyReceivePollCapError` — the exact terminal error string older clients
  wrote when they wrongly applied the outgoing poll cap to a server-owned
  receive invoice. It is exported because the boot repair below and
  `swapwallet`'s activity repair both key off an exact match; treat it as a
  frozen compatibility constant, never as a reason to write anew.

## Relationships

- **Depends on**: `baselib/actor` (durable/plain actor framework, TLV
  mailbox), `baselib/protofsm` (state/transition/emitted-event generics),
  `db` (`CreditOperationRecord`/`CreditOpKind` control-plane schema),
  `db/actordelivery`, `timeout` (poll-timer scheduling).
- **Depended on by**: `swapwallet` (credit-aware pay/receive routing, the
  credit projector reading terminal ops), `swapclientserver` (bridges the
  swap-server credit RPCs into `CreditServer`), `waved` (registry wiring,
  config, service startup).

## Invariants

- Every `CreditDurableMsg` (crossing a per-operation durable mailbox) must
  satisfy `actor.TLVMessage`; `ResumeCreditOpRequest` is the only application
  message that does, encoded via a local TLV type in the `0x71xx` range.
- A transition must flush a `stageRecord` checkpoint before the next state
  runs a side effect that depends on a server identifier just recorded
  (persist-before-effect); `runFSM` enforces this ordering via `ax.Stage`.
- `applyState`'s persisted state string must exactly match the `State`
  constants in state.go; an unrecognized string durably fails the operation
  (`failCorrupt`) rather than wedging it non-terminal forever.
- Every external call the behavior makes (`CreateCredit`, `SendOOR`,
  `StartPay`, `RedeemCredit`) must stay idempotent by op key or payment hash,
  since a redelivered message or a reload-after-`commitFailed` re-runs it.
- **The server owns the receive invoice lifecycle.** `MaxAwaitingPolls` caps
  only pay top-ups, credit-only pays, and redemptions; `awaitingSettlementState`
  is deliberately uncapped. A local poll count cannot prove that a
  still-payable invoice failed, so only a server-reported
  `CREDITED`/`EXPIRED`/`FAILED`/`RELEASED` state may terminate a receive.
  Re-adding a client-side cap here reintroduces the false terminal failure the
  repair below exists to undo.
- **Legacy receive repair is best-effort and strictly scoped.**
  `repairLegacyReceivePollCapFailures` runs once per boot, ahead of
  `restoreNonTerminal`, and rewrites a row only when it matches every
  `isLegacyReceivePollCapFailure` predicate (receive kind, failed state and
  status, non-empty `ServerOpID`, and `LastError` equal to
  `LegacyReceivePollCapError`). All eligible rows share one `ListCredits`
  snapshot so the pass makes one coherent decision per boot. An unavailable
  store, identity, or server snapshot logs at `Info` and leaves every row
  untouched — absent evidence cannot prove a different state, so it must
  neither rewrite local history nor block ordinary boot restore.
- **Repair never displaces a newer operation.** Pending and completed rows
  participate in the active op-key uniqueness constraint, so before reviving a
  row the pass checks `LookupActiveOperationByKey`. If a retry admitted after
  the legacy failure already owns that key, the historical row stays failed and
  only its known-false verdict is replaced (`supersededReceiveReason`) rather
  than colliding with or displacing the live operation.
- Auto-redeem is receive-triggered, not a periodic sweep. Its boot reconcile
  retries transient failures with exponential backoff for at most four
  minutes, stops immediately on permanent mailbox/Ark version errors, and
  requires a positive live operator VTXO floor. The redeem watermark is
  `max(AutoRedeemConfig.MinRedeemSat, VTXOFloor())`, refreshed per decision,
  so a raised operator minimum takes effect immediately and an unavailable
  operator suppresses the sweep rather than minting below the floor. Its
  bounded context scopes evaluation only; the queued registry signal uses a
  separately cancelable child of the daemon-lifetime context so shutdown
  remains prompt.
  `triggerRedeem` fires only after the settled receive's terminal snapshot
  commits, so a crash before that leaves no half-applied redeem.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
