# Read finite current activity batches

This ExecPlan follows `PLANS.md` and describes source implementation only.

## Purpose

Give an embedded host a finite replay boundary and current canonical receipt
rows. A latest-100 UI page is not an exhaustive receipt cursor, and an old
transition snapshot can disagree with a row's present terminal state.

## Progress

- [x] Add SQL queries for a retained event ceiling and bounded current-row join.
- [x] Regenerate query adapters through canonical `make sqlc`.
- [x] Read the ceiling and page inside one database read transaction.
- [x] Preserve monotonic caller progress across deleted internal/tail rows.
- [x] Share the wallet List decoder with Go SDK and gomobile adapters.
- [x] Join direct callbacks under the SDK's existing shutdown owner.
- [x] Exercise more than 100 entries, concurrent additions, current-state
  projection, removed internal rows and deleted-tail progress.

## Design

The canonical store owns query bounds. A first page captures the current event
ceiling in the same read snapshot; later pages reuse it. Query at most one extra
row to decide whether another page remains. Return current activity rows joined
to selected event positions. HighWater bounds positions without promising that
projection values are immutable across different read transactions.

The host persists the explicit NextCursor after applying its own presentation
policy. It cannot infer progress from the last displayed entry because rows may
be filtered, deduplicated or removed. Canonical IDs remain the receipt dedup
identity. A new consumer baselines only after separate source reconciliation.

Waved publishes its store through wallet-services readiness. The Go SDK borrows
it under direct-callback admission. The mobile binding uses a lifecycle-derived
ten-second deadline. Host timeout never releases a still-running callback's
store owner. Remote handles retain their existing RPC surfaces without gaining
a new private database read.

## Validation

Run SQLite and PostgreSQL activity-store suites with the race detector. Page
300 entries under a frozen ceiling; insert new traffic and change one canonical
row after that capture. Remove an internal row and the final retained row.
Verify no omissions in the selected range, no extension into new traffic, the
current row's status and forward-only progress after tail deletion.

Run actual SDK row conversion against a real canonical store, including numeric
mobile JSON cursors and empty arrays. Extend the blocked-final-access shutdown
regression through both new direct callback sites. Run default and tagged SDK,
mobile and wallet packages; changed-source lint/format, generation and docs.

## Compatibility and boundaries

Additive SQL queries and SDK methods only. No migration or RPC protocol change.
Old clients and databases continue normally. The new callbacks are local to an
embedded handle; there is no new remote signing or store access service.

This does not implement event-feed catch-up, a receipt reconciler, presentation
or a host execution window. Do not claim that cursor capture certifies caught-up
history, completed receive or safe suspension. No deployment or binary upload.
