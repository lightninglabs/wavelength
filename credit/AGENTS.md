# credit

## Purpose

Orchestrates server-custodial sat "credits" for sub-dust and shortfall
Lightning flows: a **pay** (optional Ark top-up then a credit or mixed
Lightning payment), a **credit receive** (a server-owned invoice that
credits the account instead of paying an on-chain/Ark destination), and a
**redeem** (materializes an accumulated credit balance back into a
spendable Ark VTXO). Each operation runs as its own durable, crash-safe
state machine; a plain (non-durable) registry actor admits, routes, and
reaps them, and arbitrates when auto-redeem should fire.

## Key Types

- `Registry` — plain supervisor actor: writes the `credit_operations` row
  before spawning a child, routes messages to the right op's child,
  dedups admission, reaps terminal children, and arbitrates auto-redeem
  via `considerRedeem`.
- `OpActor` — the durable per-operation actor; wraps a
  `baselib/actor.DurableActor` and drives the operation's `protofsm` state
  machine one step at a time via its own `runFSM`, interleaving `Stage`
  checkpoints between states.
- `CreditState` / `CreditTransition` / `CreditEmittedEvent` — package
  aliases instantiating `baselib/protofsm`'s generics
  (`protofsm.State[CreditEvent, CreditOutMsg, *opBehavior]`, etc.) for
  this package's event/outbox/environment types.
- `CreditServer` / `CreditDaemon` / `Store` — the seam interfaces
  (`config.go`) that decouple `credit` from concrete `ledger`/`vtxo`/
  `wallet`/swap-server packages: `CreditServer` is the swap-server RPC
  surface (`CreateCredit`, `ListCredits`, `RedeemCredit`, `StartPay`),
  `CreditDaemon` is the wallet/daemon surface (`IdentityPubKey`,
  `DustLimit`, `SendOOR`, `AllocateReceiveScript`,
  `FindLiveVTXOByPkScript`), `Store` is the durable control-plane store.
- `CreditTransitions` — the static `protofsm.TransitionTable` describing
  every state's outgoing edges, mirroring `round`'s
  `BoardingClientTransitions`.

## Relationships

- **Depends on**: `baselib/actor` (`DurableActor`, mailbox refs,
  `TLVMessage`, receptionist/service-key registration), `baselib/protofsm`
  (`State`, `StateTransition`, `EmittedEvent`, `TransitionTable` generics
  this package instantiates), `db` (`CreditOperationRecord`,
  `CreditOpKind`, `CreditOpStatus`, `ErrCreditOperationNotFound`), `timeout`
  (poll/retry timer actor). No direct import of `ledger`, `vtxo`, or
  `wallet` — those are reached only through the `CreditServer`/
  `CreditDaemon`/`Store` interfaces so `credit` stays a leaf the swap
  runtime wires up.
- **Depended on by**: `darepod` (`credit_registry.go` builds
  `credit.NewRegistry` and registers the `"credit-timeout"` actor),
  `swapclientserver` (`credit_bridge.go` implements `CreditServer`/
  `CreditDaemon` against the swap gRPC server), `swapwallet`
  (`router.go` and `recv.go` start pay/receive ops for sub-dust or
  credit-assisted flows, `credit_projector.go` polls op status onto
  `WalletEntry`, `deps.go` holds `CreditRegistry
  actor.ActorRef[credit.CreditMsg, credit.CreditResp]`).
- **Sends**:
  - → `timeout`: `timeout.ScheduleTimeoutRequest` (arms the poll timer
    from a parked op).
  - → `credit` registry (self-package, cross-actor): `ConsiderRedeemRequest`,
    `CreditTerminalNotification` (from `OpActor` and from the boot-time
    `autoRedeemer`).
  - → `credit` op child (self-package, durable): `ResumeCreditOpRequest`
    (the *only* message ever delivered into a durable child's mailbox;
    TLV-durable).
  - → `CreditDaemon`/`CreditServer` (interface calls, not actor Tells):
    `SendOOR`, `CreateCredit`, `RedeemCredit`, `StartPay`.
- **Receives**:
  - ← API/`swapwallet`: `StartCreditPayRequest`, `StartCreditReceiveRequest`,
    `RedeemRequest`, `ListCreditOpsRequest` (all `Ask`).
  - ← `timeout`: `*timeout.ExpiredMsg`, bridged by `NewRetryCallbackRef`
    into `ResumeCreditOpRequest{FromRetryTimer: true}` told to the registry.
  - ← boot: `RestoreNonTerminalRequest`.

## Invariants

- **Persist-before-effect.** A state that mints a server-side identifier
  the next effect depends on (a top-up destination in
  `topupCreatingState`, a redeem destination in `redeemReservingState`)
  must emit `stageRecord` — durably checkpointing the record via `Stage`
  — before the FSM advances into the state that consumes it. Skipping
  this ordering means a crash mid-effect re-derives a fresh identifier on
  resume, orphaning the original reservation/chain-watch permanently.
- **Every server/wallet effect is idempotent by a stable key.**
  `CreateCredit`, `RedeemCredit`, `SendOOR`, `StartPay` must all be keyed
  by `b.rec.OpKey` or `b.rec.PaymentHash`, never an ephemeral value —
  otherwise a retry after a crash double-spends the effect (double
  top-up, double transfer).
- **No admission message ever enters a durable child's mailbox.** The
  registry pre-writes the `credit_operations` row in an ordinary
  transaction before spawning the child; the child's only application
  message is `ResumeCreditOpRequest`, and it always reloads state from
  the durable row. Introducing a second durable-mailbox message type
  would duplicate persisted state and break the single-durable-mailbox
  correctness argument.
- **protofsm here is driven manually, not via its own runner.**
  `OpActor.runFSM` calls `ProcessEvent` one step at a time and interleaves
  `Stage` checkpoints between states as directed by outbox directives
  (`stageRecord`, `parkOp`, `triggerRedeem`); it deliberately does not use
  `protofsm.StateMachine`'s built-in runner, which would chain internal
  events within a single turn and collapse multiple durable checkpoints
  into one. See `docs/credit_durable_actor_design.md` section 4 before
  changing how `OpActor` drives transitions.
- **A corrupt persisted state must terminal-fail, not silently no-op.**
  `decodeCreditState` returning an unrecognized state string must route
  through `failCorrupt` to a durably committed `StateFailed`; otherwise
  `RestoreNonTerminal` respawns the same unresolvable row on every boot.
- **Auto-redeem arbitration is registry-owned and must stay atomic.**
  `awaitingSettlementState` only signals intent via `triggerRedeem`;
  `registryBehavior.considerRedeem` scans `ListNonTerminal` and defers
  whenever a `KindPay` or `KindRedeem` op is in flight (a `KindReceive`
  does not block). Because the registry is single-goroutine, this
  scan-then-admit must remain atomic — answering it off-actor risks
  double-redeeming the same balance.
- **Earmark subtraction is fail-safe.** Both `redeemWatermarkCleared` and
  `autoRedeemer.reconcile` must treat any earmark-provider error as
  "don't redeem" and must never let the subtraction underflow;
  over-redeeming strands a pending send or forces a re-top-up.

## Deep Docs

- [docs/credit_durable_actor_design.md](../docs/credit_durable_actor_design.md)
  — the authoritative design doc: requirements, the plain-supervisor /
  durable-per-op-actor topology, the protofsm migration rationale, all
  three flows (pay/receive/redeem), auto-redeem, durability/schema, and
  the crash-recovery walkthrough. Start here for anything non-trivial.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md)
  — general durable actor / CDC pattern (Read/Stage/Commit) that
  `OpActor` follows.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
