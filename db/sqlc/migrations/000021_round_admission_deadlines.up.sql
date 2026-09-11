-- Keep admission budgets separate from the signature-bearing round checkpoint.
-- A row here must never count as proof that input signatures were released.
CREATE TABLE round_admission_deadlines (
    round_id TEXT PRIMARY KEY,
    expires_at BIGINT NOT NULL,
    closed BOOLEAN NOT NULL DEFAULT FALSE
);
