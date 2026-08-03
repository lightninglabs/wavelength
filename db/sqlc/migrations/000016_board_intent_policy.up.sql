-- Add the custom-policy replay parameters to the Board pending-intent detail
-- table. When a Board RPC pins a vtxo_policy_template (for example to board
-- directly into a VTXO owned by an external FROST aggregate key), the template
-- and its optional pinned pk_script must survive a restart so replay recreates
-- the same custom output instead of silently re-boarding into the standard
-- collaborative shape. Both columns are nullable (NULL for board intents
-- persisted before this migration and for the legacy standard-policy path); a
-- NULL template selects the standard collaborative policy with a freshly
-- derived owner key.
ALTER TABLE pending_board_intents
    ADD COLUMN vtxo_policy_template BLOB;

ALTER TABLE pending_board_intents
    ADD COLUMN pk_script BLOB
        CHECK (pk_script IS NULL OR length(pk_script) > 0);
