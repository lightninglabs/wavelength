UPDATE boarding_intents
SET status = 'expired'
WHERE status = 'sweep_pending';

DROP INDEX IF EXISTS idx_boarding_sweep_inputs_active_outpoint;
DROP INDEX IF EXISTS idx_boarding_sweep_inputs_outpoint;
DROP INDEX IF EXISTS idx_boarding_sweep_inputs_status;
DROP TABLE IF EXISTS boarding_sweep_inputs;

DROP INDEX IF EXISTS idx_boarding_sweeps_status;
DROP TABLE IF EXISTS boarding_sweeps;

DELETE FROM boarding_statuses WHERE status_name = 'sweep_pending';
