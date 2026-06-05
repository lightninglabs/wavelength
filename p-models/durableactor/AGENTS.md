# p-models/durableactor

This package models the durable actor mailbox from distributed-systems first
principles: durable enqueue, lease ownership, retry scheduling, ack/nack token
validation, dead-letter/removal, idempotent delivery identity,
per-correlation-key FIFO, and the Read/Commit consume step (lease-fenced
exactly-once effect application under lease-expiry-during-IO).

## Files

- `infra.pproj` — P project for durable actor infrastructure checks.
- `src/mailbox_fifo.p` — ideal mailbox spec plus claim-ordering profiles.
- `test/mailbox_fifo_test.p` — green conformance tests and separate
  counterexample tests.
- `traces/*.json` — concrete scenarios replayed by the Go bridge.
- `bridge/` — Go conformance harness against the real `db/actordelivery`
  SQLite store and claim SQL.

## Commands

| Command | Purpose |
|---------|---------|
| `./p-models/scripts/check.sh` | Full default check: P model plus Go bridge |
| `p compile -pp p-models/durableactor/infra.pproj` | Compile this model |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxCorrelationKeyFIFO` | Run green durable mailbox tests |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxReadCommitFence` | Run the green Read/Commit exactly-once-effect test |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxLegacyReorderCounterexample` | Demonstrate the old ordering bug |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxUnfencedCommitCounterexample` | Demonstrate the unfenced-commit double-apply bug |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxStageCommitExactlyOnce` | Run the green Stage-then-Commit replay-safety test |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxStagedDoubleBroadcastCounterexample` | Demonstrate the unstable-broadcast double-broadcast bug |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxStaleStageRegressesCounterexample` | Demonstrate the unfenced-stage checkpoint regression bug |
| `go test ./p-models/durableactor/bridge` | Replay traces against Go |

## Modeling Guidance

- Treat the P model as the ideal specification; only add implementation modes
  when they clarify a bug or migration path.
- New correctness properties should usually be expressed both as a direct P
  scenario and as a bridge trace.
- Use `0` as the model's NULL correlation key. Non-zero keys are per-lane
  FIFO domains scoped by mailbox id.
- Keep the default test case green. Put intentional known-bad checks in a
  separate test case with "Counterexample" in the name.
