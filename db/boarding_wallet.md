# Boarding Wallet Database Schema

## Purpose

The boarding wallet persistence layer stores boarding addresses and tracks the
lifecycle of boarding intents from address creation through VTXO conversion.
This enables the Boarding Wallet Actor to recover state after restarts,
deduplicate UTXO detections, and provide query interfaces for balance
calculations and address management.

## Schema Overview

The boarding wallet schema consists of three tables:

1. **boarding_statuses**: Enum-like table defining the five possible boarding
   intent lifecycle states.

2. **boarding_addresses**: Stores generated boarding addresses with their
   cryptographic material (keys) and monitoring metadata. The tapscript is
   reconstructed on read from the stored component fields.

3. **boarding_intents**: Tracks individual boarding attempts from on-chain
   confirmation through round completion.

## Entity Relationship Diagram

```mermaid
erDiagram
    boarding_statuses ||--o{ boarding_intents : "enforces"
    boarding_addresses ||--o{ boarding_intents : "has"

    boarding_statuses {
        BIGINT id "PK"
        TEXT status_name "UK"
    }

    boarding_addresses {
        BLOB pk_script "PK"
        TEXT address_string
        BLOB client_pubkey
        INTEGER client_key_family
        INTEGER client_key_index
        BLOB operator_pubkey
        INTEGER exit_delay
        INTEGER last_confirmed_height "Idx, Default=0"
        BIGINT creation_time
    }

    boarding_intents {
        BLOB outpoint_hash "PK"
        INTEGER outpoint_index "PK"
        BLOB pk_script "FK, Idx"
        BIGINT amount
        INTEGER conf_height "Idx"
        BLOB conf_hash
        BLOB conf_tx
        TEXT status "FK, Idx"
        BIGINT creation_time
        BIGINT last_update_time
    }
```

## Table Details

### boarding_statuses

An enumeration table enforcing valid boarding intent lifecycle states through a
foreign key constraint.

**Lifecycle states**:
- `confirmed` (0): Sufficient confirmations received, ready for round inclusion
- `adopted` (1): Included in a round that has been checkpointed
- `failed` (2): Boarding attempt failed (server rejection, timeout, etc.)
- `expired` (3): CSV timeout expired, funds recoverable via timeout path
- `swept` (4): Funds swept to a new address (either via round or timeout path)

This table is static after migration and enforces type safety at the database
level.

### boarding_addresses

Stores boarding addresses that have been created and imported into the LND
wallet. Each address represents a unique 2-of-2 taproot script between the
client and operator with a CSV timelock for client recovery.

**Key fields**:
- `pk_script`: The raw P2TR output script, serves as primary key since it
uniquely identifies an address

- `client_pubkey`, `client_key_family`, `client_key_index`: The client's key
and its BIP32 derivation path for later signing

- `operator_pubkey`: The operator's public key used in collaborative spends

- `exit_delay`: CSV delay in blocks for the client's unilateral timeout path

- `last_confirmed_height`: The most recent block height at which we detected a
UTXO at this address, used for restart recovery to resume monitoring from the
last known checkpoint

**Tapscript reconstruction**: The tapscript is not stored directly. Instead, it
is reconstructed on read using `scripts.DefaultVTXOTapScript(clientPubkey,
operatorPubkey, exitDelay)`. This avoids JSON serialization complexity and
ensures the tapscript is always consistent with the stored parameters.

**Indexes**:
- `idx_boarding_addresses_last_confirmed`: Enables efficient queries during
startup to identify addresses needing monitoring from specific heights

**Usage pattern**: Addresses are created once and reused. Multiple boarding
intents can reference the same address if a user sends funds to it multiple
times.

### boarding_intents

Tracks individual boarding attempts from on-chain confirmation through
completion. Each intent represents one boarding UTXO's journey through the
round coordination process.

**Key fields**:
- `outpoint_hash`, `outpoint_index`: Composite primary key uniquely identifying
the boarding UTXO

- `pk_script`: Foreign key to `boarding_addresses`, linking this intent to its
address

- `amount`: Value of the boarding UTXO in satoshis (stored as BIGINT for
precision)

- `conf_height`, `conf_hash`: Confirmation block height and hash

- `conf_tx`: Optional serialized confirmation transaction (for auditing)

- `status`: Current lifecycle state (foreign key to `boarding_statuses`)

- `creation_time`: Unix epoch timestamp when intent was first created

- `last_update_time`: Unix epoch timestamp of the last status change

**Indexes**:
- `idx_boarding_intents_pk_script`: Enables efficient lookup of all intents for
a given address

- `idx_boarding_intents_status`: Supports queries like "fetch all confirmed
intents"

- `idx_boarding_intents_conf_height`: Enables backlog delivery by height range

**Upsert semantics**: Intents are inserted with `ON CONFLICT` handling to
enable progressive updates. When an intent is re-inserted:

- `status` is always updated (allows progression through lifecycle)
- `amount` uses COALESCE to preserve non-zero values
- `conf_height`, `conf_hash`, `conf_tx` use COALESCE to preserve once set
- `last_update_time` is always updated to track modifications

## Operational Logic

### Address Creation Flow

1. Wallet actor derives a new key using `DeriveNextKey(family=42)`
2. Constructs 2-of-2 tapscript with operator key and CSV timelock using
   `scripts.DefaultVTXOTapScript`
3. Imports tapscript into LND via `ImportTaprootScript`
4. Persists to `boarding_addresses` with `last_confirmed_height=0`
5. Returns address to caller

**Restart recovery**: On startup, `ListAllBoardingAddresses()` retrieves all
addresses, reconstructs their tapscripts, and loads `last_confirmed_height`
values, enabling the actor to resume monitoring from known checkpoints.

### UTXO Detection Flow

1. On each new block, wallet actor calls LND's `ListUnspent`

2. Filters results to only UTXOs paying to boarding addresses (checks
   `pk_script` against `boarding_addresses`)

3. Deduplicates using in-memory `fn.Set[UtxoKey]` (loaded from existing intents
   at startup)

4. For new UTXOs, inserts `boarding_intent` with status=`confirmed` and full
   chain info

5. Notifies registered actors (e.g., round actor) via `BoardingUtxoConfirmedEvent`

6. Updates address's `last_confirmed_height` to the confirmation block height

**Deduplication**: The in-memory `seenUtxos` set prevents duplicate
notifications. On restart, this set is repopulated by loading all existing
intents.

### Intent Lifecycle Progression

Typical lifecycle: `confirmed → adopted → swept`

- **confirmed**: UTXO has sufficient confirmations, ready for round inclusion
(wallet actor creates intents in this state)

- **adopted**: Round actor has included this intent in a round and checkpointed
the FSM state

- **failed**: Error occurred (server rejection, timeout, etc.), may be
recoverable via CSV path

- **expired**: CSV timeout has passed, funds can be recovered unilaterally

- **swept**: Funds have been moved (either via successful round or timeout
recovery)

## Constraints and Invariants

**Referential integrity**:
- Every `boarding_intent` must reference a valid `boarding_address` (foreign
key enforced)

- Every `boarding_intent.status` must be a valid status from
`boarding_statuses` (foreign key enforced)

**Uniqueness**:
- Each `pk_script` identifies exactly one boarding address
- Each `(outpoint_hash, outpoint_index)` identifies exactly one boarding intent
- A boarding address can have multiple intents (user sends to same address
multiple times)

**Status transitions**: While not enforced at the database level, the
application logic ensures valid state machine transitions. The foreign key to
`boarding_statuses` prevents invalid status strings.

## Performance Considerations

**Index strategy**:
- `pk_script` indexed in `boarding_intents` for address-to-intents lookups
- `status` indexed for filtering by lifecycle stage
- `conf_height` indexed for range queries and backlog delivery
- `last_confirmed_height` indexed in `boarding_addresses` for startup recovery

**Upsert optimization**: Using `ON CONFLICT DO UPDATE` with COALESCE prevents
overwriting already-set fields, reducing transaction contention and preserving
partial updates.

**Read-heavy pattern**: Most operations are reads (balance queries, address
lookups). The `BatchedTx` pattern allows specifying read-only transactions for
optimal performance.

## Future Enhancements

1. **Archival**: Complete intents could be moved to an archive table after a
   configurable time to reduce active table size.

2. **Pruning**: Addresses with no pending intents and `last_confirmed_height`
   older than a threshold could be pruned.

3. **Metrics**: Add triggers or application-level metrics to track status
   transition rates and identify stuck intents.

4. **Address expiry**: Add an `expiry_height` field to `boarding_addresses` to
   automatically stop monitoring old addresses.
