# credit

## Purpose

Durable credit subsystem: manages server-side micro-credit operations
for sub-dust payments, Lightning receives, and Ark redemptions. A plain
supervisor actor (`Registry`) spawns and monitors per-operation durable
child actors (`OpActor`), each running a protofsm FSM that drives the
full lifecycle of one credit operation (quoting → funding → settlement
→ terminal). The subsystem handles crash recovery by restoring
non-terminal operations from the durable store on boot.

## Key Types

- `Registry` — Supervisor actor. Admits new operations, routes resume
  timers, reaps terminal children, and restores in-flight operations on
  restart. Runs on a plain (non-durable) in-memory mailbox; no durable
  state of its own.
- `OpActor` — Wraps one per-operation durable actor. Owns the TLV
  mailbox, the protofsm engine, and the durable DB row. Stopped by the
  Registry when a terminal state is reached.
- `CreditMsg` — Sealed message interface for the supervisor mailbox
  (actor.Message only — not TLV-serializable, since supervisor state is
  in-memory).
- `CreditDurableMsg` — Sealed message interface for per-operation child
  mailboxes (must implement `actor.TLVMessage` for durable restart).
- `ResumeCreditOpRequest` — The only application-level message that
  crosses a durable child mailbox. Carries the operation ID so the
  child re-fetches its state from the DB after restart.
- `CreditState` / `CreditTransition` / `CreditEmittedEvent` — protofsm
  aliases for the credit FSM's state, transition, and emitted-event
  types.
- `Store` — Interface the subsystem requires for durable persistence;
  satisfied by `db.CreditOperationStoreDB`.
- `CreditServer` — Interface for reaching the swap server's credit RPC
  surface (CreateCredit, ListCredits, RedeemCredit, QuoteCredit); satisfied
  by `swapclientserver.creditServerBridge`.
- `CreditDaemon` — Interface for reaching daemon-side wallet actions
  (e.g. triggering OOR pays for top-ups); satisfied by the daemon facade.
- `AutoRedeemConfig` — Config for the auto-redeem policy that
  materializes idle credits back into Ark vTXOs above a threshold.

## Relationships

- **Depends on**:
  - `baselib/actor` (actor primitives, TLVMessage, ServiceKey, mailbox)
  - `baselib/protofsm` (FSM engine, EmittedEvent, StateTransition)
  - `db` (CreditOperationStoreDB, CreditOpKind, CreditOpStatus)
  - `timeout` (retry/callback timing)
  - `build` (subsystem logger)
- **Depended on by**:
  - `swapwallet` (routes credit-backed sends and receives through the
    Registry, projects credit op states onto wallet rows)
  - `swapclientserver` (implements CreditServer via creditServerBridge)
  - `darepod` (wires Registry into daemon startup, registers service key)
- **Sends**:
  - → `db` (CreditOperationStoreDB): UpsertOperation, GetOperation,
    ListNonTerminal (via actor transaction context)
  - → `swapclientserver` (via CreditServer): CreateCredit, ListCredits,
    RedeemCredit, QuoteCredit
  - → `swapwallet` (via CreditDaemon): OOR pay requests for Ark top-ups
- **Receives**:
  - ← `swapwallet`: StartCreditPayRequest, StartCreditReceiveRequest,
    ConsiderRedeemRequest, ListCreditOpsRequest
  - ← `darepod`: RestoreNonTerminalRequest (on daemon boot),
    ResumeCreditOpRequest (retry callbacks)

## Invariants

- The supervisor (`Registry`) holds no durable state; all crash-safe
  state lives in `db.CreditOperationStoreDB` and each child's TLV
  mailbox.
- `ResumeCreditOpRequestTLVType = 0x7102` must not collide with actor
  framework reserved types (0xFFFE/0xFFFF), OOR range (0x70xx first
  byte changes), or ledger range (0x90xx).
- The supervisor pre-writes the control-plane row before spawning a
  child, so a crash between spawn and first ack still restores
  correctly on restart via `RestoreNonTerminal`.
- `CreditOpKind` and `CreditOpStatus` values are append-only: numeric
  meanings must never shift.
- Auto-redeem policy runs only when credits exceed the configured
  threshold; it uses `EarmarkFunc` to avoid racing with an in-flight
  spend.

## Deep Docs

- [docs/credit_durable_actor.md](../docs/credit_durable_actor.md) —
  Credit subsystem design: FSM states, durable schema, resume path.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md)
  — CDC pattern and durable mailbox lifecycle.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
