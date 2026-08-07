-- Rollback dead-letter requeue support.
-- Drops the routing columns added for faithful requeue.

ALTER TABLE dead_letters DROP COLUMN max_attempts;
ALTER TABLE dead_letters DROP COLUMN priority;
ALTER TABLE dead_letters DROP COLUMN correlation_key;
ALTER TABLE dead_letters DROP COLUMN correlation_id;
ALTER TABLE dead_letters DROP COLUMN callback_actor_id;
ALTER TABLE dead_letters DROP COLUMN promise_id;
