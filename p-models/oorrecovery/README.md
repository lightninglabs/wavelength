# OOR Post-Sign Recovery Model

This P project models the durable ownership and terminal transitions of an OOR
transfer after signature persistence. It checks the narrow recovery contract
that prevents a restart from making the same input available to a conflicting
transfer.

The result is model-derived within the configured exploration bounds. It does
not prove transaction validity, signature verification, or database isolation.
Those remain implementation obligations.

## Requirements

- **OOR-RECOVERY-1:** Lock acquisition and signature persistence MUST publish
  one durable authority transition. No observer may see only one durable half.
- **OOR-RECOVERY-2:** A crash and restart MUST retain ownership after signature
  persistence until the transfer reaches its terminal state.
- **OOR-RECOVERY-3:** Conflicting admission MUST be rejected while post-sign
  ownership remains active.
- **OOR-RECOVERY-4:** Finalization and package materialization MUST publish one
  terminal transition. No observer may see only one durable half.
- **OOR-RECOVERY-5:** Replayed notification and acknowledgement MUST NOT apply
  terminal completion more than once.

`AuthorityAndTerminalTransitionsAreAtomic` checks OOR-RECOVERY-1 and
OOR-RECOVERY-4. `PostSignOwnershipSurvivesRestart` checks OOR-RECOVERY-2.
`OwnedConflictIsRejected` checks OOR-RECOVERY-3.
`TerminalCompletionIsIdempotent` checks OOR-RECOVERY-5.

Each unsafe policy has a negative test case. A negative check passes only when
P finds the expected counterexample.

## Normalized Trace

[`traces/post_sign_recovery.json`](traces/post_sign_recovery.json) is a
versioned trace exported for downstream implementation test harnesses. This
repository generates and byte-compares the artifact; it does not replay the
trace against an OOR implementation. Its stable envelope is:

```json
{
  "schema_version": 1,
  "trace_id": "post_sign_recovery",
  "description": "...",
  "producer": {
    "kind": "p",
    "path": "p-models/oorrecovery/src/oor_recovery.p",
    "model": "OORRecoveryModels",
    "testcase": "tcOORBridgeTraceExport",
    "tool_version": "3.0.4",
    "model_sha256": "..."
  },
  "steps": [
    { "op": "submit" },
    { "op": "conflicting_admission", "expect": "rejected" }
  ]
}
```

Version 1 permits only these operation strings, in protocol order:
`submit`, `lock`, `persist_signature`, `disclose`, `crash`, `restart`,
`conflicting_admission`, `finalize`, `materialize`, `notify`, and `ack`.
`expect` is optional and records the state or decision the consumer must
observe after that step.

The canonical positive trace is generated from the P checker's structured
counterexample output. `tcOORBridgeTraceExport` announces each bridge step and
then raises the exact `OOR_BRIDGE_EXPORT_COMPLETE` sentinel. The check script
requires that sentinel, normalizes only `eBridgeStep` announcements, records
the checker and model provenance, and byte-compares the result with the
checked-in file. A green execution produces no counterexample, so this
dedicated export profile makes trace production deterministic without treating
the sentinel as a safety result.

The `lock` and `persist_signature` steps are adjacent observations of one
atomic production transition. The `finalize` and `materialize` steps have the
same relationship. A conforming downstream consumer MUST reject a trace that
splits or reorders either pair; it MUST NOT insert a crash boundary between the
observations.

The two `invalid_split_*` traces are interoperability test vectors derived from
the P testcases that inject crash and restart inside each transition. Their
top-level `expect_validation_error: true` tells a downstream consumer to parse
and reject them before execution. This repository does not execute that
consumer contract; `check.sh` checks the corresponding forbidden schedules in
P and checks only the canonical positive trace for byte equality. The invalid
traces are hand-authored fixtures and do not carry the checker-generated
producer record.

## Bounds and Assumptions

The model has one transfer, one conflicting admission, one owner, and boolean
durable facts. It assumes a durable write either publishes all fields in its
transition or none. Crash clears only process-local pending work. The green
profile runs one deterministic, serial canonical schedule by construction and
replays notification plus acknowledgement once. It does not model concurrent
duplicate acknowledgement delivery; that remains an implementation
obligation. The negative profiles independently restore split authority
writes, reject crash inside either atomic observation pair, release ownership
on restart, admit a conflict, split terminal writes, and repeat terminal
completion.

The producer digest covers `src/oor_recovery.p`. It does not cover the export
driver in `test/oor_recovery_test.p`; the check script pins that driver's
expected operation sequence separately before byte-comparing the normalized
trace.

Run the project through the repository entrypoint:

```shell
./p-models/scripts/check.sh
```

For a focused green check:

```shell
p compile -pp p-models/oorrecovery/infra.pproj
p check PGenerated/PChecker/net8.0/OORRecoveryModels.dll \
  --testcase tcOORRecovery \
  --schedules 100 \
  --max-steps 300
```
