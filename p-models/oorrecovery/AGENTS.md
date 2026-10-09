# p-models/oorrecovery

## Purpose

Executable P model and normalized implementation trace for post-sign OOR
ownership, restart, conflicting admission, and terminal completion.

## Key Types

- `OORRecoveryLifecycle` — the abstract post-sign recovery state machine.
- `AuthorityAndTerminalTransitionsAreAtomic` — rejects split durable writes.
- `PostSignOwnershipSurvivesRestart` — retains non-terminal post-sign
  ownership across restart.
- `OwnedConflictIsRejected` — rejects a second admission while ownership is
  active.
- `TerminalCompletionIsIdempotent` — bounds terminal completion to one
  application.
- `post_sign_recovery.json` — the versioned trace for downstream
  implementation replay.
- `invalid_split_*` — expected-invalid contract traces for downstream
  consumer rejection.
- `scripts/normalize_trace.py` — extracts bridge announcements from the P
  checker trace and records model provenance.

## Relationships

- **Depends on:** the P 3.0.4 compiler and checker.
- **Depended on by:** `p-models/scripts/check.sh` and downstream implementation
  trace consumers.

## Invariants

- `lock` plus `persist_signature` is one durable authority transition.
- Post-sign ownership survives crash and restart until terminal completion.
- Conflicting admission rejects while post-sign ownership remains active.
- `finalize` plus `materialize` is one terminal transition.
- Notification and acknowledgement replay applies completion at most once.
- Conforming downstream trace consumers reject split or reordered atomic
  observation pairs.
- The canonical trace must byte-match the deterministic checker export.
- The repository checker does not replay the OOR traces against an
  implementation.

## Deep Docs

- [README.md](README.md) — Requirements, trace schema, bounds, and commands.
- [../README.md](../README.md) — Repository-wide P model guide.
