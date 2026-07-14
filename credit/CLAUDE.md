# credit

## Purpose

Client-side credit subsystem: a supervisor/per-operation-actor pair that drives
sub-dust pays (with optional Ark top-up), server-owned Lightning receives, and
credit redemptions against the swap-server credit ledger, as a crash-safe
`protofsm` state machine per operation.

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
  allocation, VTXO lookup), and the durable control-plane store.
- `StartCreditPayRequest` / `StartCreditReceiveRequest` / `RedeemRequest` —
  admission messages for the three operation kinds (`KindPay`, `KindReceive`,
  `KindRedeem`).

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
- Auto-redeem is receive-triggered, not a periodic sweep (except a single
  boot-time reconcile); `triggerRedeem` fires only after the settled receive's
  terminal snapshot commits, so a crash before that leaves no half-applied
  redeem.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
