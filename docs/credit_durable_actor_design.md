# Credit Durable-Actor Design

Status: **shipped** (PR #772). The client-side credit orchestration moved off
the synchronous, RPC-context-bound, in-memory path onto a crash-safe durable
execution model. This document weighed **two implementations** — a full
durable-actor subsystem (Version A) and a lighter hybrid that reuses the
existing `sdk/swaps` durability (Version B). What shipped is a trimmed Version
A: a plain-supervisor registry in front of a durable per-operation child.
Section 3 describes the shipped topology; section 5 keeps the full comparison as
the decision record.

---

## 1. Problem statement

PR #772 added server-custodial sat "credits" and routed three multi-step flows
through them. The server ledger is authoritative and guarantees **no fund
loss**, but the *client orchestration* of the multi-step flows is not
fault-tolerant.

### Current fault-tolerance gaps

| Flow | Where it lives | Gap |
|---|---|---|
| Sub-dust / shortfall **pay** (Ark top-up → wait → pay) | `swapwallet/router.go` `fundInvoiceCreditShortfall` / `waitCreditTopUp` | Synchronous, in-memory, **bound to the client RPC context**. Idempotency key = `"credit-topup-"+intent.id` where `intent.id` is a random per-`PrepareSend` value held in an in-memory map with a 5-min TTL. No durable orchestration row, no daemon-side resume. |
| Sub-dust **credit receive** | `swapwallet/recv.go` `recvCredit` | `CreateCredit` with a random `newSendIntentID()` key; pending `WalletEntry` lives only in `runtime.pending` (in-memory). On restart the pending row vanishes (a credit receive has no `sdk/swaps` session to resume). |
| **Redeem** (credits → vTXO) | `sdk/swaps` → `swapclientserver` → server | Fully wired at the raw layer, but a **synchronous one-shot passthrough**: no wait for the redeemed VTXO to land, no persistence, no resume. **Not exposed in walletdk at all**, so over-funded/stranded credits have no wallet-level recovery path. |

### The two concrete bugs that fall out

1. **Double top-up window.** Crash (or client disconnect) after `SendOOR` fires
   but before the server finalizes the credit: on retry the caller
   re-`PrepareSend`s, mints a *new* `intent.id` → *new* idempotency key → a
   *second* `CreateCredit` with a *different* destination → a *second*
   `SendOOR`. Both transfers credit. No loss, but the excess is parked as credit
   balance.
2. **Stranded credits.** The excess from (1) — or any credit balance — can only
   be spent by a *future* credit/mixed send. walletdk exposes no redemption, so
   for a normal wallet user the value is effectively trapped.

### Root cause

The client orchestration holds an **ephemeral idempotency key** and runs as a
**synchronous, cancelable, non-durable** sequence. The fix is two-part and is
**shared by both versions** below:

- **Stable idempotency keys** derived from durable identifiers (payment hash /
  persisted operation id), persisted *before* the first server call.
- **Durable, daemon-owned execution** that survives restart and client
  disconnect, re-driving from persisted state and reconciling against the
  server ledger (`ListCredits`) as the source of truth.

---

## 2. Shared design (applies to both versions)

### 2.1 Three logical operations

Everything reduces to three durable operations with one stable key each:

| Op | Stable key | Completion signal (authoritative) |
|---|---|---|
| `pay` (with optional top-up) | `pay:<paymentHash>` | server ledger shows credits available, then `StartPay` idempotent by payment hash |
| `recv` (credit receive) | `recv:<paymentHash>` | server ledger shows the receive op `CREDITED` |
| `redeem` (credits → vTXO) | `redeem:<clientRedeemID>` | redeemed VTXO lands locally (`FindLiveVTXOByPkScript`) |

The **same key** is passed to the server `CreateCredit`/`RedeemCredit` **and** to
the OOR transfer. On any resume, `CreateCredit(key)` returns the existing op +
same destination, and the OOR transfer dedups — so **at most one OOR transfer
ever exists per operation**, regardless of crash timing. This single change
closes the double-top-up window; durability just guarantees the key is actually
reused.

### 2.2 OOR is delegated, never re-implemented

The top-up (client → server-owned credit destination) and the redeem
(server → client destination) are **OOR transfers**. The existing `oor`
subsystem already runs durable, idempotency-key-deduped, crash-safe OOR
sessions. Both versions delegate the transfer to OOR with `key` as the OOR
`StartTransferRequest.IdempotencyKey`, rather than calling `daemon.SendOOR`
inline.

Elegant consequence: the credit layer does **not** need to track the OOR
session's terminal state for the top-up. The OOR actor owns transfer
crash-safety; the **server ledger** (`ListCredits` → `CREDITED`) is the
completion signal. The credit layer waits on the ledger, not on OOR internals.
(Redeem is the exception: its completion signal is the local VTXO landing,
because the value flows server → client.)

### 2.3 Auto-redeem policy (wallet-owned, never user-facing)

Per the decision that the wallet decides when to redeem and redemption is **not
exposed to the user**, a `creditRedeemPolicy` runs inside the credit subsystem.
It is conservative to avoid churn and to never strand value:

- **Trigger A — over-funded top-up.** When a `pay` op finishes and the server
  reports `available_sat > 0` left over from a top-up that over-shot, schedule a
  `redeem` of the excess back to a fresh wallet vTXO.
- **Trigger B — idle watermark sweep.** A periodic check (timer-driven): if
  `available_sat ≥ dust_limit` **and** no `pay`/`redeem` op is in flight that is
  expected to consume credits, redeem the available balance down. The interlock
  has two parts: the in-flight check on durable `credit_operations` rows, **and**
  an earmark provider wired from the wallet's prepared-send store. A credit-
  backed `PrepareSend` reserves nothing server-side and writes no row until
  `Send`, so the sweep subtracts the credits earmarked by live prepared sends
  before deciding — without it, a sweep could redeem credits the user is about
  to spend, forcing the pending send to re-top-up.
- **Never** auto-redeem below the operator dust limit (it cannot become a vTXO),
  and never redeem reserved balance (`reserved_sat`), only `available_sat`.

**Threshold (decided):** auto-redeem fires once `available_sat` exceeds a
configured `MinAutoRedeemSat`, which **defaults to the operator dust limit**.
That is the smallest amount that can legally become a vTXO, so the default both
recovers stranded value as early as possible and never attempts an impossible
sub-dust redemption. `MinAutoRedeemSat` and `AutoRedeemInterval` are config knobs
with sane defaults; auto-redeem can be disabled entirely for raw/advanced
callers. The redeemed amount is `available_sat` rounded down to a dust-clearing
value (never leaving a sub-dust remainder that would immediately re-trigger).

### 2.4 Error discipline

Following the established durable-actor rule (returning an error from a turn =
retry until dead-letter), the credit layer classifies server responses:

- **Deterministic rejections** (insufficient balance, idempotency-key reuse with
  different params, expired invoice, sub-dust impossible) → **terminal-fail** the
  op with a durable reason (mirrors `sdk/swaps` `failTerminal` /
  `oor` `terminalOutboxError`). Never wedge in redelivery.
- **Transient errors** (network, `Unavailable`) → return the error for the free
  retry / backoff.

### 2.5 walletdk handoff (identical surface for both versions)

`Send`/`Recv` stop doing the multi-step work inline. They hand off and return a
**pending `WalletEntry`** immediately:

- `router.Send`: when the quote rail is `CREDIT`/`MIXED` (or amount < dust),
  hand the prepared invoice + quote to the credit subsystem and return the
  pending entry. The old `fundInvoiceCreditShortfall` / `waitCreditTopUp` is
  deleted.
- `recv.Recv`: when amount < dust and not credit-assisted, hand off a
  `recv` op and return the server invoice + pending entry.
- The runtime monitor loop projects credit-op state transitions onto
  `WalletEntry` exactly as it already does for swaps (the entry id is the stable
  `opKey` / server operation id).

The public RPC shape (`PrepareSend` → preview → `Send`) is unchanged.

---

## 3. Version A (shipped) — Plain supervisor + durable per-op actor

The shipped design is Version A with **one deliberate trim**: the registry is a
**plain (non-durable) supervisor actor**, not a durable one. Durability lives in
exactly two places — the `credit_operations` control-plane table and the
per-operation child's durable mailbox — because that is all the correctness
argument needs. Every external effect is idempotent by the stable op key (or the
invoice payment hash), and the server ledger (`ListCredits`) is the authoritative
completion signal, so durability collapses to "persist the row, then re-drive
non-terminal rows on boot/timer." A second durable mailbox on the registry would
buy at-least-once delivery of the admission request, but the synchronous caller
already retries under the same stable key and the boot scan already finds any
committed row — so it is omitted.

### 3.1 Package + topology

```
walletdk Send/Recv
      │  Ask StartCredit{Pay,Receive}Request          (plain actor Ask; returns pending entry)
      ▼
CreditRegistry              plain in-memory mailbox "credit-client"  (RestoreNonTerminal on boot)
   ├─ dedup by opKey (durable table + partial UNIQUE index)
   ├─ write credit_operations row (ordinary txn)      ◄── the one durable table
   ├─ lazy-spawn + route Resume to child
   ├─ reap on CreditTerminalNotification
   └─ runs creditRedeemPolicy (timer-driven watermark sweep)
      │  Tell Resume   (the only durable message into the child)
      ▼
CreditOpActor (per op)      durable mailbox "credit-op-<opID>"  (Read/Commit; FSM + snapshot)
   ├─ gRPC → swap server         CreateCredit / ListCredits / RedeemCredit   (key = opKey)
   ├─ DurableTell → OOR registry StartTransfer{IdempotencyKey: opKey, ...}
   ├─ gRPC → swapclientserver    StartPay(invoice, maxCreditSat)             (terminal pay)
   └─ Tell → ledger (optional)   accounting, joins commit tx
```

This keeps `oor`'s registry + child split, but only the **child** carries a
durable inbound mailbox (its FSM advance + snapshot are written in one
transaction). The registry is a supervisor: it serializes admissions on its
in-memory mailbox, writes the row in an ordinary transaction before spawning, and
the partial `UNIQUE` index on `op_key` is the hard dedup backstop. Because the
supervisor pre-writes the row, `Resume` is the child's only application-level
message — every admission detail is reloaded from the durable row, so no `Start*`
message is ever serialized.

### 3.2 Messages

The registry mailbox is an ordinary in-memory mailbox, so the messages that
cross it are plain `CreditMsg` values, not TLV-serialized ones. Only the message
that re-enters the durable child mailbox needs a codec entry.

| Message | Kind | Path |
|---|---|---|
| `StartCreditPayRequest{invoice, maxFeeSat, quote, paymentHash}` | plain `CreditMsg` | walletdk → registry (Ask) |
| `StartCreditReceiveRequest{amountSat, memo, paymentHash}` | plain `CreditMsg` | walletdk → registry (Ask) |
| `RedeemRequest{amountSat}` | plain `CreditMsg` | auto-redeem policy → registry (Tell) |
| `CreditTerminalNotification{opKey, terminal}` | plain `CreditMsg` | child → registry (reap) |
| `ListCreditOpsRequest/Response` | plain `CreditMsg` | status RPC |
| `RestoreNonTerminalRequest` | plain `CreditMsg` | boot scan → registry |
| `ResumeCreditOpRequest{opID, fromRetryTimer}` | **durable TLV** (`0x7102`) | registry → child mailbox; boot restore and retry-timer wake |

`ResumeCreditOpRequest` is the only application message the codec registers,
because it is the only one that crosses the child's durable mailbox. The
framework adds two entries of its own: `RestartMessage` (injected as the first
message after a restart) and `AskResponse` (how a `DurableAsk` reply returns).
There is deliberately no `Start*` or `Drive*` message into the child — the
supervisor pre-writes the durable row before spawning, so the child reloads
every admission detail from the row instead of from a redelivered message.

Child config (`CreditOpActorConfig`) carries the swap-server conn, the OOR
registry `TellOnlyRef`, the `swapclientserver` StartPay handle, the
`RegistryStore`, `DeliveryStore`, optional `LedgerSink`, and a `TimeoutActor`
ref for backoff timers — the same shape as `oor.SessionActorConfig`.

### 3.3 Pay FSM

```
Quoting → TopupCreating → TopupFunding → TopupAwaitingCredit → Paying → PayAwaitingSettlement → Completed
   │            │              │                  │              │              │
   └────────────┴──────────────┴──────────────────┴──────────────┴──────────────┴──► Failed (terminal, classified)
```

- `Quoting`: shortfall/topup amounts known from the quote (no top-up needed →
  jump to `Paying`).
- `TopupCreating`: `CreateCredit(ARK_TOPUP, opKey)`, then **`Stage`** the
  `serverOperationID` + `destinationPubkey` durably **before** advancing — the
  persist-before-effect checkpoint that keeps the OOR send from re-creating the
  credit on resume.
- `TopupFunding`: delegated OOR `SendOOR{key: opKey, dest, amt: topupSat}`. Crash
  here → resume re-issues; OOR dedups by `opKey`.
- `TopupAwaitingCredit`: arm a `timeout`-actor backoff timer; on each
  `ResumeCreditOpRequest` re-`ListCredits` and check the funding op is
  `CREDITED`. Gating on `CREDITED` alone (not `available_sat ≥ maxCreditSat`,
  which can be the sentinel max for a must-use-credit pay) avoids parking
  forever; `StartPay` reserves what it needs. Deterministic op failure
  (`EXPIRED`/`FAILED`/`RELEASED`) → `Failed`.
- `Paying`: `StartPay(invoice, maxCreditSat)`, idempotent by payment hash. A
  mixed pay hands terminal authority to the swap monitor and completes on
  hand-off. A credit-only pay advances to `PayAwaitingSettlement` instead of
  declaring success on hand-off.
- `PayAwaitingSettlement` (credit-only): timer-driven `ListCredits`, correlating
  the pay operation by the invoice payment hash. `DEBITED` → `Completed`;
  `RELEASED`/`FAILED`/`EXPIRED` → `Failed`. This closes the window where a
  credit-only pay was reported complete before the Lightning leg settled.

Every awaiting state also honors an optional `MaxAwaitingPolls` cap: an
operation the server never resolves terminal-fails after the cap rather than
polling forever. Zero (the default) relies on the server-reported terminal
states to bound the wait.

### 3.4 Receive FSM

```
ReceiveCreating → AwaitingSettlement → Completed
                        └──► Expired/Failed (terminal)
```

`CreateCredit(LIGHTNING_RECEIVE, opKey)` persisted before the invoice is
returned to walletdk, so the pending wallet row **survives restart**.
`AwaitingSettlement` is timer-driven `ListCredits` polling for `CREDITED` (or a
server-push event if/when available).

### 3.5 Redeem FSM

```
RedeemReserving → AwaitingOOR → Completed
                       └──► Failed (terminal)
```

`RedeemReserving`: allocate a fresh receive script, then **`Stage`** the
destination durably **before** calling `RedeemCredit(opKey, dest)`. This is
load-bearing: without the checkpoint, a crash between the reservation and the
commit would re-allocate a new script on resume, leaving the server reservation
bound to the first (now-forgotten) destination and the chain-watch looking at
the wrong `pkScript` — stranding the redemption forever. `AwaitingOOR`:
timer-driven `FindLiveVTXOByPkScript(dest)` until the redeemed VTXO lands, and
`ListCredits` to terminal-fail on a server `RELEASED`/`FAILED`. Driven by the
auto-redeem policy; no user-facing verb.

### 3.6 Schema

```
credit_operations(
  op_key            TEXT NOT NULL,         -- stable idempotency key
  kind              TEXT NOT NULL,         -- pay | recv | redeem
  state             TEXT NOT NULL,
  server_op_id      TEXT,
  destination_pubkey BLOB,
  oor_session_id    TEXT,
  payment_hash      BLOB,
  invoice           TEXT,
  amount_sat        BIGINT NOT NULL DEFAULT 0,
  topup_sat         BIGINT NOT NULL DEFAULT 0,
  max_credit_sat    BIGINT NOT NULL DEFAULT 0,
  snapshot          BLOB,                  -- opaque resume blob (TLV)
  created_at, updated_at
)
-- partial UNIQUE index on op_key for live-or-completed rows (oor pattern)
```

Lives in the daemon-owned swap DB (alongside the existing swap store), via
`db/queries` + sqlc. Terminal rows retained for status/diagnostics, as `oor`
does.

### 3.7 Boot / resume

`credit.Register(...)` (called by `swapruntime`-tagged `darepod`, next to
`swapclientserver.Register` and the OOR registry) opens the store, wires the
registry actor, and calls `RestoreNonTerminal` **synchronously** before serving:
each non-terminal row respawns its child and is told `ResumeCreditOpRequest` so
it re-drives from persisted state (retry timers are in-memory and do not survive
restart — re-armed on resume, same as `oor`).

### 3.8 Crash-recovery walkthrough

| Crash point | Behavior |
|---|---|
| after `CreateCredit`, before OOR | resume → `TopupCreating` re-calls `CreateCredit(opKey)` → same op + dest |
| after OOR Tell, before server credits | resume → `TopupFunding` re-Tells OOR; **OOR dedups by opKey → no second transfer** |
| server credited, before `StartPay` | resume → `TopupAwaitingCredit` sees `CREDITED` → `Paying` |
| client disconnect mid-wait | irrelevant — child runs on daemon root context |
| redeem OOR in flight at crash | resume → `AwaitingOOR` reconciles the landed VTXO |

---

## 4. Version B — Hybrid (sdk/swaps store + resume workers, OOR via actor)

Same shared design (§2), but the orchestration is **not** a new actor subsystem.
It reuses the durability the pay/receive FSMs already use: the `sdk/swaps`
SQLite store plus the `swapclientserver` resume-worker registry. The OOR
transfer is still delegated to the OOR durable actor.

### 4.1 What changes

- **Stable key + durable row.** Add a `credit_operations` table (or extend the
  existing swap session tables) in the `sdk/swaps` store. `Send`/`Recv` persist
  the op row with a stable `op_key` **before** the first `CreateCredit`. The
  ephemeral `intent.id` key is gone.
- **Orchestration as an FSM in `sdk/swaps`.** Add a small credit-op FSM (the same
  states as §3.3–3.5) driven by the Loop FSM engine the pay/receive sessions
  already use, persisted via the existing `mutateAndPersist` discipline.
- **Resume via the existing worker registry.** `swapclientserver.resumePending`
  already revives persisted pay/receive sessions on boot using `rootCtx` (not the
  RPC context). Extend it to also revive non-terminal credit ops: a credit-op
  worker re-drives from the persisted row and reconciles against `ListCredits`.
- **OOR via the actor.** The top-up/redeem transfer is delegated to the OOR
  registry actor with `op_key` as the idempotency key (same as Version A) — the
  `sdk/swaps` layer already holds an in-process Ark facade and can reach the OOR
  registry ref.
- **Auto-redeem.** Runs as a small periodic task inside the swap runtime
  (alongside the resume sweep), gated by the same policy (§2.3).

### 4.2 walletdk handoff

`Send`/`Recv` call a new `swapclientserver` RPC (e.g. `StartCreditPay` /
`StartCreditReceive`) that persists the op row, starts/reuses a worker, and
returns the pending summary — exactly the shape of the existing
`StartPay`/`StartReceive`. The worker runs on `rootCtx`, so a client disconnect
cannot cancel it.

### 4.3 Why this is lighter

It introduces **no new actor mailbox, no codec, no registry/child split, no
cross-durability straddle for the pay handoff** — the credit op and the terminal
pay live in the *same* `sdk/swaps` store and resume mechanism, so the
"reconcile whether StartPay already happened" boundary collapses to a normal
intra-store FSM transition. The only cross-subsystem hop is the OOR transfer,
which is already a clean durable-actor boundary.

---

## 5. Compare & contrast

| Dimension | Version A (full actor) | Version B (hybrid) |
|---|---|---|
| New infra | New `credit` package: registry + per-op actors, TLV codec, mailbox tables | Extend `sdk/swaps` store + `swapclientserver` workers; no new actor |
| Durability mechanism | `baselib/actor` durable mailbox (dedup, dead-letter, lease, outbox for free) | `sdk/swaps` Store + Loop FSM + resume-worker registry |
| Crash-safety of orchestration | Yes | Yes |
| Stable idempotency key (fixes double top-up) | Yes | Yes |
| OOR transfer reuse | Yes (DurableTell into OOR registry) | Yes (same) |
| Pay-handoff boundary | **Straddles two durability systems** (actor → `sdk/swaps` pay). Reconciled via payment-hash idempotency + `GetSwap`, but it is a real seam | **No straddle** — credit op and pay live in the same store/resume path |
| Cross-actor atomicity | Strong: Tell-into-mailbox joins the commit tx; ledger accounting atomic with op state | Weaker: `sdk/swaps` persists, then issues gRPC `StartPay`; reconciled, not transactional |
| Dead-letter / lease / backpressure | Built in | Hand-rolled (retry counts in the row, like `oor` `MetadataAttempts`) |
| Consistency with codebase direction | Matches the `oor` migration trajectory (darepo#527) | Matches how swaps are durably executed today |
| Code volume | Higher (new subsystem, messages, registry, wiring) | Lower (extend existing tables + workers) |
| Testability | Actor unit tests + `internal/actortest` harness | Existing `swapclientserver` worker test patterns |
| Operational surface | New actor in the system, new mailbox ids, restore path | New worker class in an existing registry |

### Recommendation

For **correctness alone**, both are equivalent — both adopt the shared stable-key
+ durable-resume fix (§2) that actually closes the double-top-up and
stranded-credit bugs. The decision is about *architecture fit*:

- Choose **Version A** if the priority is cross-actor atomicity (credit-op state
  + ledger accounting + OOR handoff all transactional) and long-term consistency
  with the `oor`/`ledger`/`unroll` durable-actor direction. Cost: a new
  subsystem and one genuine straddle at the pay handoff.
- Choose **Version B** if the priority is minimal new surface and avoiding the
  straddle — the credit op and the terminal pay share one store and one resume
  path, which is the simplest correct thing and reuses battle-tested swap
  resume code.

A reasonable sequencing: **ship Version B's shared fix first** (stable keys +
durable rows + worker resume + auto-redeem) to stop the bleeding with minimal
risk, then migrate the orchestration to the Version A actor topology if/when the
broader durable-actor migration (darepo#527) reaches this subsystem. The stable
key and the OOR-delegation boundary are identical in both, so Version B is a
strict subset of the work, not a throwaway.

### Shipped decision

The implementation lands the **per-operation FSM as a durable actor** (the part
that genuinely benefits from the framework's atomic advance-and-ack, resume
scaffolding, and timer-driven redelivery) while keeping the **registry a plain
supervisor** (it owns no durable state worth a second mailbox). This is the
middle of the two versions: it reuses the existing durable-actor infrastructure
for the one place that needs it and reduces the durability surface to a single
table plus a single per-op mailbox — not two stacked durable mailboxes. The
auto-redeem policy and the OOR-delegation boundary are unchanged from the
sketch above.

---

## 6. Open questions

1. **Credit-only pay settlement contract (server).** `PayAwaitingSettlement`
   reconciles a credit-only pay by matching the invoice payment hash against the
   pay operation in `ListCredits`, reading its `DEBITED`/`RELEASED`/`FAILED`
   state. This relies on the swap server surfacing the credit pay operation in
   `ListCredits` with the invoice payment hash and the documented pay-lifecycle
   states (the `CreditOperation` proto already carries `payment_hash`). Confirm
   swapdk-server#134 does so; a server that does not list the pay op leaves a
   credit-only pay parked in `PayAwaitingSettlement` (bounded only by
   `MaxAwaitingPolls`, if configured).
2. **Receive completion signal.** Is there (or will there be) a server push for
   credit-receive settlement, or is timer-driven `ListCredits` polling the only
   option? Affects how snappy the pending→complete transition is.
3. **Credit-assisted receive.** Today this is already durable inside the
   `sdk/swaps` receive FSM (migration 000003). Does it stay there, or also move
   under the credit subsystem for a single status surface? (Recommend: leave it —
   it is already crash-safe and validated.)

The original auto-redeem-interlock open question is now resolved: the sweep
consults both durable `credit_operations` rows and an earmark provider wired
from the wallet's prepared-send store (§2.3).
