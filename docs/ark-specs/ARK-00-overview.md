# ARK-00: Protocol Overview and Terminology

## Abstract

This document defines the Ark protocol, a second-layer Bitcoin scaling solution that enables users to hold and transfer bitcoin off-chain through Virtual Transaction Outputs (VTXOs). The protocol allows participants to transact with instant finality while maintaining the ability to unilaterally exit to the Bitcoin base layer at any time.

Ark operates through a central coordinator called the Ark Operator (or Ark Service Provider, ASP) who facilitates the creation of shared UTXOs that can be virtually subdivided among multiple participants. Participants can transfer value off-chain through Ark Transactions, refresh their holdings through Batch Swaps, or exit to on-chain UTXOs through Leave Requests.

## Status

This specification is a working draft.

## Table of Contents

1. [Introduction](#introduction)
2. [Requirements Language](#requirements-language)
3. [Protocol Versioning](#protocol-versioning)
4. [Terminology](#terminology)
5. [Security Model](#security-model)
6. [Trust Assumptions](#trust-assumptions)
7. [Notation Conventions](#notation-conventions)
8. [References](#references)

## Introduction

### Background

Bitcoin's base layer has limited throughput, making it challenging to support high-frequency, low-value transactions. Various second-layer solutions have emerged to address this limitation, including the Lightning Network and other payment channel constructions.

Ark represents an alternative approach that trades some properties of payment channels for different trade-offs:

- **No inbound liquidity requirement**: Unlike Lightning, recipients do not need pre-existing channel capacity.
- **Simpler user experience**: Users interact with a single operator rather than managing channel state with multiple peers.
- **Operator liquidity provision**: The operator provides the capital required for instant settlement.

### Protocol Overview

The Ark protocol operates through periodic **Rounds** during which the operator constructs a **Commitment Transaction**. This transaction creates one or more **Batch Outputs** that pay to a **Virtual Transaction Tree (VTXT)**. The leaves of this tree contain **Virtual Transaction Outputs (VTXOs)** owned by individual participants.

Between rounds, participants can spend their VTXOs through **Out-of-Round (OOR) Transactions**, also called **Ark Transactions**. These transactions are co-signed by the operator and create new VTXOs without requiring an on-chain transaction.

VTXOs have a limited lifetime determined by their batch's **Absolute Expiry**. Before expiry, participants MUST either:
1. Perform a **Batch Swap** to obtain a fresh VTXO in a new batch
2. Execute a **Leave Request** to exit to an on-chain UTXO
3. Perform a **Unilateral Exit** by broadcasting the VTXT path on-chain

### Document Organization

The Ark specification is organized into the following documents:

| Document | Title | Description |
|----------|-------|-------------|
| ARK-00 | Protocol Overview and Terminology | This document |
| ARK-01 | Transaction Formats and Scripts | Transaction structures and Bitcoin Script specifications |
| ARK-02 | Round Lifecycle Protocol | Batch construction and signing protocol |
| ARK-03 | Out-of-Round Transactions | OOR/Ark transactions and checkpoint mechanism |
| ARK-04 | Monitoring and Fraud Response | Operator monitoring and response requirements |
| ARK-05 | Client Wallet Requirements | Client-side implementation requirements |
| ARK-06 | Wire Protocol | Client-operator communication protocol |

## Requirements Language

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in this document are to be interpreted as described in BIP 14 [[1]](#references) when, and only when, they appear in all capitals.

These words may also appear in this document in lower case as plain English words, and in that case do not carry the normative meaning defined above.

## Protocol Versioning

### Version Field

The protocol version is represented as a 16-bit unsigned integer. The initial version defined by this specification is version `1`.

```
Version := uint16
```

### Version Attachment

Protocol versions are attached at the **batch level**. All VTXOs created within a single Commitment Transaction MUST use the same protocol version. This version determines:

- The script structures used for VTXOs and VTXT nodes
- The message formats for the round signing protocol
- The OOR transaction and checkpoint formats

### Version Negotiation

During connection establishment, clients and operators negotiate the protocol version:

1. The client MUST query the operator's supported versions via the `GetInfo` message.
2. The operator MUST respond with a list of supported versions and a preferred version.
3. The client SHOULD select the highest mutually supported version.
4. If no common version exists, the client MUST NOT participate in rounds with that operator.

### Backwards Compatibility

Operators MAY support multiple protocol versions simultaneously. When doing so:

- Each batch MUST use exactly one version.
- VTXOs from batches with different versions MAY be spent together in an OOR transaction if both versions are compatible.
- The operator MUST reject OOR transactions that mix incompatible versions.

### Version Upgrade Transitions

When a new protocol version is deployed:

1. Operators SHOULD announce support for new versions while maintaining support for existing versions.
2. Existing batches continue using their original version until expiry.
3. New batches MAY use the new version once operator and client both support it.
4. Operators SHOULD maintain support for older versions until all batches using those versions have expired.

## Terminology

### Core Concepts

#### Virtual Transaction Output (VTXO)

A Virtual Transaction Output is an output that can be spent either collaboratively (with operator co-signature) or unilaterally (after a timeout). VTXOs are "virtual" in the sense that they exist off-chain unless explicitly broadcast.

A VTXO has two spend paths:
1. **Collaborative Path**: Spendable immediately via MuSig2 aggregated signature of the VTXO owner and operator.
2. **Unilateral Exit Path**: Spendable by the VTXO owner alone after a relative timelock (CSV delay).

VTXOs are categorized as:
- **Confirmed VTXO**: A VTXO that is a direct leaf of the VTXT, spending from a VTXT branch transaction.
- **Preconfirmed VTXO**: A VTXO that results from an OOR/Ark transaction, spending from another VTXO.

#### Virtual Transaction Tree (VTXT)

The Virtual Transaction Tree is a balanced tree of pre-signed transactions that subdivides a Batch Output into individual VTXOs. The tree structure allows any participant to unilaterally claim their VTXO by broadcasting only the path from the root to their leaf.

VTXT nodes have two spend paths:
1. **Collaborative Path**: MuSig2 aggregated signature of all downstream VTXO owners and the operator.
2. **Sweep Path**: Spendable by the operator alone after the batch's absolute timelock expires.

#### Commitment Transaction

The Commitment Transaction is a Bitcoin transaction that anchors one or more batches on-chain. It contains:
- **Inputs**: Boarding inputs from participants and/or operator wallet inputs
- **Batch Outputs**: Outputs paying to VTXT roots
- **Connector Outputs**: Outputs used for forfeit transaction atomicity
- **Leave Outputs**: Direct on-chain outputs for leave requests
- **Change Outputs**: Change returned to the operator

#### Batch Output

A Batch Output is an output of the Commitment Transaction that pays to the root of a VTXT. The total value of a Batch Output equals the sum of all VTXO values in that tree plus any fees.

### Operations

#### Round

A Round is the process of constructing, signing, and broadcasting a Commitment Transaction. Rounds occur periodically and aggregate multiple participant requests.

#### Boarding

Boarding is the process of entering the Ark by spending an on-chain UTXO (a Boarding UTXO) as an input to a Commitment Transaction in exchange for receiving one or more VTXOs in the resulting batch.

A Boarding UTXO has two spend paths:
1. **Collaborative Path**: Operator and participant co-sign to use as Commitment Transaction input.
2. **Timeout Path**: Participant can reclaim after a relative timelock if boarding fails.

#### Leave Request (Collaborative Exit)

A Leave Request allows a participant to exit the Ark by forfeiting one or more VTXOs in exchange for receiving a standard on-chain UTXO in the Commitment Transaction.

#### Batch Swap (Refresh)

A Batch Swap allows a participant to refresh expiring VTXOs by forfeiting them in exchange for new VTXOs in a fresh batch. The new VTXOs will have a later expiry.

#### Forfeit Transaction

A Forfeit Transaction spends a VTXO via the collaborative path and a Connector Output, paying the VTXO value to the operator. Forfeit transactions provide atomicity for Leave Requests and Batch Swaps.

The Forfeit Transaction has two inputs:
1. The VTXO being forfeited (spent via collaborative path)
2. A Connector Output from the new Commitment Transaction

This structure ensures the forfeit is only valid if the new Commitment Transaction is confirmed.

#### Out-of-Round Transaction (OOR Transaction / Ark Transaction)

An OOR Transaction, also called an Ark Transaction, spends one or more VTXOs and creates new VTXOs. OOR transactions do not require a new Commitment Transaction and can be performed at any time between rounds.

#### Checkpoint Transaction

A Checkpoint Transaction is an intermediate transaction between a VTXO and an Ark Transaction. It provides anti-griefing protection by allowing the operator to claim funds if a malicious participant attempts to force expensive on-chain resolution.

### Timelocks

#### Absolute Expiry (T_e)

The Absolute Expiry is a block height after which the operator can sweep all unspent VTXT outputs (branch nodes and batch outputs). It is expressed as an absolute block height using `OP_CHECKLOCKTIMEVERIFY`.

All VTXOs in a batch share the same absolute expiry.

#### Relative Delay (t_e)

The Relative Delay is the CSV (CheckSequenceVerify) delay on the unilateral exit path of VTXOs. It provides time for the operator to respond if a participant attempts to claim a VTXO that has been forfeited or spent via OOR transaction.

The relative delay ensures that even if a VTXO is broadcast just before the absolute expiry, the operator still has time to respond.

#### Connector Output

A Connector Output is an output in the Commitment Transaction used to provide atomicity for Forfeit Transactions. Connector outputs are organized in a tree structure (the Connector Tree) to efficiently support many forfeit transactions.

Connector outputs are spendable only by the operator.

## Security Model

### Threat Model

The Ark protocol considers the following threat scenarios:

1. **Malicious Participant**: A participant attempts to double-spend by unilaterally broadcasting spent VTXOs.
2. **Malicious Operator**: The operator attempts to steal funds by refusing to honor valid VTXOs.
3. **Colluding Parties**: Operator and participant(s) collude against other participants.

### Security Properties

#### Property 1: Unilateral Exit

A participant holding a valid VTXO MUST be able to claim their funds on-chain without operator cooperation, provided they act before the batch expiry.

**Mechanism**: The participant broadcasts the VTXT path to their VTXO, then spends the VTXO via the unilateral exit path after the CSV delay.

#### Property 2: Forfeit Protection

If a participant forfeits a VTXO (for a Leave Request or Batch Swap), the operator MUST be able to claim those funds if the participant later attempts to unilaterally exit from the forfeited VTXO.

**Mechanism**: The operator holds a signed Forfeit Transaction that spends the VTXO via the collaborative path. If the VTXO is broadcast on-chain, the operator broadcasts the Forfeit Transaction before the CSV delay expires.

#### Property 3: Checkpoint Protection

If a participant spends a VTXO via OOR transaction, the operator MUST be able to claim the funds if the participant later attempts to unilaterally exit from the spent VTXO.

**Mechanism**: Checkpoint transactions ensure the operator can claim funds without needing to broadcast the full OOR chain. The checkpoint output has a timeout path allowing the operator to sweep if the participant doesn't continue the chain.

#### Property 4: Atomicity

Leave Requests and Batch Swaps MUST be atomic: either the participant receives their new output AND the operator receives the forfeited VTXO, or neither party receives anything.

**Mechanism**: Forfeit Transactions spend both the VTXO and a Connector Output from the same Commitment Transaction. The forfeit is only valid if that Commitment Transaction is confirmed.

### Operator Availability Requirements

The operator MUST:
1. Monitor all unswept batch outputs for spends.
2. Respond to unilateral exits by broadcasting Forfeit or Checkpoint transactions within the CSV delay.
3. Sweep expired batches to reclaim liquidity.

If the operator fails to respond within the CSV delay, participants may successfully double-spend forfeited or spent VTXOs.

## Trust Assumptions

### Operator Trust

Participants trust the operator to:

1. **Availability**: Remain online and responsive to facilitate rounds and OOR transactions.
2. **Honest Signing**: Co-sign valid OOR transactions and not sign conflicting transactions.
3. **Timely Response**: Monitor the chain and respond to unilateral exits within the CSV delay.

Participants do NOT need to trust the operator to:

1. **Custody**: The operator cannot steal funds unilaterally; participants can always exit on-chain.
2. **Censorship Resistance**: If the operator censors a participant, they can exit on-chain.

### Preconfirmed VTXO Trust

Recipients of preconfirmed VTXOs (from OOR transactions) have additional trust considerations:

1. **Sender Trust**: The sender could attempt to double-spend by unilaterally broadcasting the original VTXO.
2. **Monitoring Requirement**: The recipient should monitor the chain or perform a Batch Swap promptly.

If the sender does attempt to double-spend:
- The operator will broadcast checkpoint transactions to prevent the double-spend.
- The recipient's funds are protected as long as the operator responds correctly.
- The sender's malicious behavior becomes publicly provable (evidence of the double-sign).

### Reputation

Operator double-signing (signing conflicting transactions) produces cryptographic proof of misbehavior. This proof can be used to:
- Establish public reputation systems for operators.
- Provide evidence for legal action in jurisdictions where applicable.

## Notation Conventions

### Keys and Points

| Notation | Description |
|----------|-------------|
| `P_o` | Operator's public key |
| `P_c` | Client/participant's public key |
| `P_v` | VTXO ownership public key |
| `P_m` | MuSig2 aggregate public key |
| `P_s` | Signing session public key |

### Timelocks

| Notation | Description |
|----------|-------------|
| `T_e` | Absolute expiry block height (CLTV) |
| `t_e` | Relative delay in blocks (CSV) |
| `t_b` | Boarding UTXO timeout (CSV) |
| `t_c` | Checkpoint timeout (CSV) |

### Transactions

| Notation | Description |
|----------|-------------|
| `ctx` | Commitment Transaction |
| `vtx_n` | Virtual Transaction at tree level n |
| `ark_n` | Ark/OOR Transaction number n in a chain |
| `cp_n` | Checkpoint Transaction number n |
| `forfeit` | Forfeit Transaction |

### Scripts

| Notation | Description |
|----------|-------------|
| `<sig>` | A signature |
| `<pk>` | A public key |
| `<n>` | A number |
| `OP_CSV` | OP_CHECKSEQUENCEVERIFY |
| `OP_CLTV` | OP_CHECKLOCKTIMEVERIFY |

## References

1. BIP 14: Protocol Version and User Agent - https://github.com/bitcoin/bips/blob/master/bip-0014.mediawiki
2. BIP 327: MuSig2 for BIP340-compatible Multi-Signatures - https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki
3. BIP 341: Taproot: SegWit version 1 spending rules - https://github.com/bitcoin/bips/blob/master/bip-0341.mediawiki
4. BIP 342: Validation of Taproot Scripts - https://github.com/bitcoin/bips/blob/master/bip-0342.mediawiki
5. BIP 68: Relative lock-time using consensus-enforced sequence numbers - https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
6. BIP 112: CHECKSEQUENCEVERIFY - https://github.com/bitcoin/bips/blob/master/bip-0112.mediawiki

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
