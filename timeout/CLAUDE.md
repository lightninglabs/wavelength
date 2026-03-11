# timeout

## Purpose

Generic fire-and-forget timeout scheduling actor. Sends `ExpiredMsg` to a
callback when a scheduled timeout fires.

## Relationships

- **Depends on**: `baselib/actor` (actor framework).
- **Depended on by**: `round` (forfeit collection timeouts, registration timeouts).
