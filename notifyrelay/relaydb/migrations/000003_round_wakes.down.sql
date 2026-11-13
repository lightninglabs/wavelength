-- Round wake policy is a forward-only activation boundary.
-- Fail rather than silently discard enrolled authority or pending jobs.
SELECT round_wake_schema_requires_a_forward_fix;
