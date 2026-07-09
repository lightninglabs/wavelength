# Credit Durable-Actor Design

The credit subsystem orchestrates server-custodial sat "credits" through three
multi-step flows:

- a sub-dust or shortfall **pay** (an optional Ark top-up, then a credit or
  mixed pay),
- a sub-dust **credit receive**, and
- a **redeem** that materializes available credits back into an Ark vTXO.

The server ledger is authoritative and guarantees no fund loss. The *client
orchestration* of these flows, however, must be fault-tolerant on its own: it has
to survive a daemon restart and a client disconnect, it must never fund a top-up
twice, and it must never strand credits with no way to recover them. This
document describes how the subsystem meets those requirements.

---

## 1. Requirements

Two properties make the orchestration crash-safe.

- **Stable idempotency keys** derived from durable identifiers (a payment hash or
  a persisted operation id), written *before* the first server call.
- **Durable, daemon-owned execution** that survives restart and client
  disconnect, re-driving from persisted state and reconciling against the server
  ledger (`ListCredits`) as the source of truth.

Both exist to prevent two failure modes.

1. **Double top-up.** A crash or client disconnect after the top-up transfer
   fires but before the server finalizes the credit must not cause a second
   transfer. With an ephemeral key, a retry would mint a new key, create a second
   server credit operation with a new destination, and send a second transfer.
   Both would credit the account; the excess would park as credit balance. A
   stable key reused across the retry collapses this to a single operation and a
   single transfer.
2. **Stranded credits.** Any credit balance, including the excess from a
   double-funded top-up, can otherwise only be spent by a future credit or mixed
   send. Auto-redeem (section 6) recovers it back into a spendable vTXO without
   exposing redemption to the user.

---

## 2. Foundations

### 2.1 Three logical operations

Everything reduces to three durable operations with one stable key each.

| Op | Stable key | Authoritative completion signal |
|---|---|---|
| `pay` (with optional top-up) | `pay:<paymentHash>` | server ledger shows credits available, then `StartPay` idempotent by payment hash |
| `recv` (credit receive) | `recv:<random>` | server ledger shows the receive op `CREDITED` |
| `redeem` (credits to vTXO) | `redeem:<random>` | redeemed vTXO lands locally (`FindLiveVTXOByPkScript`) |

The **same key** goes to the server `CreateCredit` or `RedeemCredit` *and* to the
OOR transfer. On any resume, `CreateCredit(key)` returns the existing op and the
same destination, and the OOR transfer dedups, so **at most one OOR transfer ever
exists per operation**, regardless of crash timing.

### 2.2 OOR is delegated, never re-implemented

The top-up (client to server-owned credit destination) and the redeem (server to
client destination) are **OOR transfers**. The `oor` subsystem already runs
durable, idempotency-key-deduped, crash-safe OOR sessions, so both transfers pass
`key` as the OOR `StartTransferRequest.IdempotencyKey` rather than calling
`daemon.SendOOR` inline.

This has a clean consequence: the credit layer does not track the OOR session's
terminal state for the top-up. The OOR actor owns transfer crash-safety, and the
**server ledger** (`ListCredits` reaching `CREDITED`) is the completion signal.
The credit layer waits on the ledger, not on OOR internals. Redeem is the one
exception: its completion signal is the local vTXO landing, because there the
value flows server to client.

### 2.3 Auto-redeem policy (wallet-owned, never user-facing)

The wallet decides when to redeem, and redemption is **not exposed to the user**.
A settled receive is the only event that grows the available balance, so
auto-redeem is driven by the receive state machine: when a receive settles and
the available balance clears the watermark, it signals a redeem (section 6). A
single boot-time reconcile covers the one case the receive trigger cannot, a
balance already sitting over the watermark at startup. There is no periodic
sweep.

The policy stays conservative, to avoid churn and to never strand value:

- **Threshold.** Auto-redeem fires once `available_sat` strictly exceeds
  `MinRedeemSat`, which **defaults to the operator dust limit**. The dust limit
  is the smallest amount that can legally become a vTXO, so the default recovers
  stranded value as early as possible and never attempts an impossible sub-dust
  redemption.
- **Earmark interlock.** A credit-backed `PrepareSend` reserves nothing
  server-side and writes no durable row until `Send`. Before deciding,
  auto-redeem subtracts the credits earmarked by such in-flight sends (an
  `EarmarkFunc` wired from the wallet's prepared-send store), so it never redeems
  credits the user is about to spend.
- **In-flight interlock.** The registry blocks a redeem while any pay or redeem
  operation is in flight on a durable `credit_operations` row (section 6).
- Auto-redeem reads only `available_sat`, never reserved balance, and can be
  disabled entirely for raw or advanced callers.

### 2.4 Error discipline

Returning an error from a turn means retry until dead-letter, so each transition
classifies server responses:

- **Deterministic rejections** (insufficient balance, idempotency-key reuse with
  different params, expired invoice, impossible sub-dust) **terminal-fail** the
  op with a durable reason. They never wedge in redelivery.
- **Transient errors** (network, `Unavailable`) return the error for the
  framework's free retry and backoff.

### 2.5 walletdk handoff

`Send` and `Recv` do not run the multi-step work inline. They hand off and return
a **pending `WalletEntry`** immediately:

- `router.Send`: when the quote rail is `CREDIT` or `MIXED` (or the amount is
  below dust), it hands the prepared invoice and quote to the credit subsystem
  and returns the pending entry.
- `recv.Recv`: when the amount is below dust and not credit-assisted, it hands
  off a `recv` op and returns the server invoice and pending entry.
- The runtime monitor loop projects credit-op state transitions onto
  `WalletEntry` exactly as it does for swaps; the entry id is the stable op key or
  server operation id.

The public RPC shape (`PrepareSend`, preview, `Send`) is unchanged.

---

## 3. Architecture: plain supervisor, durable per-op actor

Durability lives in exactly two places: the `credit_operations` control-plane
table and the per-operation child's durable mailbox. That is all the correctness
argument needs. Every external effect is idempotent by the stable op key (or the
invoice payment hash), and the server ledger is the authoritative completion
signal, so durability reduces to "persist the row, then re-drive non-terminal
rows on boot." The registry that admits operations holds no durable state worth a
second mailbox, so it is a plain supervisor: the synchronous caller already
retries under the same stable key, and the boot scan already finds any committed
row.

### 3.1 Topology

```
walletdk Send/Recv
      │  Ask StartCredit{Pay,Receive}Request          (plain actor Ask; returns pending entry)
      ▼
Registry                    plain in-memory mailbox "credit-client"  (RestoreNonTerminal on boot)
   ├─ dedup by opKey (durable table + partial UNIQUE index)
   ├─ write credit_operations row (ordinary txn)      ◄── the one durable table
   ├─ lazy-spawn + route Resume to child
   ├─ reap on CreditTerminalNotification
   ├─ arbitrate ConsiderRedeemRequest (in-flight interlock, admit redeem)
   └─ run the auto-redeem boot reconcile (one-shot)
      │  Tell Resume   (the only durable message into the child)
      ▼
OpActor (per op)            durable mailbox "credit-op-<opID>"  (Read/Stage/Commit; protofsm FSM)
   ├─ gRPC → swap server         CreateCredit / ListCredits / RedeemCredit   (key = opKey)
   ├─ DurableTell → OOR registry StartTransfer{IdempotencyKey: opKey, ...}
   ├─ gRPC → swapclientserver    StartPay(invoice, maxCreditSat)             (terminal pay)
   └─ Tell → registry            ConsiderRedeemRequest (after a settled receive)
```

Only the **child** carries a durable inbound mailbox. The registry serializes
admissions on its in-memory mailbox, writes the row in an ordinary transaction
before spawning, and relies on the partial `UNIQUE` index on `op_key` as the hard
dedup backstop. Because the supervisor pre-writes the row, `Resume` is the child's
only application-level message: every admission detail is reloaded from the
durable row, so no `Start*` message is ever serialized into the child.

### 3.2 Messages

The registry mailbox is an ordinary in-memory mailbox, so the messages crossing
it are plain `CreditMsg` values, not TLV-serialized. Only the message that
re-enters the durable child mailbox needs a codec entry.

| Message | Kind | Path |
|---|---|---|
| `StartCreditPayRequest{invoice, maxFeeSat, quote, paymentHash}` | plain `CreditMsg` | walletdk to registry (Ask) |
| `StartCreditReceiveRequest{opKey, amountSat, memo}` | plain `CreditMsg` | walletdk to registry (Ask) |
| `RedeemRequest{opKey, amountSat}` | plain `CreditMsg` | registry-internal (admitted by `considerRedeem`) |
| `ConsiderRedeemRequest{availableSat}` | plain `CreditMsg` | child or boot reconcile to registry (Tell) |
| `CreditTerminalNotification{opKey, terminal}` | plain `CreditMsg` | child to registry (reap) |
| `ListCreditOpsRequest/Response` | plain `CreditMsg` | status RPC |
| `RestoreNonTerminalRequest` | plain `CreditMsg` | boot scan to registry |
| `ResumeCreditOpRequest{opID, fromRetryTimer}` | **durable TLV** (`0x7102`) | registry to child mailbox; boot restore and retry-timer wake |

`ResumeCreditOpRequest` is the only application message the codec registers,
because it is the only one that crosses the child's durable mailbox. The
framework adds `RestartMessage` and `AskResponse` of its own. There is
deliberately no `Start*` or `Drive*` message into the child.

---

## 4. The operation state machine on protofsm

Each operation is a state machine built from `baselib/protofsm`'s types, the same
way `round` builds the boarding machine. The durable per-operation actor drives
it.

### 4.1 What protofsm supplies, and what the actor owns

protofsm supplies the **types**: `State`, `StateTransition`, `EmittedEvent`,
`TransitionTable`. The credit package aliases them in `states.go` and lists every
concrete state (`quotingState`, `topupCreatingState`, and so on, one zero-sized
marker per persisted `state` string). The credit machine does **not** run on
protofsm's `StateMachine` runner, and that is deliberate: the runner chains
internal events within a single turn, which would collapse several steps into one
and defeat the per-step durable checkpoint. Instead the durable actor drives the
states itself, one step per turn, so it can interleave a `Stage` write between two
states.

The driver lives in `op_actor.go` (`runFSM`). One step is:

1. Call the current state's `ProcessEvent`. It runs the state's idempotent side
   effect (a server or daemon call), records the result on the operation record,
   and returns the next state plus a set of **outbox directives**.
2. Mirror the next state onto the record (`applyState`), *then* execute the
   directives.
3. Loop, until a state parks on a poll or reaches a terminal state.

The outbox directives (`events.go`) are how the FSM dictates persistence and
cross-actor effects while the actor keeps owning the lease-fenced writes and the
exactly-once ack:

| Directive | Actor action |
|---|---|
| `stageRecord` | `ax.Stage` a durable checkpoint of the record before the next state runs its effect |
| `parkOp` | stop driving this turn and arm the reconciliation poll timer |
| `triggerRedeem` | after the turn commits, Tell the registry a `ConsiderRedeemRequest` |

Each turn ends with one `ax.Commit` that folds the final record snapshot and the
mailbox ack into a single short transaction. So the transition decides *what* to
persist and signal; the actor decides *how*.

If a resumed row carries a `state` string that does not decode to a known FSM
state, the driver treats it as a corrupt row and drives it to a durable failure,
so it terminal-commits and is reaped rather than being respawned on every boot as
a non-terminal row that can never advance.

### 4.2 Persist-before-effect

The ordering invariant is that a server identifier the *next* effect depends on
is durable before that effect runs. It falls out of the drive loop directly. A
state that mints such an identifier (a top-up destination, a redeem destination)
returns `stageRecord`. The driver mirrors the advanced state onto the record and
then flushes the `Stage`, so the checkpoint persists the *advanced* state with
the *recorded* identifier. Only after the checkpoint commits does the loop run
the next state's effect. A crash before the turn commits therefore re-drives from
the checkpointed state against the same identifier the in-flight effect is bound
to, rather than minting a fresh one the server or chain-watch would never match.

Plain advances that mint no such identifier carry no `stageRecord`; they run
successive effects within one turn and rely on idempotency by op key or payment
hash, persisting only at the turn's `Commit`. A crash there re-drives the same
state and re-issues the same idempotent call.

### 4.3 The transition table

`CreditTransitions` in `transition_table.go` enumerates every state, its outgoing
edges, and the directives each edge emits, mirroring round's
`BoardingClientTransitions`. It documents the machine in one place; the live
dispatch is each state's `ProcessEvent`, maintained alongside the table by hand.

---

## 5. The three flows

### 5.1 Pay

```
quoting → topup_creating → topup_funding → topup_awaiting_credit → paying → pay_awaiting_settlement → completed
   │            │                │                  │                │              │
   └────────────┴────────────────┴──────────────────┴────────────────┴──────────────┴──► failed (terminal, classified)
```

- `quoting`: shortfall and top-up amounts are known from the quote. No top-up
  needed jumps straight to `paying`.
- `topup_creating`: `CreateCredit(ARK_TOPUP, opKey)`, record the
  `serverOperationID` and `destinationPubkey`, then `stageRecord`. The checkpoint
  is what keeps the OOR send from re-creating the credit on resume.
- `topup_funding`: delegated OOR `SendOOR{key: opKey, dest, amt: topupSat}`. A
  crash here re-issues on resume; OOR dedups by `opKey`.
- `topup_awaiting_credit`: arm the poll timer; on each drive re-`ListCredits` and
  check the funding op is `CREDITED`. Gating on `CREDITED` alone (rather than
  `available_sat >= maxCreditSat`, which can be a sentinel max for a
  must-use-credit pay) avoids parking forever; `StartPay` reserves what it needs.
  A deterministic op failure (`EXPIRED` / `FAILED` / `RELEASED`) fails the op.
- `paying`: `StartPay(invoice, maxCreditSat)`, idempotent by payment hash. A
  mixed pay hands terminal authority to the swap monitor and completes on
  hand-off. A credit-only pay advances to `pay_awaiting_settlement` instead.
- `pay_awaiting_settlement` (credit-only): poll-driven `ListCredits`, correlating
  the pay operation by the invoice payment hash. `DEBITED` completes; `RELEASED`
  / `FAILED` / `EXPIRED` fails. This closes the window where a credit-only pay was
  reported complete before the Lightning leg settled.

Every awaiting state honors an optional `MaxAwaitingPolls` cap: an operation the
server never resolves terminal-fails after the cap rather than polling forever.
Zero (the default) relies on the server-reported terminal states to bound the
wait.

### 5.2 Receive

```
receive_creating → awaiting_settlement → completed
                          └──► failed (terminal)
```

The registry creates the server-owned invoice synchronously at admission
(`createReceiveInvoice`), so the pending wallet row carries the invoice back to
the caller and survives restart. The spawned child therefore enters at
`awaiting_settlement`, a poll-driven `ListCredits` for `CREDITED`.
`receive_creating` remains the resume and fallback path; its `CreateCredit` is
idempotent by op key.

When the receive settles, the same step evaluates the auto-redeem watermark
(section 6) and, when it clears, emits `triggerRedeem` on the edge to
`completed`.

### 5.3 Redeem

```
redeem_reserving → redeem_submitting → awaiting_oor → completed
                                            └──► failed (terminal)
```

The reservation is split across two states so the destination checkpoint is
strictly a state boundary, not a mid-step write:

- `redeem_reserving`: allocate a fresh wallet-owned receive script, record the
  destination, then `stageRecord`. A resumed op that already recorded a
  destination skips straight to `redeem_submitting` without re-allocating.
- `redeem_submitting`: `RedeemCredit(opKey, dest)` against the checkpointed
  destination, idempotent by op key. A crash after the call but before the turn
  commits re-drives this state and re-issues the same reservation.
- `awaiting_oor`: poll-driven `FindLiveVTXOByPkScript(dest)` until the redeemed
  vTXO lands, plus `ListCredits` to fail on a server `RELEASED` / `FAILED`. The
  destination is allocated fresh per op, so a live vTXO at it is this
  redemption's payout. The step reconciles the landed amount against the reserved
  amount: it records what actually materialized (so the completed op, and the
  wallet entry projected from it, reflects the value that landed) and logs a
  warning on any divergence. It never terminal-fails on a divergence; the value is
  already home, so failing would mismark a settled redemption.

Without the checkpoint before `RedeemCredit`, a crash between the reservation and
the commit would re-allocate a new script on resume, leaving the server
reservation bound to the first, now-forgotten destination and the chain-watch
looking at the wrong `pkScript`, stranding the redemption forever. The split
makes that checkpoint a durable state rather than an in-step `Stage`.

---

## 6. Auto-redeem: receive trigger plus boot reconcile

Auto-redeem is folded into the receive state machine. The receive FSM signals
intent; the registry owns the decision.

**Receive trigger.** On `awaiting_settlement` reaching `CREDITED`,
`redeemWatermarkCleared` decides whether to redeem:

1. Gate on `AutoRedeemEnabled`.
2. Resolve the threshold: `MinRedeemSat`, or the operator dust limit when zero.
3. Subtract the earmark. The earmark provider is an `atomic.Pointer[EarmarkFunc]`
   shared by the registry and every child, so the daemon can wire it once after
   construction. A nil provider subtracts nothing (safe before any credit-backed
   send has been prepared); an error from the provider redeems nothing.
4. Redeem only when the earmark-adjusted balance strictly exceeds the threshold.
   The subtraction floors at zero, so it cannot underflow.

When it clears, the step emits `triggerRedeem{AvailableSat}`. The actor stashes it
and, *after* the terminal receive snapshot commits, Tells the registry a
`ConsiderRedeemRequest`. A crash in that window leaves the credits available and
safe; nothing is half-applied, and the next receive trigger or the boot reconcile
re-derives the signal. A failed Tell is logged loudly rather than dropped
silently.

**Registry arbitration.** `considerRedeem` applies the in-flight interlock the
receive FSM does not own. It scans non-terminal `credit_operations` rows and
defers when any pay or redeem is in flight: a pending pay may consume the same
credits, and a pending redeem already owns the materialize. A pending receive only
adds credits, so it does not block. When nothing blocks, it admits a fresh
`RedeemRequest` under a random `redeem:<...>` key, which always admits a new
operation rather than deduping against an old one. Because the registry runs on a
single goroutine, the scan and the admit are atomic with respect to every other
admission, so two near-simultaneous signals cannot both pass and double-redeem the
same balance. The admitted amount is the balance the trigger observed, which may
be slightly stale by the time it reserves; that is safe because the server
revalidates the reservation, so an amount the balance no longer supports fails the
redeem cleanly rather than over-materializing.

**Boot reconcile.** A balance already over the watermark at startup is the one
case no receive trigger covers, because no new receive settles to re-evaluate it.
The `autoRedeemer` handles exactly that: when enabled, it runs `reconcile` **once**
at boot (no ticker, no loop), reading the snapshot, subtracting the same shared
earmark, and Telling the registry a `ConsiderRedeemRequest` when the balance
clears the threshold. The interlock then applies as for any other signal.

One liveness gap remains: a mid-session balance increase from a *released* pay
reservation is re-evaluated only at the next receive trigger or restart, not the
moment the blocking pay clears. Closing it causally means routing credit-backed
send reservations through the registry, so reserve, receive-settle, and
redeem-decide all serialize on the one goroutine, at which point the earmark
bridge disappears entirely.

---

## 7. Durability

### 7.1 Schema

```
credit_operations(
  op_id             TEXT NOT NULL PRIMARY KEY, -- durable mailbox id source
  op_key            TEXT NOT NULL,         -- stable idempotency key
  kind              INTEGER NOT NULL,      -- 1=pay | 2=recv | 3=redeem
  state             TEXT NOT NULL,
  status            INTEGER NOT NULL,      -- 0=pending | 1=completed | 2=failed
  server_op_id      TEXT,
  destination_pubkey BLOB,
  oor_session_id    TEXT,
  payment_hash      BLOB,
  invoice           TEXT,
  amount_sat        BIGINT NOT NULL DEFAULT 0,
  topup_sat         BIGINT NOT NULL DEFAULT 0,
  max_credit_sat    BIGINT NOT NULL DEFAULT 0,
  max_fee_sat       BIGINT NOT NULL DEFAULT 0,
  last_error        TEXT,
  snapshot_data     BLOB,                  -- opaque resume blob (TLV)
  snapshot_version  INTEGER NOT NULL DEFAULT 0,
  created_at, updated_at
)
-- partial UNIQUE index on op_key WHERE status != 2 (live-or-completed rows)
```

It lives in the daemon-owned swap DB, via `db/sqlc/queries` and sqlc. Terminal
rows are retained for status and diagnostics. The `state` column holds the
persisted state string; on resume, `decodeCreditState` maps it back to the
typed protofsm state, and an unrecognized string drives the row to a terminal
failure rather than wedging it.

### 7.2 Boot and resume

`darepod`'s `initCreditRegistry` (called right after the OOR actor registers,
guarded by a nil-check on the swap-runtime-populated `cfg.Swap.Credit*`
bridges) builds the store, constructs the registry via `credit.NewRegistry`,
and calls `RestoreNonTerminal` **synchronously** before serving. Each
non-terminal row respawns its child and is told `ResumeCreditOpRequest`, so
it re-drives from persisted state. Retry timers are in-memory and do not survive
restart; they are re-armed on resume. After the restore, the daemon starts the
one-shot auto-redeem boot reconcile (`registry.StartAutoRedeem`) on the root
context.

### 7.3 Crash-recovery walkthrough

| Crash point | Behavior |
|---|---|
| after `CreateCredit`, before OOR | resume re-runs `topup_creating` `CreateCredit(opKey)`; same op and dest |
| after OOR Tell, before server credits | resume re-Tells OOR; **OOR dedups by opKey, no second transfer** |
| server credited, before `StartPay` | resume sees `CREDITED`, advances to `paying` |
| client disconnect mid-wait | irrelevant; the child runs on the daemon root context |
| redeem reserved, before vTXO lands | resume re-drives `awaiting_oor`, reconciles the landed vTXO |
| settled receive committed, before redeem Tell | credits stay available; boot reconcile re-derives the signal |

---

## 8. Open questions

1. **Credit-only pay settlement contract (server).** `pay_awaiting_settlement`
   reconciles a credit-only pay by matching the invoice payment hash against the
   pay operation in `ListCredits` and reading its `DEBITED` / `RELEASED` /
   `FAILED` state. This relies on the swap server surfacing the credit pay
   operation in `ListCredits` keyed by the invoice payment hash. A server that
   does not list the pay op leaves a credit-only pay parked in
   `pay_awaiting_settlement`, bounded only by `MaxAwaitingPolls` if configured.
2. **Receive completion signal.** Is there, or will there be, a server push for
   credit-receive settlement, or is poll-driven `ListCredits` the only option? It
   affects how quickly a pending receive transitions to complete.
3. **Single-authority credit balance.** The remaining auto-redeem liveness gap
   (section 6) closes cleanly by routing credit-backed send reservations through
   the registry, so reserve, receive-settle, and redeem-decide all serialize on
   the one goroutine and the earmark bridge disappears.
