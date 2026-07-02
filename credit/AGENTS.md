# credit

## Purpose

Client-side "credit" subsystem that lets the wallet borrow against a
server-managed Lightning credit line to cover sub-dust or shortfall
payments, receive Lightning payments into that credit line, and
auto-redeem accumulated credits back into an Ark vTXO. Each operation
(pay, receive, redeem) is a durable, crash-safe `protofsm` state machine
run by its own per-operation actor, coordinated by a supervising
`Registry`.

## Key Types

- `Registry` — plain (non-durable) supervisor actor. Admits operations
  (dedup by `OpKey`, writes the `credit_operations` control-plane row),
  spawns/reaps per-operation `OpActor` children, routes resume timers,
  restores in-flight operations on boot, and runs the wallet-owned
  auto-redeem policy (`autoRedeemer`).
- `OpActor` — one durable per-operation actor (`baselib/actor.DurableActor`),
  keyed by `ActorIDForOp(opID)`. Runs `opBehavior` on the
  Read/Stage/Commit (`TxBehavior`) path.
- `opBehavior` — drives one operation's `protofsm` state machine
  (`CreditTransitions`) to completion or to a parked awaiting state,
  checkpointing via `Stage` and acking via `Commit` in the same turn.
- `CreditState` / `CreditTransition` / `CreditEmittedEvent` — protofsm
  generic aliases (`CreditEvent` in, `CreditOutMsg` out, `*opBehavior`
  environment). Concrete states in `states.go` (`quotingState`,
  `topupCreatingState`, `topupFundingState`,
  `topupAwaitingCreditState`, `payingState`,
  `payAwaitingSettlementState`, `receiveCreatingState`,
  `awaitingSettlementState`, `redeemReservingState`,
  `redeemSubmittingState`, `awaitingOORState`, `completedState`,
  `failedState`). `CreditTransitions` (`transition_table.go`) is the
  static table documenting every edge; `transitions.go` is the live
  `ProcessEvent` dispatch.
- `opDrive` — the single `CreditEvent` that advances the FSM one step;
  `stageRecord` / `parkOp` / `triggerRedeem` — the `CreditOutMsg` outbox
  directives a transition can emit (checkpoint, park-on-poll,
  ask-registry-to-redeem).
- `CreditServer` — swap-server surface (`CreateCredit`, `ListCredits`,
  `RedeemCredit`, `StartPay`), implemented by
  `swapclientserver.creditBridge`.
- `CreditDaemon` — wallet/daemon surface (`IdentityPubKey`, `DustLimit`,
  `SendOOR`, `AllocateReceiveScript`, `FindLiveVTXOByPkScript`),
  implemented by the daemon/wallet layer in `darepod`.
- `Store` — durable control-plane store, implemented by
  `db.CreditOperationStoreDB`.
- `RegistryConfig` / `OpActorConfig` — wiring for server/daemon/store,
  `TimeoutActor`/`CallbackRef` (poll timers), `AutoRedeemConfig`.

## Relationships

- **Depends on**: `baselib/actor` (durable actor + supervisor
  frameworks, TLV codec, service keys), `baselib/protofsm` (generic FSM
  engine), `db` (`CreditOperationRecord`/`CreditOperationStoreDB`),
  `timeout` (poll/retry timers).
- **Depended on by**: `swapclientserver` (implements `CreditServer` via
  `credit_bridge.go`), `swapwallet` (router.go issues
  `StartCreditPayRequest`; recv.go issues `StartCreditReceiveRequest`;
  credit_projector.go polls `ListCreditOpsRequest` to project terminal
  ops into wallet entries), `darepod` (implements `CreditDaemon`, wires
  `Registry` via `credit_registry.go`/`config.go`/`server.go`).
- **Sends** (registry/child → server or daemon, via `CreditServer`/
  `CreditDaemon` interface calls, not actor messages):
  - → `swapclientserver`: `CreateCredit`, `ListCredits`, `RedeemCredit`,
    `StartPay`.
  - → wallet/daemon (`darepod`): `SendOOR` (fund an Ark top-up),
    `AllocateReceiveScript` / `FindLiveVTXOByPkScript` (redeem
    destination), `IdentityPubKey`, `DustLimit`.
  - → `timeout`: `ScheduleTimeoutRequest` (arm a reconciliation poll via
    `armPollTimer`).
- **Receives**:
  - ← `swapwallet` (`Registry.Ref()`/`TellRef()` via
    `NewServiceKey()`/receptionist lookup): `StartCreditPayRequest`,
    `StartCreditReceiveRequest`, `ListCreditOpsRequest`. `RedeemRequest`
    is issued internally by the wallet-owned auto-redeem policy, never
    externally.
  - ← `timeout` (via `NewRetryCallbackRef`, mapped to
    `ResumeCreditOpRequest{FromRetryTimer: true}`): poll-timer expiry.
  - ← itself/registry-internal: `ResumeCreditOpRequest` (boot restore or
    resume-after-admit), `ConsiderRedeemRequest` (receive FSM or
    boot reconcile signaling an over-watermark balance),
    `CreditTerminalNotification` (child → registry, reap trigger),
    `RestoreNonTerminalRequest` (boot-time restore entry point).
  - Only `ResumeCreditOpRequest` (plus the framework's `RestartMessage`)
    crosses a per-operation child's durable, TLV-serialized mailbox
    (`CreditDurableMsg`); every other message stays on the supervisor's
    plain in-memory mailbox (`CreditMsg`).

## Invariants

- The supervisor writes the control-plane row (`UpsertOperation`) in its
  own transaction *before* spawning a child, so a durable row always
  exists before any child turn runs, and a crash between the write and
  the spawn is recovered by `RestoreNonTerminal` on the next boot.
- Every external call a state's `ProcessEvent` makes (`CreateCredit`,
  `RedeemCredit`, `StartPay`, `SendOOR`) is idempotent by the op key or
  the invoice payment hash, so a redelivered `opDrive` after a crash
  reconciles against the same server/OOR operation rather than
  duplicating it.
- A `stageRecord` outbox directive is always flushed (persist-before-
  effect) before the next state's side-effecting call runs, so a crash
  between them re-drives from the checkpointed identifier instead of
  re-deriving a fresh one the in-flight effect is no longer bound to.
- Auto-redeem is fail-safe on the earmark: an earmark-provider error
  skips the redeem rather than risking redeeming credits an in-flight
  send is about to spend. The registry additionally enforces a
  no-pending-pay/redeem interlock (`considerRedeem`) before ever
  admitting a `RedeemRequest`.
- `MaxAwaitingPolls` (when non-zero) bounds every awaiting state so an
  operation the server never resolves terminal-fails instead of parking
  forever; zero relies solely on server-reported terminal states
  (`expired`/`failed`/`released`).
- A corrupt persisted state string (`decodeCreditState` returns
  `known=false`) is driven to `StateFailed` durably (`failCorrupt`)
  rather than silently treated as already-terminal, so the row always
  reaches a reapable terminal commit.
- `CreditOnly` pays are reconciled to settlement by the credit FSM
  itself (`payAwaitingSettlementState`); mixed pays hand terminal
  authority to the swap monitor and complete immediately on hand-off —
  the FSM must not claim authority it does not own.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — CDC pattern, durable mailbox lifecycle
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist
