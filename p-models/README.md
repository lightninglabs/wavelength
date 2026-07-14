# P Models

This directory holds executable P models for the parts of the system where
ordinary unit tests have a hard time covering all interesting interleavings.

P is a state machine modeling language for asynchronous systems. A P model is
written as a set of machines that exchange events. The P checker then explores
many possible event schedules, looking for assertion failures, monitor
violations, deadlocks, or other illegal states. This is useful for wavelength
because several important correctness properties are not local to one function:
they depend on the order in which messages are leased, retried, acknowledged,
or observed by independent actors.

The model should be read as an executable specification. It states the
behavior we want first, then checks both the abstract model and bridge traces
against that behavior. When a production bug is found, the goal is to encode
the ideal invariant that would have rejected the bad execution, not just add a
single regression test for the exact SQL row sequence.

## Layout

- `durableactor/` models the durable actor mailbox and the
  per-correlation-key FIFO claim contract.
- `durableactor/bridge/` replays checked-in traces against the real Go
  `db/actordelivery` store so the model stays connected to the shipped
  implementation.
- `durableactor/traces/` stores concrete scenarios shared by the model
  documentation and the Go bridge.
- `scripts/` contains shared entrypoints for compiling and checking models.

## Durable Actor Mailbox

The durable actor mailbox model captures the semantics needed by the mailbox
correlation-key ordering fix:

- enqueue and duplicate enqueue idempotence
- lease selection by mailbox, priority, availability, and row order
- per-correlation-key FIFO blocking
- ack, nack, lease expiry, and dead-letter removal
- lease token ownership
- leaseless single-worker peek with by-ID ack/nack
- retry exhaustion
- independence between different mailboxes and different correlation keys

The model keeps both the old and new claim rules. The old rule,
`LegacyAvailableAtOrder`, demonstrates how a nacked predecessor in backoff can
be overtaken by a later same-key successor. The new rule,
`PerCorrelationKeyFIFO`, captures the intended invariant: a live predecessor
for the same mailbox and correlation key blocks later same-key rows, even when
the predecessor is leased or waiting for retry.

## Running

Run the current green suite with:

```shell
./p-models/scripts/check.sh
```

That script:

1. compiles `p-models/durableactor/infra.pproj`;
2. runs the default P testcase, `tcMailboxCorrelationKeyFIFO`;
3. runs `go test ./p-models/durableactor/bridge`.

To demonstrate that the model would have found the original same-key reorder
bug, run the intentional negative testcase:

```shell
p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll \
  --testcase tcMailboxLegacyReorderCounterexample \
  --schedules 1 \
  --max-steps 200
```

That check runs the ideal same-key FIFO property against the legacy claim rule
and should report one bug. The default `check.sh` suite does not run this
negative testcase because it is expected to fail.

## Adding Models

Keep new models focused on durable distributed-system invariants rather than
implementation details that are already easy to cover with unit tests. Good
model targets include ordering, ownership, retry, crash/restart, backpressure,
and idempotence rules.

When a model has a corresponding implementation path, add a bridge or trace
replay test so the same scenario can exercise the real code. This keeps the P
specification useful as the implementation evolves.
