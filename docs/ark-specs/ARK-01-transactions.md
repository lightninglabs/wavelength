# ARK-01: Transaction Formats and Script Specifications

## Abstract

This document specifies the transaction formats and Bitcoin Script structures used in the Ark protocol. It defines the taproot-based output scripts for VTXOs, VTXT nodes, connector outputs, boarding outputs, and checkpoint outputs. It also specifies the structure of batch transactions, forfeit transactions, and anchor outputs.

## Status

This specification is a working draft.

## Table of Contents

1. [Introduction](#introduction)
2. [General Requirements](#general-requirements)
3. [Key Separation: Session Keys vs VTXO Keys](#key-separation-session-keys-vs-vtxo-keys)
4. [VTXO Output Script](#vtxo-output-script)
5. [VTXT Node Output Script](#vtxt-node-output-script)
6. [Connector Output Script](#connector-output-script)
7. [Boarding Output Script](#boarding-output-script)
8. [Checkpoint Output Script](#checkpoint-output-script)
9. [Batch Transaction](#commitment-transaction)
10. [Forfeit Transaction](#forfeit-transaction)
11. [Ark Transaction (OOR Transaction)](#ark-transaction-oor-transaction)
12. [Anchor Outputs](#anchor-outputs)
13. [Transaction Validation Rules](#transaction-validation-rules)

## Introduction

All Ark protocol outputs use Taproot (BIP-341) [[1]](#references). The collaborative spend mechanism varies by output type:

- **VTXT branch nodes**: Use MuSig2 (BIP-327) [[2]](#references) keypath spends for efficiency (single 64-byte signature) and privacy.
- **VTXO outputs, boarding outputs, connector leaves, checkpoint outputs**: Use script-path multi-sig (not MuSig2) with an unspendable internal key. This avoids interactive nonce exchange for leaf outputs.

### Design Rationale

VTXT branch node transactions use MuSig2 keypaths because:
1. **Smaller witness**: Keypath spends require only a single 64-byte signature.
2. **Privacy**: Keypath spends reveal no script information.
3. **Efficiency for mass signing**: Branch nodes are signed during rounds where all participants are already interacting.

Leaf-level outputs (VTXOs, boarding, connectors, checkpoints) use script-path multi-sig because:
1. **No interactive nonce exchange**: Signatures can be provided independently without coordination.
2. **Simpler key management**: Each party signs with their own key without MuSig2 session state.
3. **Operational flexibility**: The VTXO owner can sign their portion at any time.

## General Requirements

### Taproot Key Derivation

All taproot outputs in the Ark protocol MUST be constructed as follows:

1. Compute the internal key according to the specific output type.
2. Compute the taproot tweak from the script tree (if any).
3. Derive the output key as specified in BIP-341.

### MuSig2 Usage

For all MuSig2 aggregated keys:

1. Key aggregation MUST follow BIP-327 [[2]](#references).
2. Nonce generation MUST use fresh randomness for each signing session.
3. Implementations MUST NOT reuse nonces across signing sessions.

### Timelock Encoding

- Absolute timelocks (CLTV) MUST be encoded as block heights, not timestamps.
- Relative timelocks (CSV) MUST be encoded as block counts using the sequence number format specified in BIP-68 [[3]](#references).

## Key Separation: Session Keys vs VTXO Keys

### Overview

The Ark protocol uses two distinct categories of keys for different purposes:

1. **VTXO Ownership Keys (P_v)**: Used in VTXO output scripts, control actual fund ownership.
2. **Session Keys (P_s)**: Ephemeral keys used for MuSig2 signing of VTXT branch transactions.

This separation is a critical protocol design choice that provides privacy, security isolation, and operational flexibility.

### Rationale for Separation

**Privacy**: Using separate session keys prevents linking a participant's VTXO ownership across multiple batches or rounds. The VTXT branch signatures reveal the session key but not the VTXO ownership key. This means an observer cannot determine which VTXOs belong to the same participant by analyzing VTXT signatures.

**Signing Efficiency**: Session keys can be generated fresh each round without affecting long-term key management. MuSig2 nonces can be pre-generated for session keys since they are ephemeral.

**Key Isolation**: Compromise of a session key affects only one round's branch signing ability, not fund ownership. VTXO ownership keys can be kept in cold storage or hardware security modules while session keys are used for active signing.

**Operational Flexibility**: Different security policies can apply to each key type. Session keys can be "hot" for automated signing, while VTXO keys remain "cold" and are only used when spending.

### Usage by Key Type

| Context | Key Type | Purpose |
|---------|----------|---------|
| VTXO output script (internal key) | VTXO ownership key (P_v) | Controls fund ownership |
| VTXO unilateral exit path | VTXO ownership key (P_v) | Signs exit transaction |
| VTXO collaborative spend (OOR/forfeit) | VTXO ownership key (P_v) | Signs spending transaction |
| VTXT branch node keypath | Session key (P_s) | Signs branch transactions |
| Batch output keypath | Session key (P_s) | Signs root transaction |

### Key Derivation Requirements

1. VTXO ownership keys and session keys SHOULD be derived from the same HD wallet master seed.
2. They MUST use different derivation paths to ensure separation (see ARK-05 for paths).
3. Session keys SHOULD be freshly derived for each round.
4. Session keys MUST NOT be reused across different rounds.

### Security Considerations

- Compromise of a session key does NOT allow theft of funds (VTXO key required).
- Compromise of a VTXO key allows spending of that specific VTXO only.
- Loss of session key after round completion has no impact (signatures already collected).
- Loss of VTXO key results in loss of funds (backup essential).

## VTXO Output Script

### Purpose

A VTXO output represents value owned by a participant. It can be spent either collaboratively (with operator co-signature) or unilaterally (after a CSV delay).

### Script Structure

The VTXO output is a taproot output with the following structure:

```
Output Script: OP_1 <output_key>

Where:
  internal_key = ARKNUMSKey (provably unspendable)
  output_key = taproot_tweak(internal_key, script_tree_root)
```

#### Internal Key

The internal key MUST be the ARKNUMSKey, a provably unspendable point. This ensures all spends go through script paths.

#### Script Tree

The script tree contains two leaves:

```
Collaborative Spend Script (multi-sig):
  <P_v> OP_CHECKSIGVERIFY
  <P_o> OP_CHECKSIG

Unilateral Exit Script:
  <P_v> OP_CHECKSIGVERIFY
  <t_e> OP_CHECKSEQUENCEVERIFY
```

Where:
- `P_v`: The VTXO owner's public key
- `t_e`: The relative delay in blocks

### Spend Paths

#### Collaborative Spend (Script Path Multi-sig)

To spend via the collaborative path:

1. Owner produces a signature with `P_v`.
2. Operator produces a signature with `P_o`.
3. Witness: `<sig_operator> <sig_owner> <collab_script> <control_block>`

#### Unilateral Exit (Script Path)

To spend via the unilateral exit path:

1. Wait for at least `t_e` blocks after the VTXO appears on-chain.
2. Produce a signature with the owner's key.
3. Witness: `<signature> <unilateral_script> <control_block>`

### Validation Requirements

Operators validating VTXO requests MUST verify:

1. The output key is correctly derived from the claimed `P_v` and `P_o`.
2. The script tree contains only the expected unilateral exit script.
3. The CSV delay `t_e` meets the operator's minimum requirements.
4. The `P_o` in the script matches the operator's current signing key.

### Mermaid Diagram

```mermaid
graph TD
    subgraph "VTXO Taproot Output"
        IK[Internal Key: MuSig2_P_v_P_o] --> OK[Output Key]
        ST[Script Tree] --> OK
    end

    subgraph "Script Tree"
        ST --> UE[Unilateral Exit Leaf]
    end

    subgraph "Spend Paths"
        OK --> KP[Keypath: MuSig2 sig]
        OK --> SP[Scriptpath: sig + CSV wait]
    end
```

## VTXT Node Output Script

### Purpose

VTXT node outputs (including the root Batch Output) represent intermediate values in the virtual transaction tree. They can be spent collaboratively by all downstream participants or swept by the operator after the absolute expiry.

### Script Structure

```
Output Script: OP_1 <output_key>

Where:
  internal_key = MuSig2_KeyAgg(P_s1, P_s2, ..., P_sn, P_o)
  output_key = taproot_tweak(internal_key, script_tree_root)
```

#### Internal Key

The internal key is a MuSig2 aggregated key of:
- `P_s1, P_s2, ..., P_sn`: **Session public keys** of all downstream VTXO participants (not VTXO ownership keys)
- `P_o`: The operator's public key

Using session keys (rather than VTXO ownership keys) prevents linking a participant's VTXOs across different rounds, as the session keys are ephemeral and change each round.

For a binary tree with radix 2:
- Leaf level: Each branch aggregates session keys of 1 participant + operator
- Level 1: Each branch aggregates session keys of 2 participants + operator
- Level 2: Each branch aggregates session keys of 4 participants + operator
- And so on up to the root

#### Script Tree

The script tree contains a single leaf for the operator sweep path:

```
Operator Sweep Script:
  <P_o> OP_CHECKSIGVERIFY
  <T_e> OP_CHECKSEQUENCEVERIFY OP_DROP
```

Where:
- `P_o`: The operator's public key
- `T_e`: The expiry delay (relative, starts counting from when the branch transaction is confirmed on-chain)

### Spend Paths

#### Collaborative Spend (Keypath)

Used when all downstream participants agree to spend (e.g., during VTXT construction):

1. All participants and operator perform MuSig2 signing protocol.
2. Produce a single aggregated signature.
3. Witness: `<aggregated_signature>`

#### Operator Sweep (Script Path)

Used by the operator to reclaim expired batch funds:

1. Wait for at least `T_e` blocks after the branch transaction is confirmed on-chain.
2. Produce a signature with the operator's key.
3. Witness: `<signature> <sweep_script> <control_block>`

### Tree Construction

The VTXT is constructed bottom-up:

1. **Leaf Level**: Create VTXO outputs for each participant.
2. **Branch Levels**: Group outputs by radix, create branch transactions.
3. **Root Level**: Final branch transaction output is the Batch Output.

For each branch transaction:
- Inputs: Outputs from child branch/leaf transactions
- Outputs: Single output paying to the aggregated key of all downstream participants

### Mermaid Diagram

```mermaid
graph TD
    subgraph "VTXT Structure radix=2"
        ROOT[Batch Output<br/>P_1+P_2+P_3+P_4+P_o]

        B1[Branch vtx_5<br/>P_1+P_2+P_o]
        B2[Branch vtx_6<br/>P_3+P_4+P_o]

        L1[Leaf vtx_1<br/>VTXO P_1]
        L2[Leaf vtx_2<br/>VTXO P_2]
        L3[Leaf vtx_3<br/>VTXO P_3]
        L4[Leaf vtx_4<br/>VTXO P_4]

        ROOT --> B1
        ROOT --> B2
        B1 --> L1
        B1 --> L2
        B2 --> L3
        B2 --> L4
    end
```

## Connector Output Script

### Purpose

Connector outputs provide atomicity for forfeit transactions. They ensure that a forfeit is only valid if the corresponding batch transaction is confirmed.

### Script Structure

Connector leaf outputs use a taproot output with an unspendable internal key and a script-path spend:

```
Output Script: OP_1 <output_key>

Where:
  internal_key = ARKNUMSKey (provably unspendable)
  output_key = taproot_tweak(internal_key, script_tree_root)

Script Tree (single leaf):
  <P_o> OP_CHECKSIG
```

Where `P_o` is the operator's public key. The connector is spendable only by the operator via the script path.

### Connector Tree

When multiple forfeits are included in a round, connectors are organized in a tree structure to minimize on-chain footprint:

1. **Connector Tree Root**: A single output in the batch transaction.
2. **Connector Branches**: Intermediate transactions subdividing the root.
3. **Connector Leaves**: Individual connector outputs used by forfeit transactions.

The radix of the connector tree MAY differ from the VTXT radix. A higher radix reduces tree depth but increases individual transaction sizes.

### Mermaid Diagram

```mermaid
graph TD
    subgraph "Connector Tree radix=4"
        CTX[Batch Tx]
        CR[Connector Root Output]

        C1[Connector 1]
        C2[Connector 2]
        C3[Connector 3]
        C4[Connector 4]

        CTX --> CR
        CR --> CT[Connector Tx]
        CT --> C1
        CT --> C2
        CT --> C3
        CT --> C4
    end

    subgraph "Forfeit Usage"
        C1 --> F1[Forfeit Tx 1]
        C2 --> F2[Forfeit Tx 2]
    end
```

## Boarding Output Script

### Purpose

A boarding output allows a participant to enter the Ark by creating an on-chain UTXO that can be spent collaboratively into a batch transaction, with a timeout fallback.

### Script Structure

```
Output Script: OP_1 <output_key>

Where:
  internal_key = ARKNUMSKey (provably unspendable)
  output_key = taproot_tweak(internal_key, script_tree_root)
```

#### Internal Key

The internal key MUST be the ARKNUMSKey, a provably unspendable point. This ensures all spends go through script paths.

#### Script Tree

The script tree contains two leaves:

```
Collaborative Spend Script (multi-sig):
  <P_b> OP_CHECKSIGVERIFY
  <P_o> OP_CHECKSIG

Timeout Reclaim Script:
  <P_b> OP_CHECKSIGVERIFY
  <t_b> OP_CHECKSEQUENCEVERIFY
```

Where:
- `P_b`: The boarding participant's public key
- `P_o`: The operator's public key
- `t_b`: The boarding timeout in blocks (relative)

### Spend Paths

#### Collaborative Spend (Script Path Multi-sig)

Used as input to the batch transaction:

1. Participant produces a signature with `P_b`.
2. Operator produces a signature with `P_o`.
3. Witness: `<sig_operator> <sig_participant> <collab_script> <control_block>`

Note: A participant could technically board and leave in the same batch transaction. This should be discouraged via fee policy, as it could be used for free UTXO consolidation.

#### Timeout Reclaim (Script Path)

Used if boarding fails or times out:

1. Wait for at least `t_b` blocks after the boarding output is confirmed.
2. Produce a signature with the participant's key.
3. Witness: `<signature> <timeout_script> <control_block>`

### Validation Requirements

Operators validating boarding requests MUST verify:

1. The boarding UTXO exists and is confirmed.
2. The script structure is correct with expected `P_b` and `P_o`.
3. The timeout `t_b` provides sufficient time for round completion.
4. The participant can prove ownership of `P_b`.

## Checkpoint Output Script

### Purpose

Checkpoint outputs provide anti-griefing protection for OOR transactions. They prevent malicious participants from forcing the operator to broadcast expensive transaction chains.

### Script Structure

```
Output Script: OP_1 <output_key>

Where:
  internal_key = ARKNUMSKey (provably unspendable)
  output_key = taproot_tweak(internal_key, script_tree_root)
```

#### Internal Key

The internal key MUST be the ARKNUMSKey, a provably unspendable point. This ensures all spends go through script paths.

#### Script Tree

The script tree contains two leaves:

```
Collaborative Spend Script (multi-sig):
  <P_c> OP_CHECKSIGVERIFY
  <P_o> OP_CHECKSIG

Operator Timeout Script:
  <P_o> OP_CHECKSIGVERIFY
  <t_c> OP_CHECKSEQUENCEVERIFY
```

Where:
- `P_c`: The checkpoint participant's public key (same as VTXO owner being spent)
- `P_o`: The operator's public key
- `t_c`: The checkpoint timeout in blocks (relative)

### Spend Paths

#### Collaborative Spend (Script Path Multi-sig)

Used to continue the Ark transaction chain:

1. Participant produces a signature with `P_c`.
2. Operator produces a signature with `P_o`.
3. The Ark transaction spends from the checkpoint via this path.
4. Witness: `<sig_operator> <sig_participant> <collab_script> <control_block>`

#### Operator Timeout (Script Path)

Used if the participant abandons the checkpoint:

1. Wait for at least `t_c` blocks after the checkpoint appears on-chain.
2. Operator signs and sweeps the checkpoint.
3. Witness: `<signature> <timeout_script> <control_block>`

### Anti-Griefing Mechanism

The checkpoint mechanism works as follows:

1. When a participant spends VTXOs via OOR transaction, they first create checkpoint transaction(s) for each VTXO being spent.
2. Each checkpoint spends one VTXO and creates a checkpoint output.
3. The Ark transaction then spends from one or more checkpoint outputs (one per input VTXO).
4. If the participant later tries to unroll the original VTXO maliciously, the operator only needs to broadcast the checkpoint transaction for that VTXO.
5. If the participant doesn't continue the chain from the checkpoint, the operator can sweep via the timeout path, forcing the participant to broadcast the Ark transaction and complete the OOR chain.

This limits operator on-chain costs regardless of how long the OOR chain is.

## Batch Transaction

### Purpose

The batch transaction anchors one or more batches on-chain. It aggregates multiple participant requests into a single transaction.

### Transaction Structure

```
Batch Transaction:
  Version: 2
  Locktime: 0

  Inputs:
    - Boarding inputs (0 or more)
    - Operator wallet inputs (0 or more)
    - Sweep inputs from expired batches (0 or more)

  Outputs:
    - Batch outputs (1 or more)
    - Connector output (0 or 1)
    - Leave outputs (0 or more)
    - Change output to operator (0 or 1)
```

### Input Types

#### Boarding Inputs

- Spend boarding UTXOs via collaborative script-path multi-sig.
- Require individual signatures from boarding participant and operator.
- Each boarding input corresponds to one or more VTXO requests.

#### Operator Wallet Inputs

- Standard P2TR or P2WPKH inputs from operator's wallet.
- Provide liquidity for the batch.

#### Sweep Inputs

- Spend from expired batch outputs via operator sweep path.
- Recycle operator liquidity from old batches.

### Output Types

#### Batch Outputs

- Pay to VTXT roots.
- Value equals sum of VTXO values in that tree.
- Multiple batch outputs MAY exist in a single batch transaction.

#### Connector Output

- Pay to connector tree root.
- Present if any forfeit transactions are included in this round.
- Value is minimal (546 satoshis minimum for dust limit).

#### Leave Outputs

- Standard outputs paying to participant-specified scripts.
- One output per leave request.

#### Change Output

- Returns excess value to operator.
- Uses operator's standard receive script.

### Mermaid Diagram

```mermaid
graph LR
    subgraph "Batch Transaction"
        subgraph "Inputs"
            BI1[Boarding Input 1]
            BI2[Boarding Input 2]
            OI[Operator Input]
        end

        subgraph "Outputs"
            BO[Batch Output<br/>to VTXT root]
            CO[Connector Output]
            LO[Leave Output]
            CH[Change]
        end

        BI1 --> TX[CTX]
        BI2 --> TX
        OI --> TX
        TX --> BO
        TX --> CO
        TX --> LO
        TX --> CH
    end
```

## Forfeit Transaction

### Purpose

A forfeit transaction atomically transfers a VTXO to the operator in exchange for a new output (VTXO or UTXO) in the batch transaction.

### Transaction Structure

```
Forfeit Transaction:
  Version: 3 (required for zero-fee anchors)
  Locktime: 0

  Inputs:
    - VTXO input (spent via collaborative script-path multi-sig)
    - Connector input (spent via operator script-path)

  Outputs:
    - Operator output (full VTXO value minus fees)
    - Anchor output (ephemeral, 0 sats)
```

### Input Requirements

#### VTXO Input

- Spends the VTXO being forfeited via the collaborative script-path multi-sig.
- Requires individual signatures from both the VTXO owner and operator.
- Owner signs ONLY after verifying their new output in the batch transaction.

#### Connector Input

- Spends a connector leaf from the new batch transaction.
- Requires signature from operator.
- Provides atomicity: forfeit is only valid if batch transaction confirms.

### Output Requirements

#### Operator Output

- Pays the forfeited value to an operator-controlled address.
- Value equals VTXO value minus transaction fees.

#### Anchor Output

- Ephemeral anchor for fee bumping (see [Anchor Outputs](#anchor-outputs)).
- Zero satoshi value.

### Validation Requirements

Participants MUST verify before signing a forfeit:

1. The batch transaction contains their expected new output(s).
2. The connector input references the correct batch transaction.
3. The forfeit outputs are as expected.

Operators MUST verify before signing a forfeit:

1. The VTXO being forfeited is valid and unspent.
2. The participant has proven ownership.
3. The connector tree path is correct.

### Mermaid Diagram

```mermaid
graph LR
    subgraph "Forfeit Transaction"
        VTXO[Old VTXO] --> FT[Forfeit TX]
        CONN[Connector Output] --> FT
        FT --> OP[Operator Output]
        FT --> ANCHOR[Anchor 0 sats]
    end

    subgraph "Batch Transaction"
        CTX[Commitment TX] --> BATCH[Batch Output<br/>contains new VTXO]
        CTX --> CONN
    end
```

## Ark Transaction (OOR Transaction)

### Purpose

An Ark Transaction (also called Out-of-Round Transaction or OOR Transaction) spends one or more VTXOs and creates new VTXOs without requiring a new Batch Transaction. Ark transactions enable instant off-chain transfers between Ark participants.

### Transaction Structure

```
Ark Transaction:
  Version: 3 (required for zero-fee anchors)
  Locktime: 0

  Inputs:
    - Checkpoint output(s) (one per input VTXO, spent via collaborative script-path multi-sig)

  Outputs:
    - New VTXO output(s) for recipient(s)
    - Change VTXO output (if any) for sender
    - Anchor output (ephemeral, 0 sats)
```

### Input Requirements

Each input spends from a checkpoint output:
- The checkpoint was created by a checkpoint transaction that spent the original VTXO.
- Requires individual signatures from both the sender and operator.
- Ark transactions can spend from multiple checkpoint outputs (one per input VTXO being spent).

### Output Requirements

#### New VTXO Outputs

- Use the standard VTXO script structure (see [VTXO Output Script](#vtxo-output-script)).
- Can pay to any valid VTXO script (potentially different owners for each output).

#### Anchor Output

- Ephemeral anchor for fee bumping.
- Zero satoshi value.

### Validation Requirements

Operators validating Ark transactions MUST verify:

1. All input checkpoint outputs are valid and unspent.
2. Input values equal output values (accounting for any implicit fees).
3. All new VTXO outputs have valid script structures.
4. The transaction version is 3 (for zero-fee anchors).

### Mermaid Diagram

```mermaid
graph LR
    subgraph "Ark Transaction"
        CP1[Checkpoint 1<br/>from VTXO A] --> ARK[Ark TX]
        CP2[Checkpoint 2<br/>from VTXO B] --> ARK
        ARK --> V1[New VTXO<br/>to Recipient]
        ARK --> V2[Change VTXO<br/>to Sender]
        ARK --> ANCHOR[Anchor 0 sats]
    end
```

## Anchor Outputs

### Purpose

Anchor outputs enable fee bumping for pre-signed transactions. Since VTXT transactions and forfeit transactions are pre-signed, their fee rates are fixed at signing time. Anchor outputs allow adding fees at broadcast time via CPFP (Child-Pays-For-Parent).

### Ephemeral Anchor Specification

Ark uses ephemeral anchors as specified in BIP proposed for package relay:

```
Anchor Output:
  Value: 0 satoshis
  Script: OP_TRUE
```

This output:
- Has zero value (does not require dust relay rules exemption in modern nodes).
- Is immediately spendable by anyone with `OP_TRUE`.
- Can be spent by a fee-bumping child transaction.

### Usage in Ark

All off-chain transactions (VTXT transactions, Ark transactions, checkpoint transactions, forfeit transactions) MUST include an ephemeral anchor output. These transactions MUST use version 3 to support zero-fee anchors.

When broadcasting these transactions:
1. Create a child transaction spending the anchor.
2. Set the child transaction fee to cover both transactions.
3. Broadcast both transactions as a package.

### Fee Calculation

The fee-bumping child transaction:
- MUST have at least one input from the broadcaster's wallet.
- MUST spend the anchor output.
- SHOULD set fees based on current mempool conditions.

## Transaction Validation Rules

### General Rules

All Ark protocol transactions:

1. Off-chain transactions (VTXT, Ark, checkpoint, forfeit) MUST use transaction version 3 to support zero-fee anchors.
2. On-chain transactions (commitment) MUST use transaction version 2.
3. MUST use witness serialization (SegWit).
4. MUST have valid signatures for all inputs.
5. MUST NOT have negative fee (output sum <= input sum).

### VTXT Transaction Rules

VTXT transactions:

1. MUST spend from either a batch output or another VTXT transaction.
2. MUST have outputs matching the expected VTXT structure.
3. MUST include an anchor output.
4. SHOULD use locktime 0.

### Forfeit Transaction Rules

Forfeit transactions:

1. MUST have exactly two inputs (VTXO and connector).
2. MUST have the connector input from the associated batch transaction.
3. MUST pay the operator output correctly.
4. MUST include an anchor output.

### Batch Transaction Rules

Commitment transactions:

1. MUST have at least one batch output.
2. MUST have connector output if any forfeits are processed.
3. MUST NOT exceed standard transaction size limits.
4. SHOULD target reasonable confirmation time fee rates.

## References

1. BIP 341: Taproot: SegWit version 1 spending rules - https://github.com/bitcoin/bips/blob/master/bip-0341.mediawiki
2. BIP 327: MuSig2 for BIP340-compatible Multi-Signatures - https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki
3. BIP 68: Relative lock-time using consensus-enforced sequence numbers - https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
4. BIP 112: CHECKSEQUENCEVERIFY - https://github.com/bitcoin/bips/blob/master/bip-0112.mediawiki
5. BIP 65: OP_CHECKLOCKTIMEVERIFY - https://github.com/bitcoin/bips/blob/master/bip-0065.mediawiki

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
