-- 000016_unilateral_exit_policy adds durable policy identity columns to
-- unilateral_exit_jobs so policy-specific unroll jobs (e.g. vHTLC recovery)
-- restart with the same final spend policy. The 'standard_vtxo_timeout'
-- default backfills legacy rows that predate the column.
ALTER TABLE unilateral_exit_jobs
    ADD COLUMN exit_policy_kind TEXT NOT NULL
    DEFAULT 'standard_vtxo_timeout';

ALTER TABLE unilateral_exit_jobs
    ADD COLUMN exit_policy_ref TEXT;
