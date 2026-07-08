-- Add the confirmation pkScript to batch_canonicality so the batch
-- canonicality manager can re-register the batch tx confirmation watch after a
-- restart. Light-client backends (neutrino, Esplora) filter confirmation
-- watches by pkScript, so a txid alone is insufficient to re-establish the
-- watch; persisting the watched output's pkScript lets restart reconciliation
-- rebuild every non-final batch's watch. NULL on rows created by the
-- descriptor backfill (which has no batch-output pkScript to derive); those
-- fall back to a txid-only re-registration.
ALTER TABLE batch_canonicality
    ADD COLUMN confirmation_pk_script BLOB;
