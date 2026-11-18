ALTER TABLE relay_jobs ADD COLUMN wake_deadline_unix BIGINT NOT NULL DEFAULT 0;
