# credit

## Purpose

Client-side durable-actor orchestration for the sat-native credit subsystem:
a server-held sat balance (per wallet identity) that lets a wallet handle
sub-dust Lightning amounts without changing the operator dust limit. This
package drives the three multi-step client flows — a credit/mixed **pay**
(with an optional Ark top-up), a sub-dust **credit receive**, and a
**redeem** that materializes available credits back into an Ark vTXO — as
crash-safe, restart-resumable operations. The server ledger is the
authoritative source of truth for balances; this package is only a
fault-tolerant progress tracker that re-drives from persisted state and
reconciles against the server (`ListCredits`) after any crash or disconnect.

## Key Types

- `Registry` — plain (non-durable) supervisor actor. Admits operations by
  writing a `credit_operations` row in an ordinary transaction, lazily spawns
  the per-operation durable child, routes resume/retry-timer pokes, reaps
  terminal children, restores non-terminal operations on boot
  (`RestoreNonTerminal`), and arbitrates auto-redeem admission
  (`considerRedeem`). Registered under `NewServiceKey()`
  (`"credit-client"`).
- `OpActor` / `opBehavior` — durable per-operation child (`DurableActor`,
  mailbox id `"credit-op-<opID>"`), built with `NewOpActor`. Drives the
  protofsm state machine one step per turn via `runFSM` so it can flush a
  `Stage` checkpoint between two states (persist-before-effect), and folds
  the final record snapshot plus the mailbox ack into one `Commit`
  (`commitAck`).
- `CreditState` / `CreditTransition` / `CreditEmittedEvent` — protofsm
  generic aliases (`states.go`) mirroring `round`'s `interfaces.go`. The
  machine does **not** run on protofsm's own `StateMachine` runner — the
  durable actor drives it manually so it can interleave a durable checkpoint
  between states.
- FSM states (one zero-sized marker per persisted `state` string, see
  `states.go`/`state.go`): `quotingState`, `topupCreatingState`,
  `topupFundingState`, `topupAwaitingCreditState`, `payingState`,
  `payAwaitingSettlementState`, `receiveCreatingState`,
  `awaitingSettlementState`, `redeemReservingState`,
  `redeemSubmittingState`, `awaitingOORState`, `completedState`,
  `failedState`. `CreditTransitions` (`transition_table.go`) enumerates
  every edge and its outbox directives; live dispatch is each state's
  `ProcessEvent` in `transitions.go`.
- `CreditOutMsg` outbox directives (`events.go`): `stageRecord` (durably
  checkpoint before the next state's effect), `parkOp` (stop this turn, arm
  the poll timer), `triggerRedeem` (Tell the registry a
  `ConsiderRedeemRequest` after the turn commits).
- `CreditServer` — swap-server surface the actor calls: `CreateCredit`,
  `ListCredits`, `RedeemCredit`, `StartPay`. Implemented in production by
  `swapclientserver.creditServerBridge` (build tag `swapruntime`).
- `CreditDaemon` — wallet/daemon surface: `IdentityPubKey`, `DustLimit`,
  `SendOOR`, `AllocateReceiveScript`, `FindLiveVTXOByPkScript`. Implemented
  in production by `swapclientserver.creditDaemonBridge`, which routes
  `SendOOR` through the daemon's `SendOOR` RPC (and so through the OOR
  registry) rather than the credit package calling `oor` directly.
- `Store` — durable control-plane store (`GetOperation`,
  `UpsertOperation`, `LookupActiveOperationByKey`, `ListNonTerminal`,
  `ListOperations`), backed in production by `*db.CreditOperationStoreDB`.
- `OpKind` (`db.CreditOpKind` alias) — `KindPay`, `KindReceive`,
  `KindRedeem`. `State` — the persisted FSM state string; `IsTerminal`,
  `Status()` map it to `db.CreditOpStatus`.
- `RegistryConfig` / `OpActorConfig` — actor construction config, including
  `AutoRedeemConfig` (`Enabled`, `MinRedeemSat`, `EarmarkedSat`) and the
  shared `EarmarkFunc` (`*atomic.Pointer[EarmarkFunc]`) the daemon wires
  after construction via `Registry.SetEarmarkProvider`.
- `NewRetryCallbackRef` (`retry_callback.go`) — bridges `timeout` actor
  poll-timer expiries into `ResumeCreditOpRequest{FromRetryTimer: true}`
  Told to the registry.

## Relationships

- **Depends on**: `baselib/actor` (plain + durable actor framework,
  `DeliveryStore`, `MessageCodec`), `baselib/protofsm` (`State`,
  `StateTransition`, `EmittedEvent`, `TransitionTable` generics), `db`
  (`CreditOperationRecord`, `CreditOperationStoreDB`, `CreditOpKind`/
  `CreditOpStatus`), `timeout` (poll/retry timer scheduling and expiry
  messages), `build` (context-scoped logger fallback).
- **Depended on by**: `swapclientserver` (`credit_bridge.go` — implements
  `CreditServer`/`CreditDaemon` over `swapclientrpc`/`daemonrpc`, build tag
  `swapruntime`), `swapwallet` (`router.go`/`recv.go` hand a sub-dust or
  shortfall `Send`/`Recv` off to the registry and return a pending
  `WalletEntry`; `credit_projector.go` polls `ListCreditOpsRequest` and
  projects op state onto `WalletEntry`), `darepod` (`credit_registry.go`
  builds and wires the registry, the dedicated `"credit-timeout"` actor, and
  the earmark setter published on `cfg.Swap.CreditEarmarkSetter`).
- **Sends**:
  - → per-operation child (registry `Tell`, only message crossing the
    durable child mailbox, TLV type `0x7102`): `ResumeCreditOpRequest`
  - → `Registry` (child `Tell`, after a turn commits): `ConsiderRedeemRequest`
    (settled receive cleared the auto-redeem watermark),
    `CreditTerminalNotification` (reap me)
  - → `timeout` actor (`Tell`): `timeout.ScheduleTimeoutRequest` (arm the
    reconciliation poll timer for an awaiting state)
  - → `CreditServer` (interface call, not an actor message):
    `CreateCredit`, `ListCredits`, `RedeemCredit`, `StartPay`
  - → `CreditDaemon` (interface call): `SendOOR`, `AllocateReceiveScript`,
    `FindLiveVTXOByPkScript`, `IdentityPubKey`, `DustLimit`
- **Receives**:
  - ← `swapwallet` (`Ask`, walletdk `Send`/`Recv` handoff):
    `StartCreditPayRequest`, `StartCreditReceiveRequest`
  - ← registry-internal (`considerRedeem` admits a fresh op): `RedeemRequest`
  - ← `darepod` boot (`Ask`, synchronous, before serving): `RestoreNonTerminalRequest`
  - ← `timeout` actor (via `NewRetryCallbackRef`, `Tell`): `ResumeCreditOpRequest`
  - ← `swapwallet` credit-op projector / status RPC (`Ask`): `ListCreditOpsRequest`

## Invariants

- Every operation uses one **stable idempotency key** derived from a durable
  identifier — `pay:<paymentHash>`, `recv:<paymentHash>`, or
  `redeem:<random>` — reused for both the server `CreateCredit`/
  `RedeemCredit` call *and* the delegated OOR transfer, so a crash-and-retry
  never mints a second server operation or a second transfer.
- **Persist-before-effect**: a state that mints a server-dependent identifier
  (a top-up destination, a redeem destination) emits `stageRecord`, and the
  driver mirrors the advanced state onto the record and flushes the
  checkpoint *before* the next state runs its effect. A crash before commit
  re-drives from the checkpointed state against the same identifier, never a
  freshly minted one the in-flight effect is no longer bound to.
- The supervisor **pre-writes** the `credit_operations` row (partial UNIQUE
  index on `op_key`) before spawning a child. `ResumeCreditOpRequest` is the
  child's only application-level inbound message — every other admission
  detail is reloaded from the durable row, never redelivered.
- A resumed row whose persisted `state` string does not decode to a known FSM
  state (`decodeCreditState`) is treated as **corrupt** and driven to a
  durable `failed` terminal, not silently respawned as non-terminal forever.
- Deterministic server rejections (insufficient balance, idempotency-key
  reuse with different params, expired invoice, impossible sub-dust)
  terminal-fail the op; transient errors (network, `Unavailable`) return the
  error for the durable-actor framework's retry/backoff.
- `MaxAwaitingPolls` (zero = unlimited) bounds an awaiting state's
  reconciliation polls so an operation the server never resolves
  terminal-fails instead of parking forever.
- A mixed pay (`payingState` → `completedState`) hands terminal authority to
  the swap monitor once `StartPay` is accepted; only a **credit-only** pay
  advances to `payAwaitingSettlementState` and self-reconciles against
  `ListCredits` by payment hash.
- **Auto-redeem is wallet-owned and never user-facing.** It fires only when
  a settled receive clears the watermark (`MinRedeemSat`, defaulting to the
  operator dust limit) or via the single boot-time reconcile — there is no
  periodic sweep. It subtracts the shared `EarmarkFunc`-reported balance
  (credits reserved by an in-flight `PrepareSend` with no durable row yet)
  before deciding, and the registry defers admission entirely while any
  `pay` or `redeem` op is non-terminal (a pending `receive` does not block).
- `credit` never imports `oor` or the swap-server RPC stubs directly; all
  cross-subsystem effects go through the `CreditServer`/`CreditDaemon`
  interfaces, which `swapclientserver` and `darepod` implement. This keeps
  the package unit-testable with fakes and avoids import cycles.
- `ResumeCreditOpRequest` is the only credit message registered in the
  child's `MessageCodec` (`NewCodec`) besides the framework's
  `RestartMessage`/`AskResponse` — every other `CreditMsg` only ever crosses
  the registry's plain in-memory mailbox and is not TLV-serializable.

## Deep Docs

- [docs/credit_system.md](../docs/credit_system.md) — Account model,
  receive/send rails, funding flows, operation states, restart rules; the
  end-to-end (server + client) picture, with cross-links into
  `sdk/walletdk`.
- [docs/credit_durable_actor_design.md](../docs/credit_durable_actor_design.md)
  — This package's crash-safety design in depth: stable keys, OOR
  delegation, auto-redeem policy, the protofsm drive loop, the three flows'
  state diagrams, durability schema, and crash-recovery walkthrough.
- [sdk/walletdk/CLAUDE.md](../sdk/walletdk/CLAUDE.md) — Wallet-facing SDK
  that folds credit details into `Receive`/`PrepareSend`/`Send`/`Balance`.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
