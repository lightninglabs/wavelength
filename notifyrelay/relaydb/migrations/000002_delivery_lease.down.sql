DROP INDEX relay_job_expiry;
ALTER TABLE relay_installations DROP COLUMN delivery_lease_until;
ALTER TABLE relay_installations DROP COLUMN delivery_claim_token;
