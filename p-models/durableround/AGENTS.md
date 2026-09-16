# p-models/durableround

## Purpose

Specify the required durable commit and operation ownership boundary for
participant and operator round actors. The current model abstracts their
protocol states; it is not proof of production actor conformance.

## Key Types

- `RoundCommitBoundary` requires checkpoints and outgoing effects before
  acknowledgement, rejects repeated application, and allows repeated sends.
- `RoundExclusiveAttempt` prevents overlapping attempts and premature release
  of uncertain signing work.
- `RoundCommitHistory` varies precommit failure and repeated publication for
  both roles within a bounded history.

## Relationships

- Uses the P checker.
- `../durableactor/bridge/round_commit_test.go` checks the storage primitive
  with production SQLite code and database reopen after each failure point.

## Invariants

- Negative tests must fail on the specific expected assertion.
- Do not equate storage primitive coverage with full actor replay coverage.
- External signing uncertainty cannot be cleared by a timeout alone.

## Deep Docs

- [README.md](README.md) describes the boundary and remaining coverage.
