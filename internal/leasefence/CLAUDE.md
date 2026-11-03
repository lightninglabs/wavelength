# internal/leasefence

## Purpose

Carries a mailbox lease token through `context.Context` for operations whose
public store signature predates lease fencing.

## Key Functions

- `WithToken` attaches the expected lease token to one store operation.
- `Token` reads a non-empty attached token.

## Invariants

- The token is operation-local metadata. Do not retain its context beyond the
  store call.
- Absence preserves the legacy by-ID operation for leaseless single-worker and
  maintenance paths.
- Presence requires the store to validate both the token and live lease in the
  same transaction as the mutation.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent internal package overview.
- [db/actordelivery/CLAUDE.md](../../db/actordelivery/CLAUDE.md) — Durable
  mailbox store invariants.
