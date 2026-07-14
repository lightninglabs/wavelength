# Fee + change-output model

This document explains how the seal-time fee handshake (issue #270)
distributes the operator fee across a client's outputs in a round, and
how each protocol flow expresses *which* output absorbs the residual.

It is the source-of-truth narrative for the implementation in:

- [`client/round/transitions.go`](../round/transitions.go) —
  `designateChangeMarker`, `validateQuoteEchoes`, `evaluateQuote`.
- `rounds/seal_time_fee_builder.go` (server/lumos repo) —
  `resolveChangeDesignation`, `computeSealTimeQuotes`,
  `quoteForClient`.
- [`client/rpc/roundpb/round.proto`](../rpc/roundpb/round.proto) —
  `VTXORequest`, `LeaveRequest`, `JoinRoundQuote`, `QuoteReason`.

If anything here drifts from those files, the code wins and this doc
is wrong.

## TL;DR

1. The client submits an *intent* (a list of VTXO outputs + on-chain
   leave outputs). One — and only one — of those outputs is the
   **change output** that absorbs the round's operator fee.
2. The server is the fee authority. It computes the operator fee at
   seal time using live chain rate, treasury utilization, and real
   batch size, then stamps the residual onto the change output.
3. The client picks which output is change in one of two ways:
   - **Implicit change**: the intent has a single output. No
     `is_change` bit needed; the lone output absorbs the fee by
     contract.
   - **Explicit change**: the intent has multiple outputs. Exactly
     one `VTXORequest` or `LeaveRequest` carries `is_change=true`.
     The server stamps the residual on that one and echoes the
     intent's `target_amount_sat` for every other output.
4. If the client forgets to designate a change output, the FSM
   normalizes the intent at the
   `PendingRoundAssembly → IntentSentState` boundary via
   `designateChangeMarker`. The wire intent is therefore *always*
   well-formed by the time it leaves the client.

## Why a change output exists at all

The operator fee is `Σ(input amounts) − Σ(output amounts)`. To
balance the books, exactly one output must accept "whatever's left
after the fixed targets and the operator fee". That's the change
output.

Pre-#270, the client guessed the operator fee at submit time and
hard-coded all output amounts. The server then validated
`Σin − Σout == operator_fee` and rejected the round if the guess was
wrong — a thin contract that broke whenever the chain rate or
treasury utilization moved between intent build and seal.

Under #270 the client only fixes the amounts it actually cares about
(directed-send recipients, leave amounts, refresh leg sizes). The
change output's `target_amount_sat` is a placeholder; the server
overwrites it at seal time with the real residual.

## Proto contract

```protobuf
message VTXORequest {
    int64  target_amount_sat = 1;  // hint for non-change outputs
    bool   is_change         = 4;  // designates this as the residual slot
    bool   fixed_amount      = 5;  // disables the single-output implicit-change exception
    // ... policy template + signing key omitted ...
}

message LeaveRequest {
    int64  target_amount_sat = 2;  // same semantics as VTXORequest
    bool   is_change         = 3;  // same semantics as VTXORequest
    // ... destination address omitted ...
}

enum QuoteReason {
    QUOTE_OK                    = 0;
    INSUFFICIENT_RESIDUAL       = 1;  // change would go below dust / negative
    INVALID_CHANGE_DESIGNATION  = 2;  // 0 or >1 is_change markers in a multi-output intent
}

message VTXOQuote {
    bytes pk_script    = 1;  // server echoes the intent's pkScript
    int64 amount_sat   = 2;  // server-decided amount
    bytes recipient_key = 3; // server echoes the MuSig2 signing key
}

message LeaveQuote {
    bytes pk_script  = 1;
    int64 amount_sat = 2;
}

message JoinRoundQuote {
    string             round_id        = 1;
    bytes              quote_id        = 2;  // hash(round_id || seal_pass_number || client_id)
    uint32             seal_pass_number = 3;
    repeated VTXOQuote vtxo_quotes     = 4;
    repeated LeaveQuote leave_quotes   = 5;
    int64              operator_fee_sat = 6;
    FeeBreakdown       breakdown       = 7;
    int64              quote_expires_at = 8;
    QuoteReason        reject_reason   = 9;  // when != QUOTE_OK, *_quotes are empty
}
```

Two invariants hold across both `VTXORequest` and `LeaveRequest`:

- `target_amount_sat` is honored verbatim in the quote when
  `is_change=false`.
- `target_amount_sat` is overwritten with
  `Σin − Σ(fixed targets) − operator_fee_sat` when `is_change=true`.

The server cross-checks both above invariants and the per-output
`pk_script` / `recipient_key` echoes via `validateQuoteEchoes` on the
client side.

## Designation rules (`designateChangeMarker`)

The client FSM normalizes the intent's `is_change` bits before the
wire intent leaves the actor. Rules apply in order:

1. **One marker already set** (the explicit-change happy path) →
   leave it alone. This preserves explicit wallet decisions:
   boarding self-change in `handleBoard` / `handleTriggerBoard` and
   directed-send self-change in `handleSendVTXOs`.
2. **Two or more markers set** (defensive — should not happen) →
   keep the first marker, clear the rest. VTXO markers win over
   leave markers when both have one.
3. **No markers set, multi-output intent** → stamp the first VTXO.
   When the intent has only leaves (cooperative leave-only batch),
   stamp the first leave.
4. **No markers set, single-output intent** → no marker stamped.
   The server treats the lone output as implicit change.

The historical bug this fixed: each entry-point handler
(`buildVTXORequestFromRefresh`, `handleRefreshVTXOs`,
`handleLeaveVTXOs`, ...) used to stamp `is_change=true` itself, so
N expiring VTXOs in one round produced N markers. Centralizing the
rule at the FSM seam means each entry-point handler is silent on
`is_change` and the assembler enforces the "exactly one" invariant
once.

## Server-side enforcement (`resolveChangeDesignation`)

`rounds/seal_time_fee_builder.go::resolveChangeDesignation` is the
mirror of `designateChangeMarker` on the server. It accepts the
client's intent verbatim and decides:

- `totalOutputs == 1` → implicit change (single output is change).
- `totalOutputs > 1` and exactly one `is_change=true` → explicit
  change at the stamped slot.
- Any other shape → return `QuoteReasonInvalidChangeDesignation`,
  which the FSM surfaces as a `JoinRoundQuote` with empty
  `vtxo_quotes` / `leave_quotes` and that reject reason. The client
  drops the intent into `ClientFailedState`.

## Client-side echo validation (`validateQuoteEchoes`)

`client/round/transitions.go::validateQuoteEchoes` cross-checks
every quote field against the intent before the FSM accepts the
quote:

- `len(quote.VTXOQuotes) == len(intent.VTXOs)` and same for leaves
  (positional parity).
- `entry.PkScript == EffectivePkScript(intent.VTXOs[i])` — the
  server's pkScript echo must equal the intent's derived pkScript.
- `entry.RecipientKey == SerializeCompressed(intent.VTXOs[i].SigningKey)` —
  the server's signing-key echo must equal the intent's signing key.
- For non-change outputs (and only when the total output count is
  greater than one), `entry.AmountSat == intent.VTXOs[i].Amount`.
  The change output is exempt from amount equality because the
  server fills it.
- The implicit-change case (`totalOutputs == 1`) skips the
  amount-equality check entirely; the lone output is server-stamped
  by definition — unless that lone `VTXORequest` sets
  `fixed_amount=true`, which disables the exception and re-enables
  the amount check (a fixed-amount single output must carry its own
  change leg to pay fees).

A mismatch fails the FSM with a `QuoteRejected` event and emits a
`JoinRoundReject` outbox echoing the `quote_id`.

## Scenario catalogue

The 11 scenarios below cover every combination of boarding, refresh,
leave, and directed send the round protocol supports.

For each scenario the table reads:

- `vtxoReqs[]` — the client's `VTXORequest` slice.
- `leaveReqs[]` — the client's `LeaveRequest` slice.
- `IsChange` — the wire-level `is_change=true` slot after
  `designateChangeMarker` has run.
- `Server stamps` — what the `JoinRoundQuote` carries for that
  output.
- `Failure modes` — quote rejection reasons that apply.

### 1. Boarding only (single output)

The simplest case: client funds one VTXO from a confirmed boarding
UTXO and keeps the entire residual.

| Slot              | Intent            | Wire             | Quote            |
|-------------------|-------------------|------------------|------------------|
| `vtxoReqs[0]`     | self, target=Σin  | `is_change=false` (implicit) | `amount = Σin − fee` |
| `leaveReqs`       | empty             | —                | —                |

- `target_amount_sat` is a hint only; server overwrites to residual.
- `designateChangeMarker` stamps nothing (single-output intent).
- Failure mode: `INSUFFICIENT_RESIDUAL` if `Σin < fee + dust`.

### 2. Boarding fan-out (N self-VTXOs)

Boarding into multiple receive scripts (e.g., split-by-purpose
wallet UX). One of the VTXOs absorbs the fee.

| Slot              | Intent                   | Wire             | Quote                      |
|-------------------|--------------------------|------------------|----------------------------|
| `vtxoReqs[0]`     | self, target=A           | `is_change=true` (auto-stamped) | `amount = Σin − Σ(B...) − fee` |
| `vtxoReqs[1..N-1]`| self, target=B...        | `is_change=false`| `amount = target` (echoed) |
| `leaveReqs`       | empty                    | —                | —                          |

- If the wallet wants a *specific* slot to be change (e.g., the
  largest), it sets `is_change=true` on that slot itself; the
  designator respects it. Otherwise `vtxoReqs[0]` is auto-stamped.
- Failure mode: `INSUFFICIENT_RESIDUAL` if the fixed targets eat
  all the input.

### 3. Boarding directed-send (recipient absorbs fee)

Client sends to an external recipient *and* the recipient pays the
fee — Lightning's "subtract fee from amount" semantic. The client
opts in by marking the recipient's `VTXORequest` as change.

| Slot              | Intent                            | Wire             | Quote                      |
|-------------------|-----------------------------------|------------------|----------------------------|
| `vtxoReqs[0]`     | recipient pubkey, target=Σin      | `is_change=true` (explicit) | `amount = Σin − fee`       |
| `leaveReqs`       | empty                             | —                | —                          |

- Wallet opt-in is required because the default is "fee from sender's
  change", not "fee from recipient amount".
- CLI surfaces this as a "subtract fee from amount" flag (see CLI
  mapping below).

### 4. Boarding mixed (self + directed send + change)

Boarding split across a self-VTXO, an external recipient, and a
self-change slot.

| Slot              | Intent                         | Wire             | Quote                      |
|-------------------|--------------------------------|------------------|----------------------------|
| `vtxoReqs[0]`     | self, target=A                 | `is_change=false`| `amount = A`               |
| `vtxoReqs[1]`     | recipient pubkey, target=B     | `is_change=false`| `amount = B`               |
| `vtxoReqs[2]`     | self, target=C (change)        | `is_change=true` (explicit) | `amount = Σin − A − B − fee` |
| `leaveReqs`       | empty                          | —                | —                          |

- The wallet must explicitly mark `vtxoReqs[2].IsChange = true` —
  the designator only auto-stamps `vtxoReqs[0]`, which would be
  wrong here.
- Failure mode: `INSUFFICIENT_RESIDUAL` if `Σin − A − B < fee + dust`.

### 5. Refresh single (single VTXO refreshed in place)

Standard expiry-driven refresh: one input VTXO, one output VTXO,
implicit change.

| Slot              | Intent                            | Wire             | Quote                      |
|-------------------|-----------------------------------|------------------|----------------------------|
| `vtxoReqs[0]`     | self, target=vtxo.Amount          | `is_change=false` (implicit) | `amount = vtxo.Amount − fee` |
| `leaveReqs`       | empty                             | —                | —                          |

- `buildVTXORequestFromRefresh` no longer pre-quotes the fee — the
  server fills the amount at seal time.
- Failure mode: `INSUFFICIENT_RESIDUAL` if the input is too small
  to cover the live forfeit-path fee.

### 6. Refresh multi (N VTXOs refreshed in one round)

Multiple expiring VTXOs refreshed together. Auto-stamped first VTXO
absorbs the fee.

| Slot              | Intent                       | Wire             | Quote                                  |
|-------------------|------------------------------|------------------|----------------------------------------|
| `vtxoReqs[0]`     | self, target=v0.Amount       | `is_change=true` (auto-stamped) | `amount = Σ(v_i) − Σ(v_1...v_N-1) − fee` |
| `vtxoReqs[1..N-1]`| self, target=v_i.Amount      | `is_change=false`| `amount = v_i.Amount`                  |
| `leaveReqs`       | empty                        | —                | —                                      |

- Equivalent net result: one output gets a smaller amount equal to
  `v_0.Amount − fee` (the others are echoed verbatim).
- This is the case the bug-fix #270 unrooted: the client used to
  stamp `is_change=true` on every refresh leg, producing N markers.

### 7. Refresh fan-out (one VTXO into N outputs)

Wallet splits an expiring VTXO into multiple receive scripts during
refresh.

| Slot              | Intent                          | Wire             | Quote                       |
|-------------------|---------------------------------|------------------|-----------------------------|
| `vtxoReqs[0]`     | self, target=A                  | `is_change=true` (auto-stamped) | `amount = vtxo.Amount − Σ(B,C...) − fee` |
| `vtxoReqs[1..N-1]`| self/recipient, target=B,C,...  | `is_change=false`| `amount = target`           |
| `leaveReqs`       | empty                           | —                | —                           |

- If the wallet wants the *last* slot (or a specific one) to be
  change, it sets `is_change=true` there; the designator respects
  the explicit choice.

### 8. Leave alone (cooperative exit, single output)

Single LeaveRequest taking a VTXO on-chain. Implicit change.

| Slot              | Intent                                  | Wire             | Quote                       |
|-------------------|-----------------------------------------|------------------|-----------------------------|
| `vtxoReqs`        | empty                                   | —                | —                           |
| `leaveReqs[0]`    | on-chain output, target=vtxo.Amount     | `is_change=false` (implicit) | `amount = vtxo.Amount − fee` |

- Single-output intent: no marker stamped, server treats as
  implicit change.
- The on-chain output value is `Σin − fee`.

### 9. Leave + VTXO mix

Wallet partially exits some funds on-chain while keeping some
off-chain. Either the leave OR the new VTXO can be change; the
wallet decides.

| Slot              | Intent                                  | Wire             | Quote                       |
|-------------------|-----------------------------------------|------------------|-----------------------------|
| `vtxoReqs[0]`     | self, target=A                          | `is_change=false`| `amount = A`                |
| `leaveReqs[0]`    | on-chain, target=B (change)             | `is_change=true` (explicit) | `amount = Σin − A − fee`    |

- Equivalent (different policy): mark `vtxoReqs[0].IsChange = true`
  if the wallet wants the off-chain side to absorb the fee instead.
- Failure mode: `INSUFFICIENT_RESIDUAL` if `Σin − A < fee + dust`.

### 10. Directed send within round (recipient + self-change)

In-round directed send. Recipient amount is fixed; sender's
self-change slot absorbs the fee. This is the canonical "send X to
Bob, fee from my balance" case.

| Slot              | Intent                            | Wire             | Quote                       |
|-------------------|-----------------------------------|------------------|-----------------------------|
| `vtxoReqs[0]`     | recipient, target=B               | `is_change=false`| `amount = B`                |
| `vtxoReqs[1]`     | self, target=Σin − B (change)     | `is_change=true` (explicit) | `amount = Σin − B − fee`    |
| `leaveReqs`       | empty                             | —                | —                           |

- The wallet sets `vtxoReqs[1].IsChange = true` explicitly.
- Failure mode: `INSUFFICIENT_RESIDUAL` if `Σin − B < fee + dust`.

### 11. Directed send-as-change (subtract-fee-from-amount)

Recipient eats the fee. Same wire shape as scenario 3 but in a
forfeit-driven round (refresh + send) rather than a boarding round.

| Slot              | Intent                            | Wire             | Quote                       |
|-------------------|-----------------------------------|------------------|-----------------------------|
| `vtxoReqs[0]`     | recipient, target=Σin (change)    | `is_change=true` (explicit) | `amount = Σin − fee`        |
| `leaveReqs`       | empty                             | —                | —                           |

- The CLI presents this as "send X to Bob, fee out of X" — Bob
  receives `X − fee`.
- The recipient sees a smaller VTXO than the sender's intent
  target. This is a deliberate UX choice; wallets that want to
  guarantee a specific received amount must build scenario 10
  instead.

## Failure modes

| `QuoteReason`                  | Trigger                                                                                       |
|--------------------------------|-----------------------------------------------------------------------------------------------|
| `QUOTE_OK`                     | Quote is binding; `vtxo_quotes` / `leave_quotes` populated.                                    |
| `INSUFFICIENT_RESIDUAL`        | After applying fixed targets and seal-time fee, the change output would be `< dust` or `< 0`. |
| `INVALID_CHANGE_DESIGNATION`   | Multi-output intent with 0 or ≥2 `is_change=true` markers.                                    |

The client treats any non-`QUOTE_OK` reason as terminal for that
intent in the current pass. The FSM emits a `JoinRoundReject` echo
of the `quote_id` and transitions to `ClientFailedState`.

Reseal handling is orthogonal: the server may reseal up to
`MaxSealPasses` (default 3) times across surviving accepted clients.
A client that holds an older `quote_id` after a reseal walks back
to `QuoteReceivedState` to evaluate the new quote (see
[`round/CLAUDE.md`](../round/CLAUDE.md)).

In addition to the wire-level reasons, the client's local cap fires
before any server interaction:

- `quote.OperatorFeeSat > env.MaxOperatorFee` →
  `JoinRoundReject` with reason `"operator fee X exceeds cap Y"`.
- `quote.QuoteExpiresAt <= now` (when non-zero) →
  `JoinRoundReject` with reason `"quote expired at T (now=N)"`.

## CLI mapping

`wavecli` flags map onto the `is_change` semantics. Names below use
the current command tree; flags marked *(future)* aren't wired up yet
and only describe how the mapping is intended to land.

- `wavecli ark send oor --to <addr> --amount <sat>` —
  **scenario 10**. Sender pays the fee from a self-change VTXO.
- `wavecli ark send oor --to <addr> --amount <sat> --subtract-fee`
  *(future)* — **scenarios 3 / 11**. Recipient absorbs the fee;
  the recipient's `VTXORequest` is marked `is_change=true`.
- `wavecli ark vtxos refresh --all` — **scenario 6**. All expiring
  VTXOs are submitted; auto-stamped first VTXO absorbs the fee.
- `wavecli ark vtxos refresh --outpoint <op>` — **scenario 5**.
  Implicit change on the single-output intent.
- `wavecli ark vtxos leave --outpoint <op> --address <addr>` —
  **scenario 8** when a single VTXO covers the leave; **scenario 9**
  with a `--keep <sat>` flag *(future)* that adds a self-VTXO leg.
- `wavecli ark board` — **scenario 1** for a single receive script;
  **scenario 2** when `--target-vtxo-count N` is used.

The CLI prints "estimated ~X sats; actual fee confirmed when the
round seals" before submitting an intent, since `EstimateFee` and
the auto-refresh fee quoter are now advisory previews — not
binding amounts.
