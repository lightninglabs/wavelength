# ARK-00: Protocol Overview and Terminology

## Abstract

This document defines the Ark protocol, a second-layer Bitcoin scaling solution that enables users to hold and transfer bitcoin off-chain through Virtual Transaction Outputs (VTXOs). The protocol allows participants to transact with instant finality while maintaining the ability to unilaterally exit to the Bitcoin base layer at any time.

Ark operates through a central coordinator called the Ark Operator (or Ark Service Provider, ASP) who facilitates the creation of shared UTXOs that can be virtually subdivided among multiple participants. Participants can transfer value off-chain through Ark Transactions, refresh their holdings through Batch Swaps, or exit to on-chain UTXOs through Leave Requests.

## Status

This specification is version 1 (v1). All normative language reflects the
current production protocol; legacy v0 paragraphs have been retired.

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

The Ark protocol operates through periodic **Rounds** during which the operator constructs a **Batch Transaction**. This transaction creates one or more **Batch Outputs** that pay to a **Virtual Transaction Tree (VTXT)**. The leaves of this tree contain **Virtual Transaction Outputs (VTXOs)** owned by individual participants.

Between rounds, participants can spend their VTXOs through **Out-of-Round (OOR) Transactions**, also called **Ark Transactions**. These transactions are co-signed by the operator and create new VTXOs without requiring an on-chain transaction.

VTXOs have a limited lifetime determined by their batch's **Sweep Delay** (`T_e`). Before the operator becomes eligible to sweep, participants MUST either:
1. Perform a **Batch Swap** to obtain a fresh VTXO in a new batch
2. Execute a **Leave Request** to exit to an on-chain UTXO
3. Perform a **Unilateral Exit** by broadcasting the VTXT path on-chain

#### Round Cadence

Rounds advance on a fixed cadence rather than purely on-demand. The
operator MUST run a periodic round-tick so that rounds progress even
with zero admitted clients, and MUST fail fast on any administrative
"trigger batch" request against a Created-state round that has no
admitted clients (see ARK-02). This guarantees forward progress and
bounded round lifetime regardless of client arrival pattern.

#### Relay Primitive: TRUC + P2A

All on-chain transactions defined by this specification — Batch
Transactions, VTXT branch transactions, Checkpoint Transactions, Ark
Transactions, Forfeit Transactions, Connector Tree transactions, and
Sweep Transactions — MUST be constructed as TRUC (`nVersion=3`) zero-fee
templates that carry exactly one ephemeral P2A anchor as their final
output. Fees are supplied at broadcast time via package relay using a
CPFP child funded from the broadcasting party's wallet. See ARK-01 for
the normative tx format and ARK-04 for operator-side package-relay
requirements.

#### Operator Fraud-Response Subsystem

The operator runs a fraud-response subsystem that monitors all unswept
batch outputs and reacts to on-chain spends of VTXOs whose state requires
an operator response. The subsystem covers two response paths:

1. **Spent VTXOs** (already consumed via OOR): the operator broadcasts
   the persisted Checkpoint Transaction, then ratchets a watched frontier
   forward through the resulting recipient Ark Transactions. The ratchet
   iterates to arbitrary depth across multihop OOR transfer chains until
   it reaches either a still-live recipient VTXO (which transitions to
   the terminal `unrolled_by_client` state) or the operator's own CSV
   timeout sweep on a checkpoint output. See ARK-04.
2. **Forfeit VTXOs** (forfeited as part of a Leave or Batch Swap): the
   operator rebuilds the connector path from the round's connector tree
   descriptor, signs each ancestor with the operator key, and submits
   the connector ancestors followed by the stored forfeit transaction
   sequentially via package relay. See ARK-04.

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

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED", "NOT RECOMMENDED", "MAY", and "OPTIONAL" in these documents are to be interpreted as described in RFC 2119 [[1]](#references) when, and only when, they appear in all capitals.

These words may also appear in this document in lower case as plain English words, and in that case do not carry the normative meaning defined above.

## Protocol Versioning

### Version Field

The protocol version is represented as a 16-bit unsigned integer. The
version defined by this specification is version `1`.

```
Version := uint16
```

### Version Attachment

Protocol versions are attached at the **batch level**. All VTXOs created within a single Batch Transaction MUST use the same protocol version. This version determines:

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
- New VTXOs created in a batch MUST use the operator's current preferred version, regardless of the version of the input VTXOs being spent or swapped.
- VTXOs from batches with different versions MAY be spent together in an OOR transaction, provided the operator supports spending from those older versions.
- The operator MUST support spending FROM VTXOs created under older versions (to allow batch swaps and OOR transfers), but is NOT required to create new batches using old versions.
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
1. **Collaborative Path**: Spendable immediately via a tapscript-path multi-sig of the VTXO owner and operator. This uses individual BIP-340 Schnorr signatures (not MuSig2 keypath) to avoid the need for interactive nonce exchange.
2. **Unilateral Exit Path**: Spendable by the VTXO owner alone after a relative timelock (CSV delay).

The internal key of a VTXO output is the ARKNUMSKey (a provably unspendable point), ensuring all spends go through script paths.

VTXOs are categorized as:
- **Confirmed VTXO**: A VTXO that is a direct leaf of the VTXT, spending from a VTXT branch transaction.
- **Preconfirmed VTXO**: A VTXO that results from an OOR/Ark transaction, spending from another VTXO.

#### Virtual Transaction Tree (VTXT)

The Virtual Transaction Tree is a fan-out radix tree of pre-signed
transactions that subdivides a Batch Output into individual VTXOs. The tree
structure allows any participant to unilaterally claim their VTXO by
broadcasting only the path from the root to their leaf.

Leaves are assigned using a deterministic LPT ordering (amount descending,
pkScript tiebreaker) to keep the fan-out balanced.

VTXT branch nodes have two spend paths:
1. **Collaborative Path**: MuSig2 aggregated signature of all downstream participants' ephemeral signing keys and the operator key. Note: these are per-round signing keys (`P_s`), NOT the VTXO ownership keys (`P_v`), to prevent cross-round linkability.
2. **Sweep Path**: Spendable by the operator alone after a relative timelock (CSV delay) following the on-chain confirmation of the branch transaction.

Note: VTXO leaves do not have a sweep path — only VTXT branch nodes do. Similarly, connector tree nodes do not have a sweep path.

#### Batch Transaction

The Batch Transaction is a Bitcoin transaction that anchors one or more batches on-chain. It contains:
- **Inputs**: Boarding inputs from participants and/or operator wallet inputs
- **Batch Outputs**: Outputs paying to VTXT roots that have VTXO leaves
- **Connector Outputs**: Outputs paying to tree roots with connector leaves used for forfeit transaction atomicity
- **Leave Outputs**: Direct on-chain outputs for leave requests
- **Change Outputs**: Change returned to the operator

#### Batch Output

A Batch Output is an output of the Batch Transaction that pays to the root of a VTXT which has VTXOs as leaves. The total value of a Batch Output equals the sum of all VTXO values in that tree.

Note that Connector Outputs also pay to tree roots, but those trees have connector leaves (not VTXOs) used for forfeit transaction atomicity. Connector tree nodes do not have a sweep path — only the operator can spend connector outputs.

### Operations

#### Round

A Round is the process of constructing, signing, and broadcasting a Batch Transaction. Rounds occur periodically and aggregate multiple participant requests.

#### Boarding

Boarding is the process of entering the Ark by spending an on-chain UTXO (a Boarding UTXO) as an input to a Batch Transaction in exchange for receiving one or more VTXOs in the resulting batch.

A Boarding UTXO has two spend paths:
1. **Collaborative Path**: Operator and participant provide individual signatures via a script-path multi-sig (not MuSig2 keypath).
2. **Timeout Path**: Participant can reclaim after a relative timelock if boarding fails.

#### Leave Request (Collaborative Exit)

A Leave Request allows a participant to exit the Ark by forfeiting one or more VTXOs in exchange for receiving a standard on-chain UTXO in the Batch Transaction.

#### Batch Swap (Refresh)

A Batch Swap allows a participant to refresh expiring VTXOs by forfeiting them in exchange for new VTXOs in a fresh batch. The new VTXOs will have a later expiry.

#### Forfeit Transaction

A Forfeit Transaction spends a VTXO via the collaborative path and a Connector Output, paying the VTXO value to the operator. Forfeit transactions provide atomicity for Leave Requests and Batch Swaps.

The Forfeit Transaction has two inputs:
1. The VTXO being forfeited (spent via collaborative path)
2. A Connector Output from the new Batch Transaction

This structure ensures the forfeit is only valid if the new Batch Transaction is confirmed.

#### Out-of-Round Transaction (OOR Transaction / Ark Transaction)

An OOR Transaction, also called an Ark Transaction, spends one or more VTXOs and creates new VTXOs. OOR transactions do not require a new Batch Transaction and can be performed at any time between rounds.

#### Checkpoint Transaction

A Checkpoint Transaction is an intermediate transaction between a VTXO and an Ark Transaction. It provides anti-griefing protection by allowing the operator to claim funds if a malicious participant attempts to force expensive on-chain resolution.

The checkpoint output uses a tapscript tree with two leaves:
1. **Operator Unroll Leaf**: Spendable by the operator after a CSV timeout (`t_c`), allowing the operator to claim funds if the participant abandons the chain.
2. **Owner Closure Leaf**: A pluggable script (closure) that enables spending into the Ark transaction. The default closure is a collaborative multi-sig between the VTXO owner and operator.

This mechanism also incentivizes users not to perform griefing attacks, as they would lose their funds to the operator via the timeout path.

#### Closure

A Closure is a pluggable script committed to the checkpoint tap tree's owner leaf. It defines how the Ark transaction can spend from the checkpoint output. The default closure is a collaborative multi-sig (`<P_c> OP_CHECKSIGVERIFY <P_o> OP_CHECKSIG`), but operators MAY define policy for acceptable closure types to support more advanced spending conditions.

### Timelocks

#### Sweep Delay / Batch Lifetime (T_e)

The Sweep Delay (also called Batch Lifetime) is the duration after which the operator can sweep unspent VTXT branch node outputs. It is expressed as a relative timelock using `OP_CHECKSEQUENCEVERIFY` (CSV), which starts counting from when the branch transaction is confirmed on-chain.

**Important:** Because `T_e` is a CSV (relative) timelock rather than a CLTV (absolute) timelock, a participant who broadcasts VTXT branch transactions late can effectively extend the operator's lockup period. In the worst case, if a participant broadcasts just before the expected batch expiry, the operator's sweep becomes available `T_e` blocks after that late confirmation — extending the total lockup.

All VTXOs in a batch share the same sweep delay.

#### VTXO Exit Delay (t_e)

The VTXO Exit Delay is the CSV (CheckSequenceVerify) delay on the unilateral exit path of VTXOs. It provides time for the operator to respond if a participant attempts to claim a VTXO that has been forfeited or spent via OOR transaction.

The exit delay ensures that even if a VTXO is broadcast on-chain near the end of the batch lifetime, the operator still has time to respond by broadcasting a forfeit or checkpoint transaction before the CSV delay expires.

#### Connector Output

A Connector Output is an output in the Batch Transaction used to provide atomicity for Forfeit Transactions. Connector outputs are organized in a tree structure (the Connector Tree) to efficiently support many forfeit transactions.

Connector outputs are spendable only by the operator.

## Security Model

### Threat Model

The Ark protocol considers the following threat scenarios:

1. **Malicious Participant**: A participant attempts to double-spend by unilaterally broadcasting spent VTXOs, or performs griefing attacks such as:
   - Forcing on-chain resolution of long VTXO-spend chains (mitigated by the checkpoint mechanism, which bounds operator costs)
   - Forcing the operator to lock up liquidity asymmetrically (e.g., boarding with a large UTXO and immediately leaving in the same batch, locking operator wallet inputs for the batch lifetime with no consequence to the participant)
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

**Mechanism**: Checkpoint transactions ensure the operator can claim funds without needing to broadcast the full OOR chain. The operator only needs to broadcast the single checkpoint transaction that directly spends the contested VTXO. If the participant doesn't continue the chain from the checkpoint (by broadcasting the Ark transaction), the operator claims the funds via the checkpoint timeout path after `t_c` blocks. This bounds the operator's on-chain costs to a single transaction regardless of OOR chain length.

#### Property 4: Atomicity

Leave Requests and Batch Swaps MUST be atomic: either the participant receives their new output AND the operator receives the forfeited VTXO, or neither party receives anything.

**Mechanism**: Forfeit Transactions spend both the VTXO and a Connector Output from the same Batch Transaction. The forfeit is only valid if that Batch Transaction is confirmed.

### Operator Availability Requirements

The operator MUST:
1. Monitor all unswept batch outputs for spends.
2. Respond to unilateral exits by broadcasting Forfeit or Checkpoint transactions within the VTXO exit delay (`t_e`).
3. Sweep expired batches to reclaim liquidity after the sweep delay (`T_e`) elapses.

If the operator fails to respond within the VTXO exit delay, participants may successfully double-spend forfeited or spent VTXOs.

## Trust Assumptions

### Operator Trust

Participants trust the operator to:

1. **Availability**: Remain online and responsive to facilitate rounds and OOR transactions. Note that even if the operator disappears entirely, participants can still exit on-chain via unilateral exit — they do not lose funds, only the convenience of off-chain transfers.
2. **Honest Signing**: Co-sign valid OOR transactions and not sign conflicting transactions. If the operator signs conflicting transactions, there is clear cryptographic evidence (two valid signatures on conflicting transactions spending the same VTXO) which can be publicized, causing the operator to immediately lose trust for future participation.
3. **Timely Response**: Monitor the chain and respond to unilateral exits within the CSV delay.

Participants do NOT need to trust the operator to:

1. **Custody**: The operator cannot steal funds unilaterally; participants can always exit on-chain.
2. **Censorship Resistance**: If the operator censors a participant, they can exit on-chain.

### Preconfirmed VTXO Trust

Recipients of preconfirmed VTXOs (from OOR transactions) have additional trust considerations compared to confirmed VTXOs:

1. **Sender Trust**: The sender could attempt to double-spend by unilaterally broadcasting the original VTXO. However, the operator holds checkpoint transactions that can be broadcast to reclaim the funds.
2. **Monitoring Requirement**: If the recipient is not the owner of the confirmed-parent VTXOs in the chain, they should monitor the chain for parent VTXT confirmations and potentially manage checkpoint transaction broadcasts if the operator is offline.

**Confirmed vs Preconfirmed VTXOs**:
- A preconfirmed VTXO can be converted to a confirmed one via a Batch Swap. Recipients SHOULD batch-swap promptly to reduce trust exposure.
- A confirmed VTXO has significantly fewer trust assumptions: it is a direct leaf of an on-chain VTXT and doesn't depend on parent VTXOs or OOR transaction chains.
- If a preconfirmed VTXO holder is not the owner of the confirmed-parent VTXOs, they must monitor the chain and potentially broadcast checkpoint transactions if the operator is unavailable.

If the sender does attempt to double-spend:
- The operator detects the on-chain broadcast of the original VTXO.
- The operator broadcasts the checkpoint transaction that spends the same VTXO via the collaborative path, racing the sender's CSV delay.
- If the checkpoint confirms, the operator claims the funds via the timeout path after `t_c` blocks. The operator is now economically whole and can include a replacement VTXO for the recipient in a future batch.
- The recipient's funds are protected as long as the operator responds correctly.
- The sender's malicious behavior becomes publicly provable (two valid signatures on conflicting transactions constitute cryptographic evidence of double-signing).

Note: The recipient must trust the operator to include their replacement VTXO in a future batch. The economic incentive aligns: the operator benefits from maintaining reputation and the recipient's continued participation.

### Reputation

Operator double-signing (signing conflicting transactions) produces cryptographic proof of misbehavior. This proof can be used to:
- Establish public reputation systems for operators.
- Provide evidence for legal action in jurisdictions where applicable.

## Notation Conventions

### Keys and Points

| Notation | Description |
|----------|-------------|
| `P_o` | Operator's collaborative signing public key |
| `P_c` | Client/participant's public key |
| `P_v` | VTXO ownership public key (used in VTXO scripts) |
| `P_s` | Per-VTXO ephemeral signing key (used for VTXT MuSig2 branch signing) |
| `P_sw` | Operator's sweep public key (may differ from `P_o`) |
| `P_m` | MuSig2 aggregate public key (VTXT branches only) |

### Timelocks

| Notation | Description |
|----------|-------------|
| `T_e` | Sweep delay / batch lifetime (CSV relative timelock on VTXT branch nodes) |
| `t_e` | VTXO exit delay in blocks (CSV on unilateral exit path) |
| `t_b` | Boarding UTXO timeout (CSV) |
| `t_c` | Checkpoint timeout (CSV on operator unroll leaf) |

### Transactions

| Notation | Description |
|----------|-------------|
| `ctx` | Batch Transaction |
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

1. RFC 2119: Key words for use in RFCs to Indicate Requirement Levels - https://www.ietf.org/rfc/rfc2119.txt
2. BIP 327: MuSig2 for BIP340-compatible Multi-Signatures - https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki
3. BIP 341: Taproot: SegWit version 1 spending rules - https://github.com/bitcoin/bips/blob/master/bip-0341.mediawiki
4. BIP 342: Validation of Taproot Scripts - https://github.com/bitcoin/bips/blob/master/bip-0342.mediawiki
5. BIP 68: Relative lock-time using consensus-enforced sequence numbers - https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
6. BIP 112: CHECKSEQUENCEVERIFY - https://github.com/bitcoin/bips/blob/master/bip-0112.mediawiki

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
