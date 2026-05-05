# ARK-05: Client Wallet Requirements

## Abstract

This document specifies the requirements for Ark client wallet implementations. It covers key management, state storage, verification procedures, and unilateral exit processes. Clients implementing this specification will be able to safely participate in the Ark protocol.

## Status

This specification is version 1 (v1). Client-side requirements track
the v1 protocol contract: TRUC + P2A package broadcast, server-
authoritative VTXO locks with owner proof, the seal-time fee
handshake, the `LeaveVTXOs` and `NewReceiveScript` RPCs, and the
typed OOR rejection branch. Legacy v0 paragraphs have been retired.

## Table of Contents

1. [Introduction](#introduction)
2. [Key Management](#key-management)
3. [State Storage Requirements](#state-storage-requirements)
4. [Verification Requirements](#verification-requirements)
5. [Unilateral Exit Procedure](#unilateral-exit-procedure)
6. [Operational Procedures](#operational-procedures)
7. [Security Considerations](#security-considerations)

## Introduction

### Client Responsibilities

An Ark client wallet is responsible for:

1. **Key management**: Generating, storing, and using cryptographic keys.
2. **State storage**: Maintaining data necessary for VTXO ownership and exit.
3. **Verification**: Validating all data received from the operator.
4. **Unilateral exit**: Broadcasting transactions to claim funds without operator cooperation.

### Trust Model

Clients MUST NOT trust the operator for:
- Custody of funds (clients can always exit unilaterally)
- Correctness of data (all data must be verified)

Clients MAY trust the operator for:
- Availability (operator being online to process transactions)
- Honest behavior (not signing conflicting transactions)

## Key Management

### Key Types

```mermaid
graph TD
    subgraph "Key Hierarchy"
        MK[Master Key / Seed]
        MK --> VK[VTXO Keys]
        MK --> SK[Session Keys]
        MK --> BK[Boarding Keys]

        VK --> VK1[VTXO Key 1]
        VK --> VK2[VTXO Key 2]
        VK --> VKN[VTXO Key N]

        SK --> SK1[Session Key 1]
        SK --> SK2[Session Key 2]

        BK --> BK1[Boarding Key 1]
        BK --> BK2[Boarding Key 2]
    end
```

### VTXO Ownership Keys

VTXO ownership keys are used in VTXO output scripts for both collaborative and unilateral spending.

**Requirements:**
1. Keys MUST be derived from a BIP-32 HD wallet.
2. Each VTXO SHOULD use a unique key (for privacy).
3. Keys MUST be stored securely and backed up.

**Derivation Path (RECOMMENDED):**
```
m/ark'/<coin_type>'/<account>'/<change>/<index>

Where:
  ark' = hardened purpose for Ark (number TBD)
  coin_type' = 0' for mainnet, 1' for testnet
  account' = account number
  change = 0 for receiving, 1 for change
  index = sequential key index
```

### Signing Keys

Signing keys are used for MuSig2 signing sessions during rounds, specifically
for VTXT branch node aggregated keys. These are provided **per VTXO request**
and are ephemeral — they are used only during the round and can be discarded
after batch confirmation.

**Requirements:**
1. Each VTXO MUST have its own signing key (in addition to its VTXO output key).
2. Signing keys MAY be derived from the HD wallet or randomly generated.
3. MUST be stored at least until the batch transaction is confirmed.
4. MUST NOT be reused across different rounds.
5. MAY be discarded after batch confirmation (the signatures are already collected).
6. MUST be different from the VTXO ownership key for the same VTXO.

**Usage:**
- Used exclusively in VTXT branch node MuSig2 aggregated keys.
- Allows round participation without exposing VTXO ownership keys.
- Distinct from VTXO output keys: signing keys sign VTXT branches, output keys
  are used in VTXO scripts.
- OOR spends use VTXO ownership keys (`P_v`), NOT signing keys (`P_s`).

**Key separation:** Each VTXO conceptually has two keys:
1. **Signing key** (`P_s`): Ephemeral, used for VTXT MuSig2 branch signing
   during the round only. Provides privacy (prevents cross-round linkability)
   and security isolation (compromise doesn't affect fund ownership).
2. **Output key** (`P_v`): Long-lived, used in the VTXO output script for
   collaborative spending (OOR, forfeit) and unilateral exit.

**Privacy note:** Because the same signing key `P_s` is used in all VTXT
branch nodes from root down to the leaf for a given VTXO, an observer who
sees the VTXT can link a signing key to a specific VTXO within that round.
However, since signing keys are ephemeral and change each round, this does
not enable cross-round linkability.

### Boarding Keys

Boarding keys are used in boarding UTXOs for entering the Ark.

**Requirements:**
1. MUST be derived from the HD wallet.
2. Each boarding UTXO SHOULD use a unique key.
3. Must be retained until boarding completes or times out.

**Derivation Path (RECOMMENDED):**
```
m/ark'/<coin_type>'/<account>'/2/<index>

Where change=2 indicates boarding keys
```

### Key Backup

Clients MUST ensure:

1. **Seed backup**: The master seed is securely backed up.
2. **Derivation reproducibility**: All keys can be regenerated from seed.
3. **Recovery process**: A documented process for wallet recovery.

**Warning:** Loss of keys results in permanent loss of funds.

## State Storage Requirements

### Minimum Required State

Clients MUST store sufficient state to:

1. Prove VTXO ownership.
2. Execute unilateral exit if needed.
3. Verify incoming VTXOs.

### VTXO Record

For each owned VTXO, store:

```
VTXORecord:
  // Identity
  vtxo_id: bytes32 (hash of outpoint)
  outpoint: (txid, vout)
  value: uint64 (satoshis)
  owner_pubkey: pubkey

  // Ownership proof
  vtxt_path: [
    {
      tx: SignedTransaction
      output_index: uint32  // Which output this path continues from
    }, ...
  ]  // Path from batch output to VTXO (fan-out)
  batch_tx: SignedTransaction
  batch_id: bytes32

  // Metadata
  batch_sweep_deadline: uint32 (block height)
  creation_height: uint32
  is_confirmed: bool  // Confirmed (VTXT leaf) vs preconfirmed (OOR)

  // For preconfirmed VTXOs
  oor_chain: [
    {
      checkpoint_tx: SignedTransaction
      ark_tx: SignedTransaction
    }, ...
  ]
```

### Storage Requirements by VTXO Type

#### Confirmed VTXO (Direct VTXT Leaf)

```
Required:
  - Batch transaction (signed)
  - VTXT path transactions (signed)
  - VTXO output details
  - Owner key reference
```

#### Preconfirmed VTXO (From OOR)

```
Required (in addition to confirmed requirements):
  - All checkpoint transactions in chain
  - All Ark transactions in chain
  - Origin VTXO's confirmed data (recursive)
```

### Proof of Inclusion

For verification purposes, clients SHOULD store:

```
ProofOfInclusion:
  // For each VTXT transaction
  tx_proof: {
    txid: bytes32
    merkle_proof: [bytes32, ...]  // If commitment tx is deep in chain
  }

  // Alternative: Full blockchain headers
  // (Needed for SPV verification)
```

### Data Retention Policy

Clients MUST retain VTXO data until:

1. The VTXO is spent via OOR transaction AND the batch expires, OR
2. The VTXO is batch-swapped for a new confirmed VTXO AND the old batch expires, OR
3. The VTXO is unilaterally exited and confirmed.

**Warning:** Premature data deletion may result in inability to exit.

## Verification Requirements

### Batch Transaction Verification

When receiving a batch transaction, verify:

1. **Transaction validity**: Valid Bitcoin transaction structure.
2. **Inputs signed**: All inputs have valid signatures (or will after round completes).
3. **Expected outputs**: Contains expected batch outputs, leave outputs, etc.
4. **Value conservation**: Input sum >= output sum.

### VTXT Path Verification

When receiving VTXT path transactions, verify:

1. **Chain validity**: Each transaction spends from the correct parent.
2. **Root anchor**: The root spends from the batch transaction batch output.
3. **Fan-out output index**: Each path entry specifies which output continues
   the path; it MUST not point to the anchor output.
4. **Leaf validity**: The leaf transaction has the expected VTXO output.
5. **Anchor output**: Each VTXT transaction ends with a P2A anchor output.
6. **Script verification**: All output scripts match expected format.
7. **Signatures valid**: All transactions are properly signed.

**Verification Algorithm:**

Note: VTXT nodes are **fan-out** — each transaction has exactly one input
spending the parent output and multiple child outputs (radix determines the
fan-out). The path from root to leaf visits one transaction per tree level.

```
function VerifyVTXTPath(batch_tx, vtxt_path, expected_vtxo):
    // Start from the batch output outpoint
    current_outpoint = (batch_tx.txid, batch_output_index)

    for entry in vtxt_path:
        tx = entry.tx
        out_idx = entry.output_index

        // Verify tx spends from expected outpoint
        assert tx.inputs[0].prev_outpoint == current_outpoint

        // Verify tx signature (MuSig2 keypath)
        assert VerifySignature(tx)

        // Verify anchor output is last and P2A
        assert IsP2AAnchor(tx.outputs[-1])

        // Output index must not point to the anchor
        assert out_idx < len(tx.outputs) - 1

        // Verify branch output script if non-leaf
        if not tx.is_leaf:
            assert VerifyBranchScript(tx.outputs[out_idx])

        // Move to next level (fan-out)
        current_outpoint = (tx.txid, out_idx)

    // Verify final VTXO output (non-anchor output in leaf tx)
    leaf_entry = vtxt_path[-1]
    leaf_tx = leaf_entry.tx
    leaf_out_idx = leaf_entry.output_index

    vtxo_output = leaf_tx.outputs[leaf_out_idx]

    assert vtxo_output.script == ExpectedVTXOScript(expected_vtxo)
    assert vtxo_output.value == expected_vtxo.value

    return true
```

### OOR Transaction Verification

When receiving or constructing OOR transactions:

1. **Input verification**: Each checkpoint input is valid.
2. **Signature verification**: All required signatures are present and valid.
3. **Output verification**: New VTXOs have correct script format.
4. **Value verification**: Output sum <= input sum.

### Incoming VTXO Verification

When receiving a VTXO from another party (see ARK-03 Recipient-Side OOR Flow):

1. **Full chain verification**: Verify the entire chain back to the confirmed
   batch transaction.
2. **Batch confirmation**: Verify the batch transaction is confirmed on-chain.
3. **Operator signature**: Verify operator co-signed all transactions in the
   chain (checkpoint and Ark transactions).
4. **Expiry check**: Verify sufficient time remains before batch sweep deadline.
5. **Authoritative metadata**: Query the operator for round ID, batch txid,
   tree path, and OOR chain depth. Do NOT treat the VTXO as live until this
   metadata is verified.
6. **Chain depth check**: Verify the OOR chain depth is within acceptable
   limits (operator policy may cap chain depth).

**Minimum Data Required:**
- Batch transaction (from operator metadata query)
- VTXT path to origin VTXO (from operator metadata query)
- All checkpoint/Ark transactions to the received VTXO (from transfer
  notification)

Recipients SHOULD batch-swap preconfirmed VTXOs to confirmed status promptly
to reduce trust exposure and monitoring requirements.

### Verification Failures

If verification fails:

1. **Reject the transaction**: Do not sign or proceed.
2. **Log the failure**: Record details for debugging.
3. **Notify user**: Alert about the failed verification.
4. **Consider operator reputation**: Track operator failures.

### Ownership Proofs

**When ownership proofs are required:**
- When **spending** a VTXO (forfeit, batch swap, OOR transaction), the client must prove
  ownership by signing with the VTXO's ownership key.

**When ownership proofs are NOT required:**
- When **creating** a VTXO for another party (sending funds), no ownership proof is needed
  for the destination VTXO. The sender simply specifies the recipient's public key.
- The recipient may not have registered with the operator and discovers their VTXO
  by scanning for outputs matching their keys.

**Optional ownership proofs:**
- Operators MAY require ownership proofs for VTXO creation requests as a spam prevention
  mechanism (e.g., to verify the sender actually controls the inputs being spent).
- This is an operator policy choice, not a protocol requirement.

**Note:** This asymmetry is intentional: spending requires proof (to prevent theft),
while receiving does not (to enable simple payment flows where the recipient may be offline).

## Unilateral Exit Procedure

### Overview

Unilateral exit allows claiming VTXO funds on-chain without operator cooperation.

```mermaid
sequenceDiagram
    participant C as Client
    participant BC as Blockchain

    Note over C,BC: Unilateral Exit Process

    C->>C: Gather all required transactions
    C->>C: Calculate fee requirements

    C->>BC: Broadcast VTXT path (root to VTXO)
    Note over BC: Wait for confirmations

    BC->>C: VTXO now on-chain
    Note over C: Wait for CSV delay

    C->>C: Create unilateral spend transaction
    C->>BC: Broadcast unilateral spend

    BC->>C: Funds in client wallet
```

### Step 1: Gather Required Transactions

Collect all transactions needed for exit:

**For Confirmed VTXO:**
- Batch transaction (if not already on-chain)
- VTXT path transactions from root to VTXO leaf

**For Preconfirmed VTXO:**
- All of the above for origin VTXO
- All checkpoint transactions
- All Ark transactions
- Repeat for any cross-batch inputs

### Step 2: Calculate Fees

Estimate fees for all transactions:

```
total_fee = sum(tx_size * fee_rate for tx in transaction_chain)
             + final_spend_tx_size * fee_rate
             + safety_margin
```

**Fee considerations:**
- Each transaction in chain needs fees.
- Use current mempool fee rate.
- Include buffer for fee volatility.
- May need to use anchor outputs for fee bumping.

### Step 3: Broadcast Transaction Chain

Broadcast transactions in order (following the fan-out path):

1. Batch transaction (if not confirmed).
2. VTXT branch transactions (root to leaf).
3. For preconfirmed VTXOs, interleave checkpoint and Ark transactions in
   chain order: `checkpoint_1 → ark_1 → checkpoint_2 → ark_2 → ...`
   Each checkpoint must confirm before the next Ark transaction can be
   broadcast, since the Ark transaction spends the checkpoint output.

**Important:** For OOR chains deeper than 1, the broadcasts MUST follow
the alternating checkpoint→Ark pattern. Broadcasting all checkpoints first
and then all Ark transactions would fail because each Ark transaction
spends from its preceding checkpoint output.

**For each transaction:**
```
function BroadcastChain(transactions):
    for tx in transactions:
        // Check if already on-chain
        if IsConfirmed(tx.txid):
            continue

        // Broadcast
        BroadcastTransaction(tx)

        // Wait for confirmation (optional, or batch broadcast)
        WaitForConfirmation(tx.txid)
```

### Step 4: Wait for CSV Delay

After the VTXO appears on-chain:

1. Record the block height of VTXO confirmation.
2. Wait for `csv_delay` blocks.
3. Only then can the unilateral spend be broadcast.

```
spend_eligible_height = vtxo_confirmation_height + csv_delay
```

### Step 5: Create and Broadcast Unilateral Spend

Create the final transaction claiming funds:

```
Unilateral Spend Transaction:
  Version: 2
  Locktime: 0

  Inputs:
    - VTXO outpoint
      sequence: csv_delay (enables CSV check)
      witness: <signature> <unilateral_script> <control_block>

  Outputs:
    - Destination output (client wallet)
```

**Witness construction:**
1. Sign with VTXO owner key.
2. Include the unilateral exit script.
3. Include the taproot control block.

### Fee Bumping During Exit

If transactions are not confirming:

1. **Use anchor outputs**: Create child transaction spending anchor with higher fee.
2. **RBF if supported**: Replace with higher fee version.
3. **Monitor mempool**: Watch for confirmation.

### Exit Timing Requirements

**CRITICAL:** Unilateral exit MUST complete before batch sweep deadline.

```
required_time = chain_length * avg_confirmation_time
              + csv_delay * avg_block_time
              + safety_margin

assert current_height + required_time < batch_sweep_deadline
```

If insufficient time remains:
- **Alert user immediately**
- **Use aggressive fees**
- **Consider partial exit** (if multiple VTXOs)

## Operational Procedures

### Batch Swap Timing

Clients SHOULD batch swap well before expiry:

```
recommended_swap_time = batch_sweep_deadline - (csv_delay * 2) - safety_margin
```

### Monitoring

Clients SHOULD monitor:

1. **Batch expiry**: Track expiry of all owned VTXOs.
2. **Operator status**: Verify operator remains responsive.
3. **Blockchain**: For SPV clients, track relevant transactions.

### Preconfirmed VTXO Handling

When receiving preconfirmed VTXOs:

1. **Verify immediately**: Don't accept without full verification.
2. **Batch swap soon**: Convert to confirmed VTXO when possible.
3. **Limit exposure**: Cap total value in preconfirmed state.

### Backup Procedures

Clients SHOULD:

1. **Regular backups**: Back up wallet state periodically.
2. **After each operation**: Backup after receiving new VTXOs.
3. **Multiple locations**: Store backups in multiple secure locations.

### Cooperative Leave (`LeaveVTXOs`)

When the operator is online and the client wishes to exit one or more
VTXOs to on-chain destinations, the client SHOULD use the
`LeaveVTXOs` RPC (see ARK-06) rather than performing a unilateral
exit. Unilateral exit MUST remain available, but cooperative leave:

1. Avoids the per-VTXO CSV exit delay.
2. Avoids broadcasting the full VTXT path.
3. Lets the client specify per-outpoint destinations.

The client MUST:

1. Construct a `LeaveVTXOsRequest` carrying a `LeaveVTXODestination`
   per outpoint, with a fresh owner proof binding the request to the
   client.
2. Generate a fresh idempotency key for the request and persist it
   alongside the in-flight state, so a retransmit after a transport
   failure does not double-admit.
3. Reconcile the per-outpoint admission results in the response. If
   any outpoint is rejected (e.g. `VTXO_LOCKED`), the client SHOULD
   retry that outpoint after the lock is released, using the same
   idempotency key.

### Receive-Script Allocation (`NewReceiveScript`)

When constructing a destination for an OOR transfer or a directed
send, the client MUST request a fresh script via the
`NewReceiveScript` RPC (see ARK-06). The same RPC backs both
flows; the response carries the P2TR pkScript and the owner pubkey
the client controls.

The client SHOULD NOT reuse a receive script across multiple incoming
transfers. Each fresh allocation prevents linkability between
unrelated payments.

### TRUC + P2A Package Broadcast

When the client broadcasts any Ark protocol transaction directly
(unilateral exit, OOR package, sweep), the broadcaster MUST:

1. Construct the parent as a TRUC (`nVersion=3`) zero-fee tx with a
   single ephemeral P2A anchor as the final output (per ARK-01).
2. Fund the package via a CPFP child from the client's on-chain
   wallet, normalizing the witness per ARK-01
   [CPFP Child Witness Normalization](ARK-01-transactions.md#cpfp-child-witness-normalization).
3. Submit parent + child as a v3 package via the client's chain
   backend (`submitpackage` against bitcoind, or equivalent).

## Security Considerations

### Key Security

1. **Key isolation**: Keep signing keys separate from hot wallet.
2. **Hardware security**: Use hardware security modules where possible.
3. **Signature verification**: Verify all signatures before broadcasting.

### Operator Trust

1. **Verify everything**: Never trust operator data without verification.
2. **Track reputation**: Monitor operator behavior over time.
3. **Diversify**: Consider using multiple operators for large holdings.

### Exit Capability

1. **Always maintain exit capability**: Never lose ability to exit unilaterally.
2. **Test exits**: Periodically verify exit procedures work.
3. **Fee reserves**: Maintain on-chain funds for exit fees.

### Privacy Considerations

1. **Key reuse**: Avoid reusing keys across VTXOs.
2. **Transaction linking**: Be aware of linkability through on-chain transactions.
3. **Operator knowledge**: Operator knows all VTXO ownership.

### Attack Awareness

Be aware of potential attacks:

1. **Fee exhaustion**: Attacker tries to drain your on-chain funds via repeated unroll attempts.
2. **Timing attacks**: Attacks timed near batch sweep deadline when exit is difficult.
3. **Collusion**: Operator collusion with malicious parties.

## References

1. BIP-32: Hierarchical Deterministic Wallets - https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki
2. ARK-00: Protocol Overview and Terminology
3. ARK-01: Transaction Formats and Script Specifications
4. ARK-03: Out-of-Round Transactions

## Authors

This specification was authored by the Lightning Labs team.

## Copyright

This document is licensed under CC0.
