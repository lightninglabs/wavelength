-- Different routes on one device share provider rate limits. A device lease
-- prevents two replicas from claiming different routes before either result
-- updates its quiet period.
ALTER TABLE relay_installations ADD COLUMN delivery_claim_token TEXT NOT NULL DEFAULT '';
ALTER TABLE relay_installations ADD COLUMN delivery_lease_until BIGINT NOT NULL DEFAULT 0;
CREATE INDEX relay_job_expiry ON relay_jobs (expires_at, id);
