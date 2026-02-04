# Rounds Persistence

This document describes the database persistence layer for the rounds package, including schema design, serialization strategies, and operational semantics.

## Overview

The rounds persistence layer provides durable storage for round state and VTXO lifecycle management. It replaces the previous mock-based implementation with SQLite/PostgreSQL-backed stores that maintain consistency across server restarts.

The implementation consists of two main components:
- **RoundStoreDB**: Persists complete round data including trees, descriptors, and client registrations
- **VTXOStoreDB**: Manages VTXO lifecycle states and locking for concurrent forfeit operations

## Schema Design

The schema is split across two migrations:
- **Migration 000002** (`rounds.up.sql`): Round lifecycle tables
- **Migration 000003** (`vtxos.up.sql`): VTXO lifecycle and forfeit tracking

### Round Lifecycle Tables

#### `round_statuses`
Enum table defining valid round states.

```sql
CREATE TABLE IF NOT EXISTS round_statuses (
    status TEXT PRIMARY KEY
);

INSERT INTO round_statuses (status) VALUES ('pending'), ('confirmed');
```

**States:**
- `pending`: Round finalized but commitment transaction not yet confirmed on-chain
- `confirmed`: Commitment transaction confirmed, round complete

#### `rounds`
Main round data including the finalized commitment transaction.

```sql
CREATE TABLE IF NOT EXISTS rounds (
    round_id BLOB PRIMARY KEY NOT NULL,
    final_tx BLOB NOT NULL,
    commitment_txid TEXT NOT NULL UNIQUE,
    confirmation_height INTEGER,
    confirmation_block_hash BLOB,
    status TEXT NOT NULL DEFAULT 'pending',
    sweep_key BLOB NOT NULL,
    csv_delay INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (status) REFERENCES round_statuses(status)
);

CREATE INDEX idx_rounds_status ON rounds(status);
CREATE INDEX idx_rounds_txid ON rounds(commitment_txid);
```

**Key fields:**
- `round_id`: UUID identifying the round (16 bytes)
- `final_tx`: Wire-serialized commitment transaction
- `commitment_txid`: Hex-encoded txid string for confirmation tracking
- `confirmation_height`/`confirmation_block_hash`: Set when confirmed on-chain
- `status`: Current round state (defaults to 'pending')
- `sweep_key`: Operator sweep public key (compressed 33 bytes)
- `csv_delay`: Relative CSV delay used to build sweep scripts

#### `round_vtxo_tree`
Marker rows for each VTXO tree (one per batch output).

```sql
CREATE TABLE IF NOT EXISTS round_vtxo_tree (
    round_id BLOB NOT NULL,
    batch_output_index INTEGER NOT NULL,
    PRIMARY KEY (round_id, batch_output_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);
```

**Purpose:** Tracks the existence of a tree for a given batch output. The
structure is stored in the recursive tree tables below.

#### Recursive VTXO Tree Tables

Tree structure is stored in normalized tables:

```sql
CREATE TABLE IF NOT EXISTS vtxo_tree_nodes (...);
CREATE TABLE IF NOT EXISTS vtxo_tree_node_outputs (...);
CREATE TABLE IF NOT EXISTS vtxo_tree_cosigners (...);
```

**Purpose:** Stores tree topology, outputs, and cosigner keys for selective
queries (descendants, leaves, cosigner lookups).

**Serialization:** Uses `SerializeTreeRecursive` to write nodes and
`DeserializeTreeRecursive` to rebuild the in-memory tree.

#### `round_connector_descriptors`
Metadata for connector tree construction.

```sql
CREATE TABLE IF NOT EXISTS round_connector_descriptors (
    round_id BLOB NOT NULL,
    output_index INTEGER NOT NULL,
    num_leaves INTEGER NOT NULL,
    forfeit_script BLOB NOT NULL,
    PRIMARY KEY (round_id, output_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);
```

**Purpose:** Provides information needed to reconstruct connector trees for forfeit transactions.

**Design:** Each row represents one connector output/tree. The `output_index` uniquely identifies each connector within a round, serving as the natural primary key.

**Fields:**
- `output_index`: Commitment tx output index (uniquely identifies this connector tree)
- `num_leaves`: Number of leaves in the connector tree
- `forfeit_script`: Script used in forfeit transactions

#### `round_client_registrations`
TLV-encoded client registration data.

```sql
CREATE TABLE IF NOT EXISTS round_client_registrations (
    round_id BLOB NOT NULL,
    client_id BLOB NOT NULL,
    registration_data BLOB NOT NULL,
    PRIMARY KEY (round_id, client_id),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE
);
```

**Purpose:** Preserves complete client registration information including boarding inputs, leave outputs, VTXO descriptors, and forfeit inputs.

**Serialization:** TLV-encoded `ClientRegistration` struct containing all
registration details. The registration TLV includes an explicit version tag
(currently `1`).

### VTXO Lifecycle Tables

#### `vtxo_statuses`
Enum table defining valid VTXO states.

```sql
CREATE TABLE IF NOT EXISTS vtxo_statuses (
    status TEXT PRIMARY KEY
);

INSERT INTO vtxo_statuses (status) VALUES
    ('pending'), ('live'), ('locked'), ('forfeited'), ('spent');
```

**State transitions:**
```
pending → live → locked → (forfeited | spent)
           ↓              ↑
           └──────────────┘ (unlock on failure)
```

**States:**
- `pending`: VTXO created but commitment tx not confirmed
- `live`: Commitment confirmed, VTXO can be used
- `locked`: Reserved for forfeit in a specific round
- `forfeited`: Reclaimed by operator via forfeit tx
- `spent`: Consumed in a new round or exit tx

#### `vtxos`
VTXO tracking with explicit descriptor fields.

```sql
CREATE TABLE IF NOT EXISTS vtxos (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    round_id BLOB,
    batch_output_index INTEGER,
    amount BIGINT NOT NULL,
    pk_script BLOB NOT NULL,
    cosigner_key BLOB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    locked_by_round_id BLOB,
    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id),
    FOREIGN KEY (status) REFERENCES vtxo_statuses(status)
);

CREATE INDEX idx_vtxos_status ON vtxos(status);
CREATE INDEX idx_vtxos_locked ON vtxos(locked_by_round_id)
    WHERE locked_by_round_id IS NOT NULL;
```

**Design decisions:**
- **Nullable `round_id`/`batch_output_index`**: Supports future virtual transactions not tied to specific rounds
- **Explicit descriptor fields**: Avoids TLV deserialization for common queries
- **Indexed lock tracking**: Fast lookup of VTXOs locked by a specific round

#### `round_forfeit_infos`
Forfeit transaction metadata.

```sql
CREATE TABLE IF NOT EXISTS round_forfeit_infos (
    round_id BLOB NOT NULL,
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    forfeit_tx BLOB NOT NULL,
    connector_output_index INTEGER NOT NULL,
    leaf_index INTEGER NOT NULL,
    PRIMARY KEY (round_id, outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id) ON DELETE CASCADE,
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES vtxos(outpoint_hash, outpoint_index)
);
```

**Purpose:** Links forfeited VTXOs to their forfeit transactions and connector
tree positions.

**Uniqueness:** A unique index enforces a single forfeit info per outpoint
across all rounds.

**Foreign keys:**
- `rounds`: CASCADE delete when round deleted
- `vtxos`: Ensures forfeit info references valid VTXO

## Serialization Strategy

### TLV Encoding

TLV (Type-Length-Value) encoding is used for all complex Bitcoin/tree types:
- `tree.VTXODescriptor` - VTXO leaf specifications (used within ClientRegistration)
- `rounds.ClientRegistration` - Complete registration data
- `waddrmgr.Tapscript` - Taproot script information

**Note:** Connector descriptors are stored as individual SQL columns rather than
TLV-encoded blobs, as they contain only simple scalar fields.

**Benefits:**
- Forward compatibility: Unknown TLV types can be ignored
- Extensibility: New fields can be added without breaking existing data
- Type safety: Structured encoding prevents serialization errors

**Implementation:**
- Uses `github.com/lightningnetwork/lnd/tlv` package
- Codec functions in `db/rounds_codec.go`
- Comprehensive test coverage in `db/rounds_codec_test.go`

### Recursive Tree Storage

VTXO trees are normalized into relational tables to support selective queries:
- `vtxo_tree_nodes`
- `vtxo_tree_node_outputs`
- `vtxo_tree_cosigners`

These are serialized via `SerializeTreeRecursive` and reconstructed with
`DeserializeTreeRecursive`.

### Wire Serialization

Bitcoin transactions use wire protocol serialization:
- `wire.MsgTx` - Commitment and forfeit transactions
- Standard Bitcoin format for compatibility

### Fixed-Length Encodings

Simple types use fixed-length binary encoding:
- `wire.OutPoint` - 36 bytes (32-byte hash + 4-byte index)
- `chainhash.Hash` - 32 bytes
- Public keys - 33 bytes compressed

## RoundStore Operations

### PersistRound

Atomically persists a complete round in a single transaction:

```go
func (r *RoundStoreDB) PersistRound(ctx context.Context,
    round *rounds.Round) error
```

**Transaction scope:**
1. Insert main round row
2. Insert all VTXO trees
3. Insert all connector descriptors
4. Insert all client registrations
5. Insert all forfeit infos

**Error handling:** Any failure triggers rollback, preventing partial state.

### LoadPendingRounds

Reconstructs all pending rounds from the database:

```go
func (r *RoundStoreDB) LoadPendingRounds(ctx context.Context) ([]*rounds.Round, error)
```

**Use case:** Called on server startup to resume tracking unconfirmed rounds.

**Process:**
1. Query all rounds with `status='pending'`
2. For each round, load related data (trees, descriptors, registrations, forfeit infos)
3. Deserialize all TLV-encoded fields
4. Reconstruct complete `rounds.Round` struct

### MarkRoundConfirmed

Updates round status and confirmation details:

```go
func (r *RoundStoreDB) MarkRoundConfirmed(ctx context.Context,
    roundID rounds.RoundID, blockHeight int32,
    blockHash chainhash.Hash) error
```

**Effect:** Round moves from `pending` to `confirmed` and is no longer loaded on restart.

## VTXOStore Operations

### PersistVTXOs

Batch insert VTXOs in a single transaction:

```go
func (v *VTXOStoreDB) PersistVTXOs(ctx context.Context,
    vtxos []*rounds.VTXO) error
```

**Use case:** Called after round finalization to create pending VTXOs.

**Atomicity:** All-or-nothing insertion ensures consistency.

### MarkVTXOsLive

Transitions all VTXOs for a round from `pending` to `live`:

```go
func (v *VTXOStoreDB) MarkVTXOsLive(ctx context.Context,
    roundID rounds.RoundID) error
```

**Use case:** Called when commitment transaction is confirmed on-chain.

**Query:** `UPDATE vtxos SET status = 'live' WHERE round_id = ? AND status = 'pending'`

### VTXO Locking

Prevents concurrent forfeit operations across multiple rounds.

#### LockVTXO

Atomically locks VTXOs for a specific round:

```go
func (v *VTXOStoreDB) LockVTXO(ctx context.Context,
    roundID rounds.RoundID, outpoints ...wire.OutPoint) error
```

**Semantics:**
- Only `live` VTXOs can be locked
- Fails if any outpoint is already locked by a different round
- Idempotent: Locking by the same round succeeds
- Sets `status='locked'` and `locked_by_round_id=roundID`

**SQL constraint:**
```sql
UPDATE vtxos
SET status = 'locked', locked_by_round_id = ?
WHERE (outpoint_hash, outpoint_index) = (?, ?)
    AND status = 'live'
    AND (locked_by_round_id IS NULL OR locked_by_round_id = ?)
```

#### UnlockVTXO

Releases locks held by a specific round:

```go
func (v *VTXOStoreDB) UnlockVTXO(ctx context.Context,
    roundID rounds.RoundID, outpoints ...wire.OutPoint) error
```

**Semantics:**
- Only unlocks VTXOs locked by the requesting round
- Idempotent: Unlocking already-unlocked VTXOs succeeds
- Returns VTXOs to `live` status
- Clears `locked_by_round_id`

**Use case:** Called when forfeit operation fails or completes.

### MarkVTXOForfeit

Marks a VTXO as forfeited and stores forfeit metadata:

```go
func (v *VTXOStoreDB) MarkVTXOForfeit(ctx context.Context,
    outpoint wire.OutPoint, info *rounds.ForfeitInfo) error
```

**Transaction scope:**
1. Update VTXO status to `forfeited`
2. Insert forfeit info with transaction details

**Atomicity:** Ensures forfeit status and metadata are always consistent.

## Query Patterns

### Fast Pending Round Lookup

```sql
SELECT round_id FROM rounds WHERE status = 'pending'
```

**Index:** `idx_rounds_status` provides fast filtering.

### Confirmation Callback Lookup

```sql
SELECT round_id FROM rounds WHERE commitment_txid = ?
```

**Index:** `idx_rounds_txid` enables O(1) lookup for blockchain callbacks.

### VTXO Status Filtering

```sql
SELECT * FROM vtxos WHERE status = 'live'
```

**Index:** `idx_vtxos_status` supports efficient filtering by state.

### Lock Tracking

```sql
SELECT * FROM vtxos WHERE locked_by_round_id = ?
```

**Index:** `idx_vtxos_locked` (partial index) provides fast lookup of locked VTXOs.

## Transaction Boundaries

### Critical Atomic Operations

1. **Round Persistence**: All round data must be persisted together or not at all
2. **VTXO Batch Insert**: VTXOs for a round are created atomically
3. **VTXO Locking**: Lock check and acquisition must be atomic
4. **Forfeit Marking**: Status update and metadata insert must be atomic

### Isolation Levels

**SQLite:** Default is `SERIALIZABLE`, preventing dirty reads and write conflicts.

**PostgreSQL:** Uses `READ COMMITTED` by default. Critical sections may need explicit locking.

## Performance Considerations

### Batch Operations

- `PersistVTXOs`: Inserts 100s of VTXOs in single transaction (typically <10ms)
- `MarkVTXOsLive`: Batch update for all round VTXOs (single query)

### Index Strategy

- **Selective indexes**: Only index frequently filtered columns
- **Partial indexes**: `locked_by_round_id` only indexed where NOT NULL
- **Covering indexes**: Status indexes avoid table lookups for status checks

### Large Round Handling

**Tested:** Rounds with 1000+ VTXOs persist in <100ms.

**Considerations:**
- TLV encoding scales linearly
- Tree serialization is O(n) in tree size
- Database I/O dominates for large rounds

## Testing Strategy

### Unit Tests

**Test files:**
- `db/rounds_store_test.go` - RoundStore functionality
- `db/vtxo_store_test.go` - VTXOStore functionality
- `db/rounds_codec_test.go` - TLV codec round-trips

**Coverage areas:**
- Round persistence and loading
- VTXO lifecycle transitions
- Locking semantics (including concurrent tests)
- Error cases (constraint violations, invalid data)
- Codec round-trips for all types

### Integration Tests

**Location:** `rounds/` package tests use real database (replacing mocks).

**Approach:**
- Tests use `db.NewTestDB()` for isolated SQLite instances
- Each test gets a fresh database in a temp directory
- Foreign key constraints verified by test failures

## Migration Strategy

### Adding New Fields

**Schema changes:**
1. Add new migration file (e.g., `000004_add_field.up.sql`)
2. Include default values for existing rows
3. Update `LatestMigrationVersion` in `db/migrations.go`

**Code changes:**
1. Update struct definitions in `rounds/` package
2. Update TLV codecs to include new fields with new type constants
3. Add tests for new field serialization

### Backward Compatibility

**TLV benefits:**
- Old code ignores new TLV types
- New code handles missing TLV types with defaults
- ClientRegistration includes an explicit version tag for format changes

**Wire format:**
- Bitcoin wire format is stable
- Changes require new transaction versions

## Operational Notes

### Database Size

**Typical growth:**
- Small round (10 VTXOs): ~50 KB
- Large round (1000 VTXOs): ~5 MB
- Pending rounds kept until confirmed, then can be archived

### Backup Considerations

**Critical data:**
- Pending rounds must be backed up (required for restart)
- Confirmed rounds can be archived or pruned
- VTXOs transition to historical data after confirmation

### Monitoring

**Key metrics:**
- Number of pending rounds (should be small)
- Number of locked VTXOs (indicates active forfeit operations)
- Database size growth rate
- Query latency for `LoadPendingRounds`

## Future Enhancements

### Potential Optimizations

1. **Pagination**: For rounds with many VTXOs, paginate tree loading
2. **Caching**: In-memory cache for frequently accessed trees
3. **Archival**: Move confirmed rounds to separate archive table
4. **Pruning**: Automatic cleanup of old confirmed data

### Virtual Transaction Support

The schema already supports virtual transactions via nullable `round_id`:
- VTXOs can exist without a round
- Future work: Add virtual transaction tracking
- Schema change: Minimal (just use existing nullable fields)
