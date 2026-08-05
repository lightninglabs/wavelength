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
  per-correlation-key FIFO claim contract, plus the connection actor's
  ingress cursor fold and its dispatch deferral against bounded in-memory
  mailboxes.
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

## Ingress Dispatch

The ingress models cover the connection actor's pull-dispatch-checkpoint loop,
which is where an envelope stops being a mailbox row and becomes a delivery to
a local actor.

`durableactor/src/ingress_fold.p` states the cursor contract: the persisted
cursor may never cover an envelope whose local enqueue did not durably commit.

`durableactor/src/ingress_deferral.p` states the delivery contract the fold
model abstracts away. The dispatch table has four kinds of destination and
only one of them is transactional, so a rollback undoes some deliveries and
not others — and one of the destinations is a fixed-capacity in-memory mailbox
that can refuse a message outright. The model says a full target must produce
a deferral rather than a blocking send, that the committed cursor stops at the
undelivered envelope, that a replayed transaction body does not re-send what
it already handed over, and that a request answered ahead of the commit is
answered at most once per process lifetime however many times the redrive
re-pulls it. The pre-fix implementation is reachable through a configuration
profile, so each fix has a counterexample test case that removes it and fails.

## Running

Run the whole suite with:

```shell
./p-models/scripts/check.sh
```

That script:

1. compiles `p-models/durableactor/infra.pproj`;
2. runs every green test case, each of which must find zero bugs;
3. runs every counterexample test case, each of which must find the bug it
   exists to catch — a clean run there fails the script, because a model that
   no longer detects its own failure mode is worthless;
4. runs `go test ./p-models/durableactor/bridge`.

To see one of the counterexamples on its own, for instance the original
same-key reorder:

```shell
p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll \
  --testcase tcMailboxLegacyReorderCounterexample \
  --schedules 1 \
  --max-steps 200
```

That check runs the ideal same-key FIFO property against the legacy claim rule
and should report one bug.

## Adding Models

Keep new models focused on durable distributed-system invariants rather than
implementation details that are already easy to cover with unit tests. Good
model targets include ordering, ownership, retry, crash/restart, backpressure,
and idempotence rules.

When a model has a corresponding implementation path, add a bridge or trace
replay test so the same scenario can exercise the real code. This keeps the P
specification useful as the implementation evolves.
