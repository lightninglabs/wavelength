# p-models

## Purpose

Executable P-language state-machine models plus Go bridge tests, used to
check concurrency and crash-recovery invariants that plain unit tests
struggle to cover across independent actors. Currently models the durable
actor mailbox (correlation-key FIFO claim, lease/ack/nack, Stage/Commit
exactly-once).

## Key Types

- `durableactor/` — the P model project (`infra.pproj`) and its traces for
  the durable mailbox: enqueue idempotence, per-mailbox/priority/order lease
  selection, per-correlation-key FIFO blocking, ack/nack/lease-expiry/
  dead-letter, and Stage/Commit replay-safety.
- `durableactor/bridge` — Go package (`mailbox_trace.go` +
  `crash_restart_test.go`, `outbox_fold_test.go`) that replays checked-in
  traces against the real `db/actordelivery` store, keeping the model
  connected to the shipped implementation.
- `scripts/check.sh` — compiles the P project, runs the green test cases
  (must find zero bugs) and the counterexample cases (must find exactly the
  expected bug), then runs the Go bridge tests.

## Relationships

- **Depends on**: `db/actordelivery` (bridge tests replay traces against the
  real delivery store), the external P checker toolchain (`dotnet tool
  install --global P`).
- **Depended on by**: none — this is verification tooling, not imported by
  production code.

## Invariants

- Models state the ideal contract first (e.g. `PerCorrelationKeyFIFO`); a
  known-bad profile (e.g. `LegacyAvailableAtOrder`) is kept only as a
  separate counterexample test case, never mixed into the default green
  suite.
- Every model scenario with a real implementation path gets a bridge or
  trace-replay test, so the P spec cannot silently drift from the Go code.
- `check.sh`'s negative test cases must find the bug they exist to catch; a
  clean run there is itself a regression, not a pass.

## Deep Docs

- [README.md](README.md) — Full P-model background, layout, and running
  instructions.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
