-- Persist the payment preimage on the activity row so List and InspectActivity
-- can return the payment credential that L402 and MPP buyers wait on. The
-- column is nullable: rows written before this migration have no preimage until
-- the projector re-projects them, and non-send kinds never carry one.
ALTER TABLE activity_entries ADD COLUMN preimage BLOB;
