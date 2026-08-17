# Make receive-script allocation retry-safe

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must stay current
while the work proceeds. Maintain this document according to `PLANS.md` in the
repository root.

## Purpose / Big Picture

After this change, a caller can attach an idempotency key to
`NewReceiveScript`. Repeating the same request, including after the daemon
restarts, returns the same wallet key, taproot script, label, and absolute
registration expiry. A retry never silently extends the registration lifetime.
Calls without an idempotency key keep the existing behavior and allocate a
fresh destination each time.

This matters when a client persists a durable operation before asking
Wavelength for a destination. If the RPC response is lost, the client can retry
without creating another indexer registration. The behavior is demonstrated by
unit tests for exact replay, changed-request rejection, concurrent replay,
registration failure, and restart, plus shared SQLite and PostgreSQL store
tests and migration tests.

## Progress

- [x] (2026-08-17 09:18Z) Audited the existing RPC, key derivation, indexer
  registration, owned-script persistence, transaction layer, and migration
  scheme.
- [x] (2026-08-17 09:40Z) Chose an additive request and persistence contract
  that preserves calls with no idempotency key.
- [x] (2026-08-17 09:29Z) Added protocol fields and migration 17, then
  regenerated RPC and SQL code.
- [x] (2026-08-17 09:38Z) Implemented atomic durable admission and exact replay
  at the artifact-store boundary.
- [x] (2026-08-17 09:53Z) Reordered the RPC path to persist the allocation
  before indexer
  registration and resume incomplete registrations on retry.
- [x] (2026-08-17 09:57Z) Added SQLite, PostgreSQL, restart, concurrency, and
  failure-shape tests.
- [x] (2026-08-17 10:18Z) Ran full SQLite and PostgreSQL-tagged unit suites,
  generation checks, formatting, lint, module checks, documentation audit,
  funds-safety review, and compatibility review.
- [ ] Refresh upstream and recheck migration numbering before push.

## Surprises & Discoveries

- Observation: the current path registers with the indexer before persisting
  local ownership.
  Evidence: `waved/receive_script.go` calls
  `RegisterReceiveScriptTaproot` before `UpsertOwnedReceiveScript` in
  `RegisterOwnedOORReceiveScript`.

- Observation: the current request has only a label, and each call derives the
  next wallet key.
  Evidence: `waverpc/daemon.proto` defines `NewReceiveScriptRequest.label`, and
  `CreateOORReceiveScript` calls `deriveNextKey` before any durable write.

- Observation: all read-write store transactions already use serializable
  isolation and retry serialization failures.
  Evidence: `db/interfaces.go` maps write transactions to
  `sql.LevelSerializable` and `TransactionExecutor.ExecTx` retries recognized
  serialization and deadlock errors.

- Observation: mailbox replay needs one stable key per logical remote
  registration, and two live callers must not share that key.
  Evidence: `mailbox/rpc/AGENTS.md` states both invariants, while
  `serverconn/unary_facade.go` mints a fresh key when callers omit one.

- Observation: the changed-file linter loads all supported build tags and
  therefore checks the SQLite and PostgreSQL variants together.
  Evidence: `make lint-changed-local base=origin/main workers=4` completed
  with zero findings on 2026-08-17 after loading `test_postgres`,
  `test_sqlite`, and `systest`.

## Decision Log

- Decision: keep the idempotency key optional. Empty keys retain the existing
  allocate-on-every-call contract.
  Rationale: existing CLI, SDK, and embedded callers do not need coordination
  with the new durable replay contract.
  Date/Author: 2026-08-17, Codex.

- Decision: treat the normalized label as the caller-controlled immutable
  fingerprint. The daemon selects and persists one absolute expiry.
  Rationale: concurrent first calls can accept the same durable winner without
  comparing independently resolved default expiries. Replay still never
  extends the stored lifetime.
  Date/Author: 2026-08-17, Codex.

- Decision: store the allocation before indexer registration, and retain it if
  registration fails.
  Rationale: retry can safely re-register the exact script. Deleting the row
  would lose the wallet-key evidence and could allocate a second destination.
  Date/Author: 2026-08-17, Codex.

- Decision: derive at most one persisted allocation per idempotency key by
  resolving the unique-key winner in the serializable store transaction.
  Rationale: two concurrent callers may derive local wallet keys, but only one
  key and script become the durable and remotely registered result. The local
  derivation is not a fund-bearing remote registration. Avoiding even that
  unused local key would require a durable lease around an external wallet
  operation and would add recovery states without improving funds safety.
  Date/Author: 2026-08-17, Codex.

- Decision: persist a random mailbox RPC key and registration-completion time.
  Serialize only callers sharing one local idempotency key.
  Rationale: ambiguous remote results retry one logical request, completed
  replay does not depend on the indexer, and unrelated allocations remain
  concurrent.
  Date/Author: 2026-08-17, Codex.

## Outcomes & Retrospective

The implementation now persists one immutable allocation and remote mailbox
key before registration. Completed replay does not contact the indexer.
Pending replay reuses the original script, expiry, label, and mailbox key.

Focused SQLite store, RPC, restart, concurrency, and failure-shape tests pass.
Focused race tests pass. The full unit suite and full PostgreSQL-tagged suite
pass, including the shared store and migration tests. Generated RPC and SQL
outputs are current. Formatting, diff checks, changed-file lint, module tidy,
SQL generation, and commit-message checks pass. An independent funds-safety
and compatibility review found no concrete blocker. Upstream refresh and the
external review remain.

## Context and Orientation

`waverpc/daemon.proto` owns the public daemon RPC. `make rpc` regenerates its
Go, REST, mailbox, CLI schema, and SDK-facing artifacts. The current
`NewReceiveScriptRequest` contains only an optional label.

`waved/rpc_oor_receive.go` implements the RPC. It fetches the current operator
taproot key and exit delay, derives the next wallet key, calls the indexer, and
returns the resulting script. `waved/receive_script.go` contains that derivation
and registration logic. An indexer registration is a server-side binding from a
taproot output script to this wallet. Its absolute expiry controls how long the
server may retain that binding.

`db/oor_artifact_store.go` owns local metadata for wallet-controlled receive
scripts. The SQL schema and queries live under `db/sqlc/migrations` and
`db/sqlc/queries`. `make sqlc` regenerates `db/sqlc/*.go` and
`db/sqlc/schemas/generated_schema.sql`. The current schema version is 16 in
`db/migrations.go`; this plan proposes migration 17, subject to a fresh
upstream check before push.

An exact replay means a request with the same non-empty idempotency key and
normalized label. It returns the same key locator and script even when the
open request count or process state changed. A fingerprint mismatch means the
same key was reused with a different label and must return
`InvalidArgument` rather than silently returning old terms.

## Plan of Work

Add `idempotency_key` to `NewReceiveScriptRequest`, and return the daemon's
resolved `expires_at_unix_s` in the response. Bound non-empty keys to 128 bytes
before database, wallet, terms, or indexer work.

Add nullable replay columns to `owned_receive_scripts`: idempotency key,
registration label, absolute expiry, stable mailbox RPC key, and remote
completion time. Add a unique partial index on
non-null idempotency keys. Old rows and non-idempotent callers leave these
columns null. Add SQL queries for lookup by idempotency key and an insert that
does not overwrite an existing key.

Extend `OwnedReceiveScriptRecord` with optional replay metadata. Add a store
method that runs one serializable transaction. It first looks up an existing
idempotency key. Exact matches return that row. Mismatches return a typed
fingerprint error. If no row exists, it registers the internal wallet key and
inserts the owned script. A unique-conflict race is resolved by loading and
validating the winner. This method is the owning boundary for replay identity.

In the RPC, keep the existing path for empty idempotency keys. For a non-empty
key, first look up the exact durable allocation. If none exists, derive a wallet
key, build the script, mint one mailbox request key, and admit it through the
store. Then register the stored script with the indexer using its stored
expiry, label, and mailbox key. Mark completion only after acknowledgement.
Pending state remains if registration fails. A completed replay returns from
storage without calling the indexer. Return fields from the stored allocation.

Keep the general-purpose `RegisterOwnedOORReceiveScript` behavior compatible
for the daemon's existing default-script and recovery paths. Add a narrow
idempotent allocation helper instead of changing unrelated callers.

## Concrete Steps

Work from `/Users/bhandras/work/ark/wavelength-idempotent-receive-scripts`.

Edit `waverpc/daemon.proto`, add migration
`db/sqlc/migrations/000017_idempotent_receive_scripts.{up,down}.sql`, update
`db/migrations.go`, and extend `db/sqlc/queries/oor_artifacts.sql`. Run:

    make rpc
    make sqlc

Implement the store and RPC changes, then format changed Go files:

    make fmt-changed

Run focused tests while iterating:

    go test ./waved ./db
    go test -race ./waved ./db

Run the PostgreSQL-tagged shared tests when the repository fixture is
available:

    go test -tags=test_postgres ./db

Before proposing the change, run the repository checks from
`docs/testing-guide.md`, the Go documentation audit from the product-work
skill, and the full unit suite. Record exact results in this plan.

## Validation and Acceptance

The new store tests must prove that an exact replay returns the same key and
script; a changed label is rejected; two concurrent first calls
select one durable winner on SQLite and PostgreSQL; and reopening the database
preserves the winner.

The RPC tests must prove that a first registration failure leaves the durable
allocation intact and a retry registers that same script; an exact replay does
not derive another key; a changed fingerprint does not call the indexer; and a
call without an idempotency key still derives a fresh key each time.

Migration tests must prove a version-16 database upgrades to version 17 without
changing existing owned-script rows and that both supported backends enforce
the unique replay key.

Acceptance requires generated-code checks, focused tests, race tests, full unit
tests, formatting, lint, documentation audit, and an explicit funds-safety and
backward-compatibility review to pass.

## Idempotence and Recovery

RPC and SQL generation are deterministic and may be rerun. Migrations are
additive. The down migration removes only the new index and columns.

If the process stops after durable admission but before indexer registration,
the next exact request loads the stored allocation and retries registration.
If registration succeeds but the response or local completion write is lost,
the same request reuses the same script and mailbox key. Remote deduplication
collapses the retry to the original logical registration. The stored absolute
expiry never moves forward. A failed or ambiguous indexer call never deletes
the allocation.

The canonical repository checkout and every pre-existing worktree remain
untouched. This work is isolated on branch `agent/idempotent-receive-scripts`
in its dedicated worktree.

## Artifacts and Notes

Initial base:

    origin/main 3c0672d26e144e1d481fc8be06bcca23bcb09fbc
    schema version 16

The public change is an independent retry-safety primitive. Its documentation,
commits, and pull request must not refer to any private downstream repository or
incident.

## Interfaces and Dependencies

At completion, `waverpc.NewReceiveScriptRequest` has an optional
`idempotency_key` field. The response reports the daemon-resolved absolute
`expires_at_unix_s`.

`db.OwnedReceiveScriptRecord` carries optional registration replay metadata.
`db.OORArtifactPersistenceStore` exposes lookup and atomic admission by
idempotency key. A typed fingerprint error allows the RPC layer to map a reused
key with changed immutable terms to gRPC `InvalidArgument`.

The implementation adds no new external dependency. It uses the existing
serializable transaction executor, internal-key registry, indexer client, and
clock seams.

Plan revision note: created 2026-08-17 after the initial source and funds-safety
audit. The plan records the chosen backward-compatible API and persist-before-
registration ordering.

Plan revision note: updated 2026-08-17 after mailbox-contract review. The plan
now records the stable remote request key, completion evidence, keyed local
serialization, daemon-owned expiry, and correct PostgreSQL test tag.
