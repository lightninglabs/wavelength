# ARK-04: Monitoring and Fraud Response

## Abstract

This document specifies the operator's monitoring and fraud response requirements. It defines the VTXO state machine, describes batch output monitoring procedures, specifies fraud response protocols, and covers batch expiry handling.

## Status

This specification is a working draft.

## Table of Contents

1. [Introduction](#introduction)
2. [VTXO State Machine](#vtxo-state-machine)
3. [Batch Output Monitoring](#batch-output-monitoring)
4. [Fraud Response Protocol](#fraud-response-protocol)
5. [Batch Expiry and Sweeping](#batch-expiry-and-sweeping)
6. [Timing Requirements](#timing-requirements)
7. [Implementation Considerations](#implementation-considerations)

## Introduction

### Operator Responsibilities

The operator is responsible for:

1. **Monitoring**: Detecting when participants broadcast transactions on-chain.
2. **Fraud Response**: Broadcasting appropriate transactions when fraud is detected.
3. **Sweeping**: Reclaiming funds from expired batches.

Failure to perform these duties may result in:
- Loss of funds if forfeited/spent VTXOs are not reclaimed.
- Reduced liquidity if expired batches are not swept.
- Protocol security degradation.

### Timing Criticality

Many operator responses are time-sensitive:

- **Forfeit response**: Must broadcast before the VTXO's CSV delay expires.
- **Checkpoint response**: Must broadcast before the checkpoint's CSV delay expires.
- **Sweep**: Can only occur after batch expiry is reached.

Operators MUST maintain sufficient monitoring and response infrastructure to meet these timing requirements.

## VTXO State Machine

### States

```mermaid
stateDiagram-v2
    [*] --> Pending: Created in round

    Pending --> Live: Round confirmed
    Pending --> Void: Round aborted

    Live --> Locked: Request received
    Live --> Spent: OOR transaction
    Live --> Unrolled: Unilateral exit

    Locked --> Live: Request canceled
    Locked --> Forfeit: Forfeit signed
    Locked --> Spent: OOR transaction

    Spent --> Unrolled: Double-spend attempt
    Forfeit --> Reclaimed: Forfeit broadcast

    Live --> Expired: Batch expired
    Spent --> Expired: Batch expired
    Forfeit --> Expired: Batch expired

    Expired --> Swept: Operator sweep
    Reclaimed --> [*]: Cleanup
    Unrolled --> [*]: Cleanup
    Swept --> [*]: Cleanup
    Void --> [*]: Cleanup
```

### State Descriptions

| State | Description | Operator Action Required |
|-------|-------------|-------------------------|
| **Pending** | Created in unsigned/unconfirmed round | Wait for confirmation |
| **Void** | Round was aborted before confirmation | Cleanup |
| **Live** | Active, can be spent via OOR or forfeit | Monitor |
| **Locked** | Reserved for pending round operation | Complete operation |
| **Spent** | Spent via OOR transaction | Store checkpoint, monitor |
| **Forfeit** | Forfeit transaction signed | Store forfeit, monitor |
| **Unrolled** | Broadcast on-chain by owner | None (legitimate exit) |
| **Reclaimed** | Forfeit transaction broadcast | Await confirmation |
| **Expired** | Batch absolute expiry reached | Sweep |
| **Swept** | Funds recovered via sweep | Cleanup |

### Transition Rules

#### Pending → Live

**Trigger:** The commitment transaction containing the VTXO is confirmed to minimum depth.

**Actions:**
1. Mark VTXO as Live.
2. Index VTXO for monitoring.
3. Notify registered watchers.

#### Pending → Void

**Trigger:** The round is aborted before broadcast.

**Actions:**
1. Mark VTXO as Void.
2. Remove from pending index.
3. No cleanup required.

#### Live → Locked

**Trigger:** VTXO is included in a round request (leave or batch swap).

**Actions:**
1. Mark VTXO as Locked.
2. Reject OOR requests for this VTXO.
3. Store lock reference to pending round.

#### Locked → Live

**Trigger:** The pending round is aborted.

**Actions:**
1. Mark VTXO as Live.
2. Resume accepting OOR requests.
3. Clear lock reference.

#### Live/Locked → Spent

**Trigger:** OOR transaction is completed (checkpoint signatures received).

**Actions:**
1. Mark VTXO as Spent.
2. Store signed checkpoint transaction.
3. Continue monitoring for double-spend.

#### Locked → Forfeit

**Trigger:** Forfeit transaction is signed and round completes.

**Actions:**
1. Mark VTXO as Forfeit.
2. Store signed forfeit transaction.
3. Continue monitoring for unilateral exit.

#### Spent/Forfeit → Unrolled (Fraud Detection)

**Trigger:** The VTXO output appears on-chain.

**Actions:**
1. Detect the spend type (VTXO unilateral exit).
2. Initiate fraud response (see [Fraud Response Protocol](#fraud-response-protocol)).
3. Mark as Unrolled after response complete.

#### Live → Unrolled

**Trigger:** The VTXO output appears on-chain (legitimate exit).

**Actions:**
1. Mark VTXO as Unrolled.
2. No fraud response needed.
3. Continue monitoring upstream tree nodes.

#### Forfeit → Reclaimed

**Trigger:** Forfeit transaction is broadcast and confirming.

**Actions:**
1. Mark VTXO as Reclaimed.
2. Track forfeit transaction confirmation.
3. Cleanup after sufficient confirmations.

#### Any → Expired

**Trigger:** The batch absolute expiry is reached.

**Actions:**
1. Mark all remaining Live/Spent/Forfeit VTXOs as Expired.
2. Add to sweep candidate list.
3. Disable OOR transactions for this batch.

#### Expired → Swept

**Trigger:** Sweep transaction is broadcast and confirmed.

**Actions:**
1. Mark as Swept.
2. Cleanup state.
3. Return liquidity to operator wallet.

## Batch Output Monitoring

### Monitoring Scope

The operator MUST monitor:

1. **Batch outputs**: Top-level outputs of commitment transactions.
2. **VTXT node outputs**: Any VTXT branch that makes it on-chain.
3. **Checkpoint outputs**: Outputs from checkpoint transactions.

### Detection Methods

#### Blockchain Subscription

Operators SHOULD subscribe to relevant address/output notifications:

1. Register commitment transaction outpoints.
2. Monitor for spends of those outpoints.
3. When spent, register child outpoints and repeat.

#### Polling

As fallback, operators MAY poll:

1. Query UTXOs for known outputs periodically.
2. Detect spent outputs by absence.
3. Query transaction history to find spending transaction.

### Spend Classification

When a monitored output is spent, classify the spend:

```mermaid
flowchart TD
    SPEND[Output Spent] --> CHECK{Is it a VTXO?}

    CHECK -->|No| VTXT[VTXT Branch Node]
    CHECK -->|Yes| VTXO[VTXO Output]

    VTXT --> VPATH{Spend path?}
    VPATH -->|Collaborative| COLLAB[Expected VTXT unroll]
    VPATH -->|Sweep| SWEEP[Batch expired sweep]

    VTXO --> VSTATE{VTXO State?}
    VSTATE -->|Live| LEGIT[Legitimate unilateral exit]
    VSTATE -->|Spent| FRAUD_SPENT[Checkpoint fraud response]
    VSTATE -->|Forfeit| FRAUD_FORFEIT[Forfeit fraud response]

    COLLAB --> REGISTER[Register child outputs]
    REGISTER --> MONITOR[Continue monitoring]
```

### Monitoring State

For each active batch, maintain:

```
BatchMonitorState:
  batch_id: bytes
  commitment_txid: bytes
  expiry_height: uint32

  batch_outputs: [
    {
      outpoint: (txid, index)
      status: (unspent | spent_collaborative | spent_sweep)
      child_outpoints: [(txid, index), ...]
    }
  ]

  vtxt_nodes: [
    {
      outpoint: (txid, index)
      level: uint8
      participant_keys: [pubkey, ...]
      status: (unspent | spent_collaborative | spent_sweep)
    }
  ]

  vtxos: [
    {
      outpoint: (txid, index)
      owner_key: pubkey
      state: vtxo_state
      checkpoint_tx: bytes (if spent)
      forfeit_tx: bytes (if forfeit)
    }
  ]
```

## Fraud Response Protocol

### Fraud Types

| Type | Description | Response |
|------|-------------|----------|
| **Spent VTXO unrolled** | Owner broadcasts VTXO that was spent via OOR | Broadcast checkpoint |
| **Forfeit VTXO unrolled** | Owner broadcasts VTXO that was forfeited | Broadcast forfeit |

### Response to Spent VTXO Unroll

When a Spent VTXO appears on-chain:

1. **Retrieve checkpoint chain**: Get all checkpoints from this VTXO to the current tip.
2. **Broadcast first checkpoint**: The checkpoint spending this VTXO.
3. **Monitor checkpoint confirmation**: Track the checkpoint transaction.
4. **Handle continuation**: If the spender continues (broadcasts Ark TX), continue broadcasting checkpoints.
5. **Claim timeout**: After `t_c` blocks, claim checkpoint via timeout path if unchallenged.

```mermaid
sequenceDiagram
    participant M as Malicious User
    participant O as Operator
    participant BC as Blockchain

    M->>BC: Broadcast VTXO unilateral exit

    O->>O: Detect VTXO spend on-chain
    O->>O: Lookup: VTXO is in Spent state
    O->>O: Retrieve signed checkpoint TX

    O->>BC: Broadcast checkpoint TX

    alt User Continues Attack
        M->>BC: Broadcast Ark TX (spends checkpoint)
        O->>O: Detect checkpoint spent
        O->>BC: Broadcast next checkpoint
        Note over O,BC: Repeat until chain exhausted
    else User Abandons
        Note over BC: Wait t_c blocks
        O->>BC: Claim checkpoint via timeout
    end
```

### Response to Forfeit VTXO Unroll

When a Forfeit VTXO appears on-chain:

1. **Retrieve forfeit transaction**: Get the signed forfeit.
2. **Verify connector**: Ensure the connector output exists (commitment TX confirmed).
3. **Broadcast forfeit**: Submit the forfeit transaction.
4. **Monitor confirmation**: Track forfeit confirmation.
5. **Fee bump if needed**: Use anchor output for CPFP.

```mermaid
sequenceDiagram
    participant M as Malicious User
    participant O as Operator
    participant BC as Blockchain

    M->>BC: Broadcast VTXO unilateral exit

    O->>O: Detect VTXO spend on-chain
    O->>O: Lookup: VTXO is in Forfeit state
    O->>O: Retrieve signed forfeit TX
    O->>O: Verify connector output exists

    O->>BC: Broadcast forfeit TX

    alt Confirmation Delayed
        O->>BC: Fee bump via anchor CPFP
    end

    BC->>O: Forfeit confirmed
    O->>O: Mark VTXO as Reclaimed
```

### Response Timing

The operator MUST broadcast response transactions before the CSV delay expires:

```
time_remaining = csv_delay - (current_height - vtxo_broadcast_height)

if time_remaining < safety_margin:
    // CRITICAL: Broadcast immediately with aggressive fee
```

**Safety margin**: RECOMMENDED minimum 6 blocks before CSV expiry.

### Fee Bumping

When response transactions are not confirming:

1. **Initial fee**: Use current mempool-appropriate fee rate.
2. **Bump threshold**: If unconfirmed after N blocks, bump fee.
3. **Bump strategy**: Increase fee by percentage or match next block target.
4. **Maximum fee**: Cap at reasonable percentage of output value.

## Batch Expiry and Sweeping

### Expiry Detection

Monitor for batch expiry:

```
for each active_batch:
    if current_height >= batch.expiry_height:
        mark_batch_expired(batch)
        add_to_sweep_candidates(batch)
```

### Pre-Expiry Actions

Before batch expiry, the operator SHOULD:

1. **Notify participants**: Warn of upcoming expiry.
2. **Encourage batch swaps**: Promote VTXO refresh.
3. **Prepare sweep**: Pre-compute sweep transaction structure.

### Sweep Transaction Construction

The sweep transaction claims all operator-recoverable funds:

```
Sweep Transaction:
  Version: 2
  Locktime: batch_expiry_height

  Inputs:
    - Unspent batch outputs (via sweep path)
    - Unspent VTXT nodes (via sweep path)
    - Confirmed forfeit outputs
    - Confirmed checkpoint outputs (via timeout)

  Outputs:
    - Operator wallet output
    - (Optional) Anchor for fee bumping
```

### Sweep Scenarios

#### Clean Sweep

No unilateral exits occurred:

1. Single input: The batch output.
2. Operator signs via sweep script path.
3. One transaction sweeps entire batch.

```mermaid
graph LR
    BO[Batch Output] --> SWEEP[Sweep TX]
    SWEEP --> WALLET[Operator Wallet]
```

#### Partial Unroll Sweep

Some VTXT branches were broadcast:

1. Multiple inputs: Unspent branch outputs.
2. Each input requires sweep path signature.
3. One transaction sweeps all unspent outputs.

```mermaid
graph TD
    subgraph "After Partial Unroll"
        BO[Batch Output<br/>SPENT]
        B1[Branch 1<br/>UNSPENT]
        B2[Branch 2<br/>UNSPENT]
        V1[VTXO 1<br/>claimed by user]
        V2[VTXO 2<br/>claimed by user]
    end

    subgraph "Sweep"
        SWEEP[Sweep TX]
        WALLET[Operator Wallet]
    end

    B1 --> SWEEP
    B2 --> SWEEP
    SWEEP --> WALLET
```

#### Sweep with Forfeit Outputs

Forfeits were broadcast during batch lifetime:

1. Include forfeit transaction outputs.
2. These are already operator-owned.
3. Can be swept immediately (no timelock).

### Sweep Batching

Operators MAY batch sweeps across multiple expired batches:

- Reduces total on-chain footprint.
- May need to wait for all batches to expire.
- Balance between efficiency and liquidity return.

## Timing Requirements

### Critical Deadlines

| Event | Deadline | Consequence of Miss |
|-------|----------|---------------------|
| Forfeit broadcast | VTXO CSV expiry | Loss of forfeited funds |
| Checkpoint broadcast | VTXO CSV expiry | Loss of spent funds |
| Checkpoint claim | Checkpoint CSV expiry | User can reclaim |
| Sweep | None (after expiry) | Delayed liquidity return |

### Recommended Timing Parameters

| Parameter | Recommended Value | Notes |
|-----------|------------------|-------|
| Monitoring poll interval | 1 block | Real-time detection |
| Response safety margin | 6 blocks | Buffer before CSV expiry |
| Fee bump trigger | 2 blocks unconfirmed | Ensure timely confirmation |
| Sweep delay after expiry | 1 block | Ensure expiry is final |

### Timing Diagram

```mermaid
gantt
    title VTXO Fraud Response Timeline
    dateFormat X
    axisFormat %s

    section Detection
    VTXO broadcast on-chain :a1, 0, 1

    section Response Window
    CSV delay period :a2, 1, 144

    section Critical Actions
    Detect broadcast :crit, a3, 1, 6
    Broadcast response :crit, a4, 6, 12
    Fee bump if needed :a5, 60, 90
    Response confirmed :milestone, 90, 90

    section Danger Zone
    Safety margin :crit, a6, 138, 144
```

## Implementation Considerations

### Database Requirements

Operators MUST persist:

1. **VTXO states**: Current state of all tracked VTXOs.
2. **Checkpoint transactions**: All signed checkpoints for spent VTXOs.
3. **Forfeit transactions**: All signed forfeits for forfeited VTXOs.
4. **Batch metadata**: Expiry heights, tree structures.

### High Availability

For production deployments:

1. **Redundant monitoring**: Multiple nodes watching the chain.
2. **Alert systems**: Immediate notification of detected events.
3. **Automated response**: Scripted fraud response for speed.
4. **Manual override**: Ability to intervene in edge cases.

### Recovery Procedures

After operator downtime:

1. **Scan for events**: Check all monitored outputs since last known state.
2. **Process pending responses**: Handle any fraud that occurred during downtime.
3. **Verify no losses**: Confirm all CSVs were respected.
4. **Resume normal operation**: Continue monitoring.

### Resource Considerations

Monitoring costs scale with:

- Number of active batches
- Number of VTXOs per batch
- OOR transaction volume
- Blockchain event rate

Operators SHOULD provision resources accordingly.

## References

1. ARK-00: Protocol Overview and Terminology
2. ARK-01: Transaction Formats and Script Specifications
3. ARK-03: Out-of-Round Transactions

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
