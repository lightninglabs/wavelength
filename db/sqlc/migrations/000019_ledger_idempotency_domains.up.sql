-- Ledger key reconciliation is implemented as the version-19 Go post-step.
-- The operation prefixes contain binary outpoint payloads, and deriving them
-- in Go keeps the rewrite byte-identical across SQLite and PostgreSQL.
SELECT 1;
