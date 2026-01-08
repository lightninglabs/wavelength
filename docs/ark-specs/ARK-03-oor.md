# ARK-03: Out-of-Round Transactions

## Abstract

This document specifies the Out-of-Round (OOR) transaction protocol, also known as Ark Transactions. OOR transactions allow participants to transfer VTXOs without waiting for a new round. The document also specifies the checkpoint transaction mechanism that provides anti-griefing protection for OOR transactions.

## Status

This specification is a working draft.

## Table of Contents

1. [Introduction](#introduction)
2. [Ark Transaction Format](#ark-transaction-format)
3. [Checkpoint Transaction Mechanism](#checkpoint-transaction-mechanism)
4. [OOR Transaction Flow](#oor-transaction-flow)
5. [Cross-Batch Transactions](#cross-batch-transactions)
6. [Preconfirmed VTXO Trust Model](#preconfirmed-vtxo-trust-model)
7. [Operator Obligations](#operator-obligations)
8. [Validation Requirements](#validation-requirements)

## Introduction

### Purpose

Out-of-Round transactions enable instant, off-chain transfers between participants without requiring a new on-chain commitment transaction. This provides:

- **Instant settlement**: Transfers complete in seconds, not waiting for rounds.
- **Reduced on-chain footprint**: Most transfers remain off-chain.
- **Flexible payments**: Support for arbitrary payment amounts and multiple recipients.

### Trade-offs

OOR transactions introduce additional considerations:

- **Preconfirmed VTXOs**: Recipients receive "preconfirmed" VTXOs that depend on the sender not double-spending.
- **Monitoring requirement**: Recipients should monitor the chain or batch-swap promptly.
- **Chain depth**: Long chains of OOR transactions increase unilateral exit complexity.

### Checkpoint Solution

The checkpoint mechanism addresses the griefing attack where a malicious sender could force the operator to broadcast expensive transaction chains. Checkpoints ensure the operator's on-chain costs are bounded regardless of OOR chain length.

## Ark Transaction Format

### Transaction Structure

An Ark transaction spends one or more VTXOs and creates new VTXOs:

```
Ark Transaction:
  Version: 2
  Locktime: 0

  Inputs:
    - Checkpoint output(s) (spent via collaborative keypath)

  Outputs:
    - New VTXO output(s)
    - Fee output to operator (optional)
    - Anchor output (ephemeral, 0 sats)
```

**Note:** Ark transactions spend from checkpoint outputs, not directly from VTXOs. Each VTXO input requires a corresponding checkpoint transaction.

### Input Requirements

#### Checkpoint Inputs

Each Ark transaction input:

1. MUST spend from a checkpoint output.
2. MUST be spent via the collaborative keypath (MuSig2 signature).
3. The checkpoint MUST spend from a valid, unspent VTXO.

#### Cross-Batch Inputs

Ark transactions MAY have inputs from VTXOs in different batches:

- Each input still requires its own checkpoint transaction.
- The checkpoint chain for each input traces back to its origin batch.

### Output Requirements

#### VTXO Outputs

Each output creating a new VTXO:

1. MUST follow the VTXO script structure (see ARK-01).
2. MUST have a valid recipient public key.
3. MUST have positive value.

#### Fee Output

If the Ark transaction includes an explicit fee output:

1. SHOULD pay directly to an operator-controlled address.
2. MAY be unconditional (no timelock or multisig required).
3. The value represents the fee for processing the OOR transaction.

#### Anchor Output

All Ark transactions:

1. MUST include an ephemeral anchor output.
2. The anchor enables fee bumping at broadcast time.

### Value Conservation

The sum of output values MUST NOT exceed the sum of input values:

```
sum(output_values) <= sum(checkpoint_values)
```

The difference represents the implicit fee paid to the operator.

### Ark Transaction Diagram

```mermaid
graph LR
    subgraph "Input Side"
        CP1[Checkpoint 1<br/>from VTXO_A]
        CP2[Checkpoint 2<br/>from VTXO_B]
    end

    subgraph "Ark Transaction"
        ARK[ark_tx]
    end

    subgraph "Output Side"
        V1[VTXO to Bob]
        V2[VTXO change to Alice]
        FEE[Fee to Operator]
        ANCHOR[Anchor 0 sats]
    end

    CP1 --> ARK
    CP2 --> ARK
    ARK --> V1
    ARK --> V2
    ARK --> FEE
    ARK --> ANCHOR
```

## Checkpoint Transaction Mechanism

### Purpose

Checkpoint transactions serve two purposes:

1. **Anti-griefing**: Limit operator's on-chain costs if a malicious participant unrolls.
2. **Atomicity marker**: Provide a clear point where the operator can claim funds.

### Checkpoint Transaction Structure

```
Checkpoint Transaction:
  Version: 2
  Locktime: 0

  Inputs:
    - VTXO input (spent via collaborative keypath)

  Outputs:
    - Checkpoint output
    - Anchor output (ephemeral, 0 sats)
```

### Checkpoint Output Script

The checkpoint output uses a taproot structure (see ARK-01):

- **Internal key**: MuSig2(P_sender, P_operator)
- **Script tree**: Single leaf with operator timeout path

```
Operator Timeout Script:
  <P_o> OP_CHECKSIGVERIFY
  <t_c> OP_CHECKSEQUENCEVERIFY
```

### Checkpoint Properties

1. **Collaborative spend**: The Ark transaction spends via keypath, requiring both sender and operator signatures.
2. **Operator fallback**: If the sender abandons the chain, the operator can claim after `t_c` blocks.
3. **Bounded chain cost**: Each checkpoint can be independently claimed, limiting operator exposure.

### Anti-Griefing Analysis

**Attack scenario without checkpoints:**

1. Alice creates a chain of 100 self-spend Ark transactions.
2. Alice batch-swaps the final VTXO for a new confirmed VTXO.
3. Alice unrolls the original VTXO on-chain.
4. Operator must broadcast all 100 Ark transactions to reach the forfeit.
5. Operator pays fees for 100+ transactions.

**With checkpoints:**

1. Alice creates a chain of 100 Ark transactions, each with a checkpoint.
2. Alice batch-swaps the final VTXO.
3. Alice unrolls the original VTXO on-chain.
4. Operator broadcasts only the first checkpoint transaction.
5. After `t_c` blocks, operator claims the checkpoint via timeout.
6. Operator pays fees for only 2 transactions (VTXO unroll response + checkpoint).

### Checkpoint Chain Diagram

```mermaid
graph TD
    subgraph "Original VTXO"
        V0[VTXO_0<br/>Alice confirmed]
    end

    subgraph "Checkpoint Chain"
        CP1[Checkpoint 1]
        ARK1[ark_1]
        V1[VTXO_1 Alice]

        CP2[Checkpoint 2]
        ARK2[ark_2]
        V2[VTXO_2 Alice]
    end

    V0 --> CP1
    CP1 --> ARK1
    ARK1 --> V1

    V1 --> CP2
    CP2 --> ARK2
    ARK2 --> V2

    subgraph "Attack Response"
        V0 -.->|"Alice unrolls"| ONCHAIN[On-chain]
        CP1 -.->|"Operator broadcasts"| RESPONSE[Response]
        RESPONSE -.->|"Operator claims after t_c"| CLAIM[Operator claims]
    end
```

## OOR Transaction Flow

### Overview

The OOR transaction flow involves multiple message exchanges between sender and operator:

1. Sender constructs checkpoint and Ark transactions.
2. Sender signs the Ark transaction (not the checkpoint).
3. Sender sends both transactions to operator.
4. Operator validates and co-signs both transactions.
5. Sender verifies operator signatures.
6. Sender signs the checkpoint transaction.
7. Sender returns checkpoint signature to operator.
8. Transaction is complete; new VTXOs are live.

### Step 1: Transaction Construction

The sender constructs:

**Checkpoint Transaction:**
- Input: The VTXO(s) being spent.
- Output: Checkpoint output paying to MuSig2(sender, operator).

**Ark Transaction:**
- Input: The checkpoint output(s).
- Outputs: New VTXOs for recipients and change.

### Step 2: Sender Signs Ark Transaction

The sender:

1. Generates MuSig2 nonces for the Ark transaction.
2. Computes partial signature for the Ark transaction.
3. Does NOT sign the checkpoint yet.

**Rationale:** The sender signs the Ark transaction first to commit to the transfer. The checkpoint is signed last to prevent premature commitment.

### Step 3: Submit to Operator

The sender submits to the operator:

- The unsigned checkpoint transaction.
- The Ark transaction with sender's partial signature.
- MuSig2 pubnonces for both transactions.

### Step 4: Operator Validation and Signing

The operator:

1. Validates the checkpoint transaction (see [Validation Requirements](#validation-requirements)).
2. Validates the Ark transaction.
3. Marks input VTXOs as "Spent" (pending confirmation).
4. Generates and returns:
   - MuSig2 partial signature for the checkpoint transaction.
   - MuSig2 partial signature for the Ark transaction.

### Step 5: Sender Verification

The sender:

1. Verifies operator's signatures are valid.
2. Aggregates signatures for the Ark transaction.
3. Verifies the complete Ark transaction signature.

### Step 6: Sender Signs Checkpoint

The sender:

1. Generates partial signature for the checkpoint transaction.
2. Aggregates with operator's partial signature.
3. Returns the complete checkpoint signature (or just sender's partial) to operator.

### Step 7: Completion

The operator:

1. Verifies the checkpoint signature.
2. Stores the signed checkpoint transaction.
3. Marks the new VTXOs as "Live" (preconfirmed).
4. Notifies any registered recipients.

### Flow Diagram

```mermaid
sequenceDiagram
    participant S as Sender
    participant O as Operator
    participant R as Recipient

    Note over S,R: OOR Transaction Flow

    S->>S: Construct checkpoint TX
    S->>S: Construct Ark TX
    S->>S: Sign Ark TX only

    S->>O: Submit checkpoint TX + Ark TX + nonces

    O->>O: Validate transactions
    O->>O: Mark VTXOs as Spent
    O->>O: Sign both transactions

    O->>S: Return operator signatures

    S->>S: Verify operator signatures
    S->>S: Aggregate Ark TX signature
    S->>S: Sign checkpoint TX

    S->>O: Submit checkpoint signature

    O->>O: Verify checkpoint signature
    O->>O: Store signed checkpoint
    O->>O: Mark new VTXOs as Live

    O->>R: Notify of incoming VTXO
```

## Cross-Batch Transactions

### Overview

Ark transactions MAY spend VTXOs from different batches. This provides flexibility but introduces additional complexity.

### Requirements

For cross-batch Ark transactions:

1. Each input VTXO MUST have its own checkpoint transaction.
2. All checkpoints MUST be from batches that have not expired.
3. The Ark transaction spends all checkpoint outputs together.

### Effective Expiry

The effective expiry of a cross-batch Ark transaction is the **minimum** of all input batch expiries:

```
effective_expiry = min(batch_expiry_1, batch_expiry_2, ..., batch_expiry_n)
```

**Rationale:** If any input batch expires, the operator can sweep that input, invalidating the entire Ark transaction chain.

### Unilateral Exit Considerations

To unilaterally exit a VTXO from a cross-batch Ark transaction, the owner must:

1. Broadcast the VTXT path for **each** input VTXO's origin batch.
2. Broadcast all intermediate checkpoint and Ark transactions.
3. Finally spend the output VTXO via unilateral exit.

This increases on-chain cost proportionally to the number of origin batches.

### Cross-Batch Diagram

```mermaid
graph LR
    subgraph "Batch 1 - Expires Block 1000"
        V1[VTXO_A]
    end

    subgraph "Batch 2 - Expires Block 1100"
        V2[VTXO_B]
    end

    subgraph "Cross-Batch Ark TX"
        CP1[Checkpoint A]
        CP2[Checkpoint B]
        ARK[Ark TX]
        V3[New VTXO<br/>Effective Expiry: 1000]
    end

    V1 --> CP1
    V2 --> CP2
    CP1 --> ARK
    CP2 --> ARK
    ARK --> V3
```

## Preconfirmed VTXO Trust Model

### Definition

A **preconfirmed VTXO** is one that results from an OOR transaction rather than directly from a VTXT leaf. The "preconfirmed" status indicates:

1. The VTXO chain is valid and co-signed by the operator.
2. The VTXO is NOT yet backed by an on-chain transaction.
3. The sender could theoretically attempt a double-spend.

### Trust Assumptions

Recipients of preconfirmed VTXOs trust:

1. **Operator honesty**: The operator will not sign conflicting transactions.
2. **Operator availability**: The operator will broadcast checkpoints if the sender attempts double-spend.
3. **Sender reputation**: The sender is not attempting fraud.

### Double-Spend Scenarios

**Scenario 1: Sender unilateral exit**

1. Sender has VTXO_A (confirmed).
2. Sender creates Ark TX, sending to Bob (VTXO_B preconfirmed).
3. Sender broadcasts VTXO_A on-chain via unilateral exit.

**Protection:**
- Operator detects the broadcast.
- Operator broadcasts the checkpoint transaction.
- Checkpoint claims funds before sender's CSV delay expires.
- Bob's VTXO_B is honored (operator makes Bob whole from checkpoint).

**Scenario 2: Operator collusion**

1. Sender and operator collude.
2. Sender creates Ark TX to Bob.
3. Operator signs but also signs a conflicting transaction.

**Detection:**
- If both transactions appear on-chain, cryptographic evidence of double-signing exists.
- This proves operator misbehavior (two valid signatures on conflicting transactions).
- Reputation damage to operator; potential legal consequences.

### Risk Mitigation for Recipients

Recipients SHOULD:

1. **Batch swap promptly**: Convert preconfirmed VTXOs to confirmed VTXOs.
2. **Monitor the chain**: Watch for unilateral exits of input VTXOs.
3. **Limit preconfirmed exposure**: Cap total value held in preconfirmed VTXOs.

Recipients MAY:

1. Require sender reputation or identity.
2. Wait for sender's batch to reach deep confirmations.
3. Request a batch swap as part of the payment flow.

### Preconfirmed Chain Depth

As preconfirmed VTXOs are spent in subsequent Ark transactions, chains grow deeper:

```
VTXO_0 (confirmed)
  └─> ark_1 -> VTXO_1 (preconfirmed, depth 1)
       └─> ark_2 -> VTXO_2 (preconfirmed, depth 2)
            └─> ark_3 -> VTXO_3 (preconfirmed, depth 3)
```

Deeper chains:
- Require more transactions for unilateral exit.
- Increase potential on-chain fees.
- MAY be limited by operator policy.

## Operator Obligations

### Immediate Obligations

After signing an OOR transaction, the operator MUST:

1. **Persist state**: Store the signed checkpoint transaction.
2. **Update VTXO states**: Mark input VTXOs as Spent, outputs as Live.
3. **Notify recipients**: Inform registered watchers of new VTXOs.

### Ongoing Obligations

The operator MUST:

1. **Monitor inputs**: Watch for unilateral exits of VTXOs spent in OOR transactions.
2. **Respond to attacks**: Broadcast checkpoints when double-spend attempts are detected.
3. **Maintain availability**: Be available to co-sign future transactions.

### Response Timing

When a spent VTXO is broadcast on-chain:

1. The operator MUST detect this within a reasonable time (RECOMMENDED: < 1 hour).
2. The operator MUST broadcast the checkpoint before the VTXO's CSV delay expires.
3. The operator SHOULD use fee bumping to ensure timely confirmation.

### State Cleanup

The operator MAY delete OOR state after:

1. The origin batch has been swept (expired and operator claimed).
2. All VTXOs in the chain have been batch-swapped to new confirmed VTXOs.
3. Sufficient time has passed with no activity.

## Validation Requirements

### Checkpoint Transaction Validation

The operator MUST validate:

1. **Input VTXO validity**: The VTXO exists and is unspent in operator's records.
2. **Input VTXO ownership**: The sender proves ownership via valid signature.
3. **VTXO not locked**: The VTXO is not locked by a pending round or other operation.
4. **VTXO not expired**: The VTXO's batch has not expired.
5. **Script correctness**: The checkpoint output script matches expected format.
6. **Operator key**: The operator key in the checkpoint matches current signing key.

### Ark Transaction Validation

The operator MUST validate:

1. **Input validity**: All checkpoint inputs are valid.
2. **Value conservation**: Output sum <= input sum.
3. **VTXO format**: All output VTXOs follow correct script format.
4. **Signature validity**: The sender's partial signature is valid.
5. **Chain depth**: (Optional) The resulting chain depth is within policy limits.

### Policy Limits

Operators MAY enforce policy limits:

| Policy | Description | Example |
|--------|-------------|---------|
| Max chain depth | Limit OOR chain length | 10 transactions |
| Max cross-batch inputs | Limit inputs from different batches | 3 batches |
| Min fee rate | Minimum fee for OOR processing | 1 sat/vbyte |
| Max VTXO count | Limit outputs per Ark transaction | 10 VTXOs |

Policy violations SHOULD be rejected with appropriate error codes.

## References

1. ARK-00: Protocol Overview and Terminology
2. ARK-01: Transaction Formats and Script Specifications
3. BIP 327: MuSig2 - https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
