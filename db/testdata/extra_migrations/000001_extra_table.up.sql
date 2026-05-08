-- Extension migration fixture used by TestExtraMigrationsSqlite.
-- The schema is intentionally trivial — the test asserts that the table
-- materializes after applyExtraMigrationsSQLite runs and that the
-- schema_migrations_<Name> tracker is populated.
CREATE TABLE IF NOT EXISTS extra_migrations_test_table (
    id INTEGER PRIMARY KEY,
    note TEXT NOT NULL
);
