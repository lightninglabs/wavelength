# p-models

This tree contains executable P models and bridge checks. Keep model-specific
files under a named subdirectory such as `durableactor/`; keep shared runner
scripts under `scripts/`.

## Commands

| Command | Purpose |
|---------|---------|
| `./p-models/scripts/check.sh` | Compile the durable actor model, run P checks, then run the Go bridge |
| `p compile -pp p-models/durableactor/infra.pproj` | Compile only the durable actor P project |
| `p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll --testcase tcMailboxCorrelationKeyFIFO` | Run the default durable mailbox P test case |
| `go test ./p-models/durableactor/bridge` | Replay checked-in traces against the real Go store |

## Layout

- `durableactor/` — durable actor mailbox model, tests, traces, and bridge.
- `scripts/` — top-level orchestration scripts.
- `PGenerated/` and `PCheckerOutput/` are generated at repo root and ignored.

## Rules

- Models should encode ideal contracts first, then implementation profiles.
- Keep known-bad or counterexample tests as separate test cases so the default
  suite stays green.
- Bridge tests should exercise real Go code where possible instead of
  reimplementing production semantics.
- Add traces for every model scenario that should be replayed against Go.
