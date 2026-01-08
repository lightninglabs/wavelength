# ARK-02: Round Lifecycle Protocol

## Abstract

This document specifies the round lifecycle protocol for constructing, signing, and broadcasting Ark commitment transactions. A round aggregates multiple participant requests (boarding, leaving, batch swaps) into a single commitment transaction with associated VTXT structures.

## Status

This specification is a working draft.

## Table of Contents

1. [Introduction](#introduction)
2. [Round Overview](#round-overview)
3. [Phase 0: Request Collection](#phase-0-request-collection)
4. [Phase 1: Construction](#phase-1-construction)
5. [Phase 2: VTXT Signing](#phase-2-vtxt-signing)
6. [Phase 3: Input Signing](#phase-3-input-signing)
7. [Phase 4: Broadcast](#phase-4-broadcast)
8. [Error Handling](#error-handling)
9. [State Transitions](#state-transitions)
10. [Restart Safety](#restart-safety)

## Introduction

The round lifecycle is the core protocol for creating new VTXOs. The operator coordinates multiple participants through a multi-phase process that ensures atomicity and allows participants to verify their outputs before committing.

### Round Frequency

Operators MAY configure round frequency based on their operational requirements. Common approaches include:

- **Time-based**: Start a new round every N minutes.
- **Request-based**: Start a new round when sufficient requests accumulate.
- **Hybrid**: Start when either condition is met.

The round frequency affects:
- User experience (latency to obtain new VTXOs)
- On-chain footprint (fewer rounds = fewer commitment transactions)
- Operator liquidity requirements (longer rounds may accumulate more value)

## Round Overview

A round proceeds through five phases:

```mermaid
stateDiagram-v2
    [*] --> RequestCollection: Round Opens

    RequestCollection --> Construction: Collection Period Ends

    Construction --> VTXTSigning: Commitment TX Built

    VTXTSigning --> InputSigning: VTXT Signatures Complete

    InputSigning --> Broadcast: All Inputs Signed

    Broadcast --> [*]: Transaction Confirmed

    RequestCollection --> [*]: Round Aborted
    Construction --> [*]: Round Aborted
    VTXTSigning --> [*]: Round Aborted
    InputSigning --> [*]: Round Aborted
```

| Phase | Purpose | Participants |
|-------|---------|--------------|
| Request Collection | Gather participant requests | All |
| Construction | Build commitment TX and VTXT | Operator |
| VTXT Signing | Sign virtual transaction tree | All with VTXOs |
| Input Signing | Sign commitment TX inputs | Boarding participants |
| Broadcast | Publish and confirm | Operator |

## Phase 0: Request Collection

### Overview

During request collection, the operator accepts requests from participants. Each request type results in specific inputs or outputs in the commitment transaction.

### Request Types

#### Boarding Request

A boarding request enters the Ark by spending an on-chain UTXO.

**Request Contents:**
- `boarding_outpoint`: The TXID and output index of the boarding UTXO
- `boarding_script`: The full script of the boarding output
- `requested_vtxos`: List of VTXO specifications (pubkey, value)
- `session_pubkey`: Ephemeral key for VTXT signing sessions
- `proof_of_ownership`: Signature proving control of the boarding key

**Operator Validation:**
1. The boarding UTXO MUST exist and be unspent.
2. The boarding UTXO MUST have sufficient confirmations (operator-defined minimum).
3. The boarding script MUST match the expected format (see ARK-01).
4. The operator key in the script MUST match the operator's current key.
5. The requested VTXO values MUST NOT exceed the boarding value minus fees.
6. The proof of ownership MUST be a valid signature.

#### Leave Request

A leave request exits the Ark by forfeiting a VTXO for an on-chain UTXO.

**Request Contents:**
- `vtxo_reference`: Reference to the VTXO being forfeited
- `vtxo_proof`: Proof of VTXO ownership and validity
- `destination_script`: The script to pay the leave output to
- `destination_amount`: The amount for the leave output

**Operator Validation:**
1. The VTXO reference MUST point to a valid, unspent VTXO.
2. The VTXO MUST NOT be locked by another pending operation.
3. The VTXO MUST NOT be from an expired batch.
4. The destination amount MUST NOT exceed VTXO value minus fees.
5. The proof MUST demonstrate ownership of the VTXO.

#### Batch Swap Request

A batch swap refreshes expiring VTXOs by forfeiting them for new VTXOs.

**Request Contents:**
- `vtxo_references`: List of VTXOs being forfeited
- `vtxo_proofs`: Proofs of ownership for each VTXO
- `requested_vtxos`: List of new VTXO specifications
- `session_pubkey`: Ephemeral key for VTXT signing sessions

**Operator Validation:**
1. All VTXO references MUST point to valid, unspent VTXOs.
2. No VTXO MUST be locked by another pending operation.
3. No VTXO MUST be from an expired batch.
4. The total requested value MUST NOT exceed total forfeited value minus fees.
5. All proofs MUST demonstrate ownership.

### VTXO Locking

When a request is accepted, the affected VTXOs MUST be locked:

- **Lock scope**: The VTXO cannot be used for OOR transactions or other round requests.
- **Lock duration**: Until the round completes (success or failure).
- **Lock storage**: MAY be in-memory during early phases; MUST be persisted after signing begins.

### Request Aggregation

The operator aggregates requests based on operational constraints:

- Maximum participants per round
- Maximum VTXT tree depth
- Maximum transaction size
- Liquidity availability

Requests that cannot be included MUST be rejected with appropriate error codes.

### Request Window

The request collection phase ends when:

- The configured collection period expires, OR
- The operator decides to proceed with current requests

The operator MUST NOT accept new requests after the collection phase ends.

## Phase 1: Construction

### Overview

The operator constructs the unsigned commitment transaction and VTXT structures based on collected requests.

### Step 1: VTXO Grouping

Group all VTXO requests (from boarding and batch swap) into trees:

1. Assign each VTXO request to a batch.
2. For each batch, organize VTXOs into a balanced tree structure.
3. The tree radix SHOULD be configurable (default: 2).

**Algorithm:**
```
function GroupVTXOs(vtxos, radix):
    // Sort VTXOs (optional, for determinism)
    sorted_vtxos = Sort(vtxos)

    // Build balanced tree bottom-up
    current_level = sorted_vtxos
    while len(current_level) > 1:
        next_level = []
        for i = 0; i < len(current_level); i += radix:
            group = current_level[i:i+radix]
            next_level.append(CreateBranch(group))
        current_level = next_level

    return current_level[0]  // Root
```

### Step 2: VTXT Construction

For each batch, construct the VTXT bottom-up:

1. **Leaf Level**: Create leaf transactions with VTXO outputs.
2. **Branch Levels**: Create branch transactions aggregating child outputs.
3. **Root Level**: The final output becomes the batch output.

**For each VTXT node:**
1. Compute the aggregated public key (all downstream owners + operator).
2. Compute the script tree (operator sweep path).
3. Derive the taproot output key.
4. Create the transaction spending from parent and producing the output.

**Note:** TXIDs are not yet known since transactions are unsigned.

### Step 3: Connector Tree Construction

If any forfeits (leave or batch swap) are included:

1. Count the number of forfeit transactions needed.
2. Build a connector tree with that many leaves.
3. The connector tree radix MAY differ from the VTXT radix.

**Connector tree structure:**
- Root: Single output in commitment transaction
- Branches: Intermediate transactions (if needed)
- Leaves: Individual connector outputs for forfeit transactions

### Step 4: Commitment Transaction Assembly

Assemble the commitment transaction template:

**Inputs:**
1. Boarding inputs (from boarding requests)
2. Operator wallet inputs (for liquidity)
3. Expired batch sweep inputs (if any)

**Outputs:**
1. Batch outputs (one per VTXT root)
2. Connector output (if forfeits exist)
3. Leave outputs (one per leave request)
4. Change output (if needed)

### Step 5: TXID Propagation

Once the commitment transaction template is complete:

1. Compute the commitment transaction TXID.
2. Update VTXT root transactions to reference this TXID.
3. Traverse the VTXT top-down, updating each transaction's inputs.
4. Update connector tree transactions similarly.

After this step, all transactions have valid input references (but no signatures).

### Step 6: Distribution to Participants

Send each participant their relevant transaction data:

**For boarding participants:**
- Full commitment transaction
- VTXT path from root to their VTXO(s)
- Connector tree path (if doing batch swap in same request)

**For leave request participants:**
- Full commitment transaction
- Connector tree path to their connector leaf
- Forfeit transaction template

**For batch swap participants:**
- Full commitment transaction
- VTXT path to their new VTXO(s)
- Connector tree path to their connector leaf
- Forfeit transaction template(s)

### Mermaid Diagram: Construction Flow

```mermaid
sequenceDiagram
    participant P as Participants
    participant O as Operator

    Note over O: Phase 1: Construction

    O->>O: Group VTXOs into trees
    O->>O: Build VTXT structures
    O->>O: Build connector tree
    O->>O: Assemble commitment TX
    O->>O: Compute TXIDs, update references

    O->>P: Send relevant TX data
    P->>P: Verify TX data
```

## Phase 2: VTXT Signing

### Overview

Participants and operator collaboratively sign the VTXT transactions using MuSig2. This phase ensures all participants have valid, signed paths to their VTXOs before committing inputs.

### MuSig2 Signing Protocol

For each VTXT transaction, signing proceeds as follows:

1. **Nonce Generation**: Each signer generates fresh nonces.
2. **Nonce Exchange**: Signers exchange public nonces.
3. **Nonce Aggregation**: Aggregate all public nonces.
4. **Partial Signing**: Each signer produces a partial signature.
5. **Signature Aggregation**: Combine partial signatures into final signature.

### Step 1: Client Nonce Submission

Each participant generates and submits nonces for their VTXT path:

**For each transaction in participant's VTXT path:**
1. Generate fresh random nonce pair (R1, R2) per BIP-327.
2. Compute public nonce (pubnonce).
3. Submit pubnonce to operator.

**Requirements:**
- Nonces MUST be generated with fresh randomness.
- Nonces MUST NOT be reused across signing sessions.
- Participants MUST store secret nonces securely until signing completes.

### Step 2: Operator Nonce Aggregation

The operator collects all nonces and aggregates:

**For each VTXT transaction:**
1. Collect pubnonces from all required signers.
2. Include operator's own pubnonce.
3. Compute aggregate pubnonce per BIP-327.
4. Distribute aggregate pubnonces to participants.

### Step 3: Partial Signature Generation

Each participant produces partial signatures:

**For each transaction in participant's VTXT path:**
1. Receive aggregate pubnonce from operator.
2. Verify aggregate pubnonce is correctly formed.
3. Compute partial signature using secret nonce and signing key.
4. Submit partial signature to operator.

### Step 4: Signature Aggregation and Distribution

The operator aggregates and distributes final signatures:

**For each VTXT transaction:**
1. Collect partial signatures from all required signers.
2. Add operator's partial signature.
3. Aggregate into final Schnorr signature per BIP-327.
4. Verify the final signature is valid.
5. Distribute final signatures to relevant participants.

### Step 5: Client Verification

Each participant verifies their complete VTXT path:

1. Verify each transaction in the path has a valid signature.
2. Verify the transaction chain from commitment TX to VTXO is complete.
3. Verify the VTXO output matches requested parameters.

**If verification fails:**
- Participant MUST NOT proceed to input signing.
- Participant SHOULD report the failure to operator.
- Participant MAY retry in a future round.

### Mermaid Diagram: VTXT Signing Flow

```mermaid
sequenceDiagram
    participant P1 as Participant 1
    participant P2 as Participant 2
    participant O as Operator

    Note over P1,O: Phase 2: VTXT Signing

    P1->>O: Submit nonces for vtx_7, vtx_5, vtx_1
    P2->>O: Submit nonces for vtx_7, vtx_6, vtx_2

    O->>O: Aggregate nonces per transaction

    O->>P1: Aggregate nonces for vtx_7, vtx_5, vtx_1
    O->>P2: Aggregate nonces for vtx_7, vtx_6, vtx_2

    P1->>O: Partial sigs for vtx_7, vtx_5, vtx_1
    P2->>O: Partial sigs for vtx_7, vtx_6, vtx_2

    O->>O: Aggregate signatures

    O->>P1: Final sigs for vtx_7, vtx_5, vtx_1
    O->>P2: Final sigs for vtx_7, vtx_6, vtx_2

    P1->>P1: Verify complete VTXT path
    P2->>P2: Verify complete VTXT path
```

## Phase 3: Input Signing

### Overview

After VTXT signing, participants sign their inputs to the commitment transaction. This phase commits participants to the round.

### Boarding Input Signing

Participants with boarding requests sign the commitment transaction inputs:

1. Verify the complete VTXT path is signed (from Phase 2).
2. Verify the commitment transaction includes expected outputs.
3. Generate MuSig2 signature for the boarding input.
4. Submit signature to operator.

**The participant MUST verify before signing:**
- All requested VTXOs are present with correct values.
- The VTXO scripts match expected format.
- The VTXT path is complete and valid.

### Forfeit Transaction Signing

Participants with leave or batch swap requests sign forfeit transactions:

1. Verify the commitment transaction includes expected outputs.
   - For leave: verify leave output with correct script and value.
   - For batch swap: verify new VTXOs (via VTXT path from Phase 2).
2. Verify the connector path is valid.
3. Construct the forfeit transaction.
4. Generate MuSig2 signature for the VTXO input of the forfeit.
5. Submit the complete signed forfeit transaction to operator.

**The forfeit transaction:**
- Spends the forfeited VTXO via collaborative keypath.
- Spends a connector output from the new commitment transaction.
- Pays the operator.

### Operator Signature Completion

The operator completes signatures:

1. Collect all boarding input signatures.
2. Collect all forfeit transactions.
3. Sign operator's wallet inputs.
4. Sign connector inputs for each forfeit transaction.
5. Assemble the fully signed commitment transaction.

### Mermaid Diagram: Input Signing Flow

```mermaid
sequenceDiagram
    participant BP as Boarding Participant
    participant BSP as Batch Swap Participant
    participant O as Operator

    Note over BP,O: Phase 3: Input Signing

    BP->>BP: Verify VTXT path complete
    BP->>O: Submit boarding input signature

    BSP->>BSP: Verify new VTXO path complete
    BSP->>BSP: Construct and sign forfeit TX
    BSP->>O: Submit signed forfeit TX

    O->>O: Sign operator inputs
    O->>O: Sign connector inputs for forfeits
    O->>O: Assemble fully signed commitment TX
```

## Phase 4: Broadcast

### Overview

The operator broadcasts the commitment transaction and monitors for confirmation.

### Transaction Broadcast

1. Verify the commitment transaction is fully signed.
2. Broadcast to the Bitcoin network.
3. Monitor for inclusion in a block.

### Confirmation Requirements

The operator SHOULD wait for a minimum confirmation depth before marking VTXOs as live:

- **Minimum confirmations**: Operator-defined (RECOMMENDED: 1-6 blocks)
- **Deep confirmations**: For high-value batches, consider more confirmations

### VTXO Activation

Once the commitment transaction reaches minimum confirmations:

1. Mark all new VTXOs from this batch as "Live".
2. Remove locks on forfeited VTXOs (they are now spent).
3. Notify participants of successful round completion.

### Failure Handling

If the commitment transaction fails to confirm within a timeout:

1. Attempt fee bumping via CPFP on anchor outputs.
2. Continue retrying until confirmed or explicitly abandoned.
3. If abandoned, release VTXO locks and notify participants.

### Mermaid Diagram: Broadcast Flow

```mermaid
sequenceDiagram
    participant O as Operator
    participant BN as Bitcoin Network
    participant P as Participants

    Note over O,P: Phase 4: Broadcast

    O->>BN: Broadcast commitment TX

    loop Until Confirmed
        O->>BN: Check for confirmation
        alt Not Confirmed
            O->>BN: Fee bump if needed
        else Confirmed
            O->>O: Update VTXO states
            O->>P: Notify completion
        end
    end
```

## Error Handling

### Round Abort Conditions

A round MAY be aborted during:

| Phase | Abort Condition | Resolution |
|-------|-----------------|------------|
| Request Collection | No valid requests | Normal termination |
| Construction | Invalid requests detected | Exclude invalid, retry |
| VTXT Signing | Participant timeout | Exclude participant, retry |
| VTXT Signing | Invalid nonce/signature | Exclude participant, retry |
| Input Signing | Participant refuses to sign | Exclude participant, retry |
| Broadcast | Persistent confirmation failure | Fee bump or abandon |

### Participant Exclusion

When a participant is excluded:

1. Remove their requests from the round.
2. Release any VTXO locks.
3. Rebuild the commitment transaction without them.
4. Restart from Construction phase.

### Retry Limits

Operators SHOULD implement retry limits:

- Maximum retries per round
- Maximum participants to exclude before abandoning
- Timeout for entire round

## State Transitions

### Round States

```mermaid
stateDiagram-v2
    [*] --> Collecting

    Collecting --> Building: Timer/Threshold
    Collecting --> Abandoned: No Requests

    Building --> Signing: TX Built
    Building --> Collecting: Build Failed

    Signing --> InputSigning: VTXT Signed
    Signing --> Building: Signing Failed

    InputSigning --> Broadcasting: Inputs Signed
    InputSigning --> Building: Signing Failed

    Broadcasting --> Confirmed: TX Confirmed
    Broadcasting --> Broadcasting: Fee Bump
    Broadcasting --> Abandoned: Timeout

    Confirmed --> [*]
    Abandoned --> [*]
```

### VTXO State Transitions

```mermaid
stateDiagram-v2
    [*] --> Pending: Created in round

    Pending --> Live: Round confirmed
    Pending --> [*]: Round aborted

    Live --> Locked: Request received
    Locked --> Live: Request canceled
    Locked --> Forfeit: Round confirmed
    Locked --> Spent: OOR confirmed

    Live --> Spent: OOR confirmed

    Forfeit --> [*]: Cleanup after expiry
    Spent --> [*]: Cleanup after expiry

    Live --> Expired: Batch expired
    Expired --> [*]: Swept
```

## Restart Safety

### Critical Persistence Points

The operator MUST persist state at these points:

1. **After VTXT signing begins**: Nonces and partial signatures.
2. **After commitment transaction is fully signed**: The complete signed transaction.
3. **After broadcast**: Transaction broadcast status.

### Recovery Procedures

#### Restart During Request Collection

- Resume collecting requests.
- Requests are idempotent; participants may resubmit.

#### Restart During Construction

- Restart construction from collected requests.
- Previously computed structures may be discarded.

#### Restart During Signing

- If nonces were distributed, MUST NOT restart signing with same nonces.
- Either complete signing with stored state or abort round.

#### Restart After Signing Complete

- The signed transaction MUST be broadcast.
- Continue monitoring for confirmation.
- Never abandon a fully signed transaction without explicit double-spend.

### Double-Spend Protection

If a fully signed commitment transaction exists:

1. The operator MUST assume it may have been broadcast.
2. The operator MUST continue trying to confirm it.
3. The operator MUST NOT sign conflicting transactions.
4. Only after explicitly double-spending an input may the operator abandon.

## References

1. BIP 327: MuSig2 for BIP340-compatible Multi-Signatures - https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
