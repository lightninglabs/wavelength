-- batch_canonicality is the durable, reorg-aware record of how each batch
-- (commitment) transaction is faring against the best chain. It is keyed by
-- the batch txid: identity is by txid, never by (txid, block hash), so a
-- reorg that re-mines the same batch in a different block is the same row.
--
-- Effective (absolute) expiry is intentionally NOT stored. The row keeps the
-- CSV-relative delta plus the current confirmation height; the effective
-- expiry is derived as confirmation_height + csv_expiry_delta and is therefore
-- recomputed on every (re)confirmation rather than frozen at first
-- confirmation. Expiry is never persisted as a one-way terminal fact.
CREATE TABLE IF NOT EXISTS batch_canonicality (
    -- batch_txid is the 32-byte commitment transaction id and primary key.
    batch_txid BLOB NOT NULL CHECK (length(batch_txid) = 32),

    -- state is the interpreted canonicality state (batchcanon.State):
    --   0 = unseen
    --   1 = provisional
    --   2 = finalized
    --   3 = reorged_out
    --   4 = conflict_provisional
    --   5 = conflict_finalized
    -- Values are append-only and must never be renumbered.
    state INTEGER NOT NULL DEFAULT 0,

    -- confirmation_height is the best-chain height at which the batch tx is
    -- currently observed confirmed. NULL means the batch is not currently
    -- confirmed (unseen or reorged out). A reorg clears it; a reconfirmation
    -- sets it to the new height.
    confirmation_height INTEGER,

    -- confirmation_block_hash is the hash of the block currently confirming
    -- the batch tx. It is an observation attribute only and is NOT part of
    -- the batch identity. NULL when not currently confirmed.
    confirmation_block_hash BLOB
        CHECK (confirmation_block_hash IS NULL
            OR length(confirmation_block_hash) = 32),

    -- csv_expiry_delta is the batch's CSV-relative expiry timeout, in blocks.
    -- Combined with confirmation_height it yields the effective expiry.
    csv_expiry_delta INTEGER NOT NULL,

    -- policy_state is a reserved policy classification slot
    -- (batchcanon.PolicyState); 0 = default. The data-model layer persists
    -- and round-trips it but assigns no business meaning.
    policy_state INTEGER NOT NULL DEFAULT 0,

    -- created_at / updated_at are unix timestamps.
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    -- confirmation_pk_script is the scriptPubKey of the confirmed batch
    -- output, needed to re-register the reorg-aware confirmation watch after
    -- a restart: light-client backends (neutrino, Esplora) filter conf
    -- watches by pkScript, so a txid alone is insufficient. NULL on rows
    -- created by the descriptor backfill (no batch-output pkScript to
    -- derive); those fall back to a txid-only re-registration. Kept last so
    -- the generated model column order matches the query/store code.
    confirmation_pk_script BLOB,

    PRIMARY KEY (batch_txid)
);

-- Index supporting "find every batch in a given state" (e.g. all provisional
-- batches the manager must re-check for finality).
CREATE INDEX IF NOT EXISTS idx_batch_canonicality_state
    ON batch_canonicality(state);

-- batch_consumed_inputs records the outpoints each batch tx spends, so the
-- canonicality manager can watch every consumed input for a conflicting
-- spend.
CREATE TABLE IF NOT EXISTS batch_consumed_inputs (
    batch_txid BLOB NOT NULL CHECK (length(batch_txid) = 32),
    input_hash BLOB NOT NULL CHECK (length(input_hash) = 32),
    input_index INTEGER NOT NULL CHECK (input_index >= 0),

    -- input_pk_script is the scriptPubKey of the spent output. It is
    -- required to register the reorg-aware spend watch: lnd's spend
    -- notifier filters by output script, so a bare outpoint is rejected
    -- ("an output script must be provided"). Persisting it lets restart
    -- reconciliation re-arm every watch. NULL only on legacy rows that
    -- predate script tracking.
    input_pk_script BLOB,

    -- conflicting / conflict_final persist the last observed conflict
    -- status of this input (a spend by a tx other than the batch itself),
    -- 0 = false, 1 = true. They let restart reconciliation rebuild the
    -- per-input conflict view: without them, a reconciled conflict batch
    -- whose confirmation is re-observed before its conflicting spend is
    -- re-observed would transiently derive back to (non-conflict)
    -- provisional and briefly admit the coin. Default 0: a freshly recorded
    -- input has seen no conflict yet.
    conflicting INTEGER NOT NULL DEFAULT 0,
    conflict_final INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (batch_txid, input_hash, input_index),
    FOREIGN KEY (batch_txid)
        REFERENCES batch_canonicality(batch_txid) ON DELETE CASCADE
);

-- Index supporting input-conflict detection: given an outpoint, find every
-- batch that consumes it (two batches consuming the same outpoint conflict).
CREATE INDEX IF NOT EXISTS idx_batch_consumed_inputs_outpoint
    ON batch_consumed_inputs(input_hash, input_index);

-- batch_dependent_vtxos records the VTXO outpoints anchored by each batch.
-- Their derived availability follows the batch's canonicality. There is
-- intentionally no FK to vtxos: a batch may anchor VTXOs the local wallet
-- does not own or persist.
CREATE TABLE IF NOT EXISTS batch_dependent_vtxos (
    batch_txid BLOB NOT NULL CHECK (length(batch_txid) = 32),
    vtxo_outpoint_hash BLOB NOT NULL CHECK (length(vtxo_outpoint_hash) = 32),
    vtxo_outpoint_index INTEGER NOT NULL CHECK (vtxo_outpoint_index >= 0),

    PRIMARY KEY (batch_txid, vtxo_outpoint_hash, vtxo_outpoint_index),
    FOREIGN KEY (batch_txid)
        REFERENCES batch_canonicality(batch_txid) ON DELETE CASCADE
);

-- Index supporting "given a VTXO outpoint, which batch anchors it".
CREATE INDEX IF NOT EXISTS idx_batch_dependent_vtxos_vtxo
    ON batch_dependent_vtxos(vtxo_outpoint_hash, vtxo_outpoint_index);

-- batch_provisional_consumers is the reverse-dependency table that lets a
-- provisionally consumed VTXO be restored if its consumer batch never becomes
-- canonical (e.g. a round-2 forfeit whose commitment tx is reorged out must
-- restore the round-1 VTXO it consumed). Each row says "consumed_vtxo is
-- provisionally consumed by consumer_batch".
CREATE TABLE IF NOT EXISTS batch_provisional_consumers (
    consumed_vtxo_hash BLOB NOT NULL CHECK (length(consumed_vtxo_hash) = 32),
    consumed_vtxo_index INTEGER NOT NULL CHECK (consumed_vtxo_index >= 0),
    consumer_batch_txid BLOB NOT NULL
        CHECK (length(consumer_batch_txid) = 32),
    created_at BIGINT NOT NULL,

    PRIMARY KEY (
        consumed_vtxo_hash, consumed_vtxo_index, consumer_batch_txid
    ),
    FOREIGN KEY (consumer_batch_txid)
        REFERENCES batch_canonicality(batch_txid) ON DELETE CASCADE
);

-- Index supporting "given an invalidated consumer batch, list the VTXOs to
-- restore".
CREATE INDEX IF NOT EXISTS idx_batch_prov_consumers_batch
    ON batch_provisional_consumers(consumer_batch_txid);
