# Host-driven mailbox ingress

`serverconn.Runtime.PumpIngress` lets a host process a bounded number of inbound
mailbox batches using a deadline. It uses the same dispatch and checkpoint
transaction as foreground polling. It is a connector primitive: the daemon and
mobile SDK still start their usual foreground services, and do not yet expose a
complete wallet wake operation.

## Ownership and invocation

Configure the connector, a transactional delivery store, and its dispatch
targets before pumping. Only one foreground poller or pump can own ingress on a
connector at a time. A concurrent invocation returns `ErrIngressBusy` before
loading the checkpoint. This gate is per connector instance; the host must also
prevent two runtimes or processes from opening the same wallet.

`PauseIngress` cancels and joins the current owner, including checkpoint loading
and foreground heartbeat work. After it returns, a host can pump or call
`StartIngress` again. Serialize these mode transitions in the host. `StopIngress`
(and `Runtime.Stop`) also closes admission permanently; subsequent ingress calls
return `ErrIngressStopped`. Repeated stop calls wait for the same owner to exit.

For an already configured runtime, a host wake can use:

```go
runtime.PauseIngress()
ctx, cancel := context.WithTimeout(hostCtx, 2*time.Second)
defer cancel()

result, err := runtime.PumpIngress(ctx, 4)
```

Choose the timeout within the execution window granted by the host. Both a
context deadline and a positive batch limit are required. Each nonempty pull
may contain at most `ConnectorConfig.PullMaxEnvelopes`, which must also be
positive. Responses exceeding that count are rejected before dispatch. The
pump asks for currently queued work with a zero long-poll wait, consumes one
batch for every attempted nonempty dispatch, and does not pull an extra batch
to discover emptiness after reaching the limit. It returns on the first error
or backpressure without sleeping through retry backoff. Authenticated connectors
send one registration heartbeat before the first pull, within the same deadline.

## Results and recovery

A nil error with `MailboxEmpty == false` means the batch limit was reached.
Schedule another invocation. `MailboxEmpty == true` means a successful pull
observed no remote envelopes after pending ACKs were flushed. It is an
observation, not a guarantee that no messages will arrive afterwards.

`Batches` counts dispatch attempts, including a failed or partially committed
batch. It is not a count of completed payments or uniquely delivered events.
`ErrDispatchDeferred` means a local target could not accept more work; let its
actor make progress before retrying. Context cancellation or deadline errors
mean this invocation stopped. Transport and storage errors are returned to the
host for its retry policy. Permanent protocol errors retain the connector's
existing incompatible state and prevent further ingress.

The host never supplies or persists mailbox cursors. Each invocation reloads
its checkpoint and ACKs only work represented by a committed dispatch state.
The fold can atomically commit a successfully delivered prefix when a later
envelope encounters backpressure. A later invocation ACKs that prefix and
continues from the deferred envelope. A failed transaction leaves the old
checkpoint for redelivery. Interruption after commit but before ACK preserves
local work; interruption after remote ACK but before its local checkpoint can
repeat the idempotent ACK. Existing at-least-once rules still apply to waiter
responses and request handlers executed before the transaction.

Some existing routes hand work to in-memory actors. A successful `TryTell` does
not establish durable operation state, even if the ingress cursor subsequently
commits. This pump preserves those existing routing semantics; it does not make
volatile routes crash-safe. Only durable consumer handoffs provide the recovery
guarantee exercised by the SQLite tests. See [mailbox ingress safety](mailbox_ingress_safety.md)
for the consumer boundary. Proving operation-level recovery for every route is
required before treating a pump return as safe wallet suspension.

## Remaining wallet lifecycle work

An empty remote mailbox does not establish that local actors, durable egress,
quarantine recovery, timers, or peer responses are finished. The pump does not
start or stop actor/egress workers, retry previously quarantined records, prune
receipts, unlock storage, or assess signing-key availability. Foreground
maintenance is unchanged. A host that only pumps will need a separate bounded
maintenance policy.

Cancellation is cooperative. The edge transport, SQL driver and dispatchers
must honor their context; the API cannot guarantee a hard wall-clock stop for
uncooperative code. Accepted durable actor work retains its own lifetime after
the pump returns. The next integration step must coordinate actor/egress work
and storage closure under the host's budget, then expose outcomes such as
awaiting a peer or needing unlock through the SDK. Platform wake notifications
can call that scheduler once it exists.

The connector tests cover batch limits, duplicate wakes, pause/resume,
checkpoint-load shutdown, partial dispatch, protocol failures, SQLite rollback,
and reopening after a deadline interrupts ACK. These are local process-recovery
checks; they do not establish power-loss durability or OS background behavior.
