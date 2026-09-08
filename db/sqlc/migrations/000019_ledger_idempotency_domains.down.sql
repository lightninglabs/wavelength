-- Reversing the key rewrite would recreate the collision this migration
-- repairs. A schema downgrade therefore leaves the corrected rows intact.
SELECT 1;
