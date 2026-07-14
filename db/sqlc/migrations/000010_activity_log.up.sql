-- The canonical activity log is the persisted source of truth for the wallet
-- activity feed. It is two tables: activity_entries holds the current state of
-- each operation (one row, updated in place, keyed by a stable canonical id)
-- and backs List; activity_events is an append-only log of lifecycle
-- transitions (one immutable row per transition, ordered by a monotonic
-- event_seq) and backs a resumable SubscribeWallet. See
-- docs/canonical_activity_log_design.md.

-- activity_kinds / activity_statuses mirror the wire enums EntryKind and
-- EntryStatus (rpc/wavewalletrpc/wallet.proto) one-to-one: the id column equals
-- the proto enum integer, so the projection stores int64(entry.GetKind()) and
-- int64(entry.GetStatus()) directly and the foreign keys reject any value that
-- is not a defined wire enum.
CREATE TABLE IF NOT EXISTS activity_kinds (
    id   BIGINT PRIMARY KEY,
    name TEXT UNIQUE NOT NULL
);

INSERT INTO activity_kinds (id, name) VALUES
    (0, 'unspecified'),
    (1, 'send'),
    (2, 'recv'),
    (3, 'deposit'),
    (4, 'exit')
    ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS activity_statuses (
    id   BIGINT PRIMARY KEY,
    name TEXT UNIQUE NOT NULL
);

INSERT INTO activity_statuses (id, name) VALUES
    (0, 'unspecified'),
    (1, 'pending'),
    (2, 'complete'),
    (3, 'failed')
    ON CONFLICT DO NOTHING;

-- activity_entries is the current-state projection. One row per operation,
-- updated in place as the operation advances. canonical_id is the stable
-- cross-restart identity (see design doc 3.2); created_at_unix never changes
-- once the row is inserted, so the feed pages by an immutable key.
CREATE TABLE IF NOT EXISTS activity_entries (
    -- canonical_id is the stable cross-restart identity for the operation.
    canonical_id TEXT PRIMARY KEY,

    kind   BIGINT NOT NULL REFERENCES activity_kinds(id),
    status BIGINT NOT NULL REFERENCES activity_statuses(id),

    -- amount_sat is signed in the wallet convention (positive inbound,
    -- negative outbound), matching WalletEntry.amount_sat on the wire. BIGINT
    -- (not INTEGER) because a bare INTEGER is 32-bit on the Postgres backend
    -- and overflows above ~21.47 BTC.
    amount_sat BIGINT NOT NULL DEFAULT 0,
    fee_sat    BIGINT NOT NULL DEFAULT 0,

    counterparty TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT '',

    -- Lifecycle projection fields the daemon already computes for the wire
    -- WalletEntryProgress. phase / failure_code store the proto enum integers.
    phase          BIGINT NOT NULL DEFAULT 0,
    phase_label    TEXT   NOT NULL DEFAULT '',
    failure_code   BIGINT NOT NULL DEFAULT 0,
    failure_reason TEXT   NOT NULL DEFAULT '',

    -- Settlement handles, populated as the operation confirms. Stored as the
    -- raw bytes the source subsystems use (BLOB), nullable until known.
    payment_hash        BLOB,
    txid                BLOB,
    confirmation_height BIGINT,
    vtxo_outpoint       TEXT NOT NULL DEFAULT '',

    -- Correlation handles back to the source subsystems so the projector can
    -- locate the row to update without re-deriving it. Nullable.
    swap_session_id BLOB,
    ledger_txid     BLOB,
    boarding_addr   BLOB,

    -- request_json is the protojson of the WalletEntryRequest oneof, kept so
    -- the schema stays stable as request shapes evolve.
    request_json TEXT NOT NULL DEFAULT '',

    created_at_unix BIGINT NOT NULL,
    updated_at_unix BIGINT NOT NULL
);

-- The feed is read newest-first. created_at_unix is immutable, so paging by it
-- never skips or duplicates a row that transitions in place; canonical_id
-- breaks ties. The updated_at index serves the legacy ordering during the
-- dual-write transition.
CREATE INDEX IF NOT EXISTS idx_activity_entries_created
    ON activity_entries (created_at_unix DESC, canonical_id);
CREATE INDEX IF NOT EXISTS idx_activity_entries_updated
    ON activity_entries (updated_at_unix DESC, canonical_id);

-- activity_events is the append-only transition log. One immutable row per
-- lifecycle transition, ordered by event_seq. A resumable subscriber replays
-- event_seq > cursor. event_seq is monotonic but NOT contiguous: a failed
-- INSERT still burns a value, so consumers treat any event_seq past their
-- cursor as new and never infer a gap means a dropped event.
CREATE TABLE IF NOT EXISTS activity_events (
    event_seq INTEGER PRIMARY KEY AUTOINCREMENT,

    canonical_id TEXT   NOT NULL REFERENCES activity_entries(canonical_id),
    status       BIGINT NOT NULL REFERENCES activity_statuses(id),
    phase        BIGINT NOT NULL DEFAULT 0,

    -- entry_json is the protojson snapshot of the WalletEntry as emitted at
    -- this transition, so a replaying subscriber needs no second query.
    entry_json TEXT NOT NULL,

    created_at_unix BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_activity_events_canonical
    ON activity_events (canonical_id);
