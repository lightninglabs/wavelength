DROP INDEX IF EXISTS idx_pending_intent_anchors_intent_id;
DROP TABLE IF EXISTS pending_intent_anchors;
DROP TABLE IF EXISTS pending_send_intents;
DROP TABLE IF EXISTS pending_board_intents;
DROP INDEX IF EXISTS idx_pending_intents_kind;
DROP TABLE IF EXISTS pending_intents;
DROP TABLE IF EXISTS pending_intent_kinds;

-- Restore the pre-generalization Board-specific outbox table exactly as
-- 000013_pending_board_request created it.
CREATE TABLE IF NOT EXISTS pending_board_requests (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    target_vtxo_count INTEGER NOT NULL DEFAULT 0,

    requested_at_unix BIGINT NOT NULL,

    PRIMARY KEY (outpoint_hash, outpoint_index),

    CHECK (target_vtxo_count >= 0),
    CHECK (requested_at_unix > 0)
);
