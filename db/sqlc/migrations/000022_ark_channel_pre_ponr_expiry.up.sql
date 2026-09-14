ALTER TABLE ark_channels ADD COLUMN pre_ponr_started_at BIGINT;

UPDATE ark_channels
SET pre_ponr_started_at = updated_at
WHERE pre_ponr_started_at IS NULL
    AND phase IN (1, 2)
    AND oor_session_id IS NOT NULL;
