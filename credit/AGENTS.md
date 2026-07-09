# credit

## Purpose

Durable actor subsystem that admits and drives server-side "credit"
account operations (sub-dust/shortfall pay, Lightning receive, redeem)
against the swap server's credit ledger, folding Ark top-ups and
wallet-owned auto-redeem into a single per-operation protofsm state
machine.

## Key Types

- `Registry` — plain in-memory supervisor actor. Admits operations by
  writing their control-plane row, spawns/routes/reaps per-operation
  durable children, restores in-flight operations on boot, and runs the
  wallet-owned auto-redeem policy.
- `OpActor` — durable per-operation actor wrapping one pay/receive/redeem
  operation's protofsm state machine (`opBehavior`), running on the
  Read/Stage/Commit path.
- `CreditServer` — swap-server credit and pay surface the actor drives
  (`CreateCredit`, `ListCredits`, `RedeemCredit`, `StartPay`); implemented
  in production by `swapclientserver`'s bridge.
- `CreditDaemon` — local wallet/daemon surface the actor drives
  (`IdentityPubKey`, `DustLimit`, `SendOOR`, `AllocateReceiveScript`,
  `FindLiveVTXOByPkScript`).
- `Store` — durable control-plane store interface for
  `credit_operations` rows; `*db.CreditOperationStoreDB` in production.
- `CreditTransitionTable` (`CreditTransitions`) — the static protofsm
  transition table for the quoting/top-up/pay/receive/redeem state
  machine, documented alongside the live dispatch in `transitions.go`.

## Relationships

- **Depends on**: `baselib/actor` (durable actor, mailbox, service-key
  framework), `baselib/protofsm` (state, transition, and emitted-event
  generics the FSM is built on), `db` (`CreditOperationRecord`,
  `CreditOpKind`, `CreditOpStatus` schema), `timeout` (poll/retry timer
  scheduling), `build` (logger-from-context).
- **Depended on by**: `swapwallet` (`router.go` admits credit/mixed pays,
  `recv.go` admits credit receives, `credit_projector.go` lists ops and
  projects terminal transitions into wallet entries, `deps.go` holds the
  registry ref), `swapclientserver` (`credit_bridge.go` implements
  `CreditServer`/`CreditDaemon` against the swap-server RPC and the
  wallet), `darepod` (`credit_registry.go` constructs and starts the
  `Registry` at daemon boot; `config.go`/`server.go` wire the ref through
  `Config.Swap`).

- **Sends**:
  - → `timeout`: `ScheduleTimeoutRequest` (arms the reconciliation poll
    timer for an awaiting state).
- **Receives**:
  - ← `swapwallet`: `StartCreditPayRequest`, `StartCreditReceiveRequest`,
    `ListCreditOpsRequest`.
  - ← `timeout`: `*timeout.ExpiredMsg`, bridged by `NewRetryCallbackRef`
    into a `ResumeCreditOpRequest` told to the registry.

## Invariants

- The supervisor (`Registry`) holds no durable state of its own: it
  always writes the control-plane row in an ordinary transaction
  *before* spawning or resuming the owning child, so a crash between the
  write and the spawn is recovered by `RestoreNonTerminal` on the next
  boot.
- Every external call an operation makes (`CreateCredit`, `SendOOR`,
  `ListCredits`, `StartPay`, `RedeemCredit`) is idempotent by the op key
  or the invoice payment hash, so redelivery after a crash never
  double-executes an effect.
- A `stageRecord` outbox directive must be flushed (Stage write) before
  the next state runs its side effect — the persist-before-effect
  invariant that lets a crash re-drive from the checkpointed state
  instead of re-deriving an identifier the in-flight effect no longer
  matches.
- Only `ResumeCreditOpRequest` crosses a per-operation child's durable
  mailbox at the application level (plus the framework-injected
  `RestartMessage`); every other admission detail is reloaded from the
  durable row rather than redelivered as a message.
- Redemption is never user-triggered: the wallet's auto-redeem policy
  (steady-state via a settled receive, boot-time via a single
  reconcile) is the only source of `RedeemRequest` admissions, gated by
  a no-pending-pay/redeem interlock in the registry.
- Persisted `State` string values (`state.go`) must stay stable across
  versions and match the `String()` methods of the concrete FSM states
  in `states.go` exactly; an unrecognized string is treated as a corrupt
  row and driven to a durable failure rather than silently retried.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
