# harness

## Purpose

Docker-based Bitcoin/LND integration test environment for system tests.
Provides test fixtures, node management, and chain control utilities.

## Relationships

- **Depends on**: `lndbackend`, `db`, `clientconn` (test wiring).
- **Depended on by**: `systest` (test environment setup).
