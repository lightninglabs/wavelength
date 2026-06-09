# The OOR Subsystem

Out-of-round (OOR) transfers move VTXOs between participants without waiting for
the next round. A sender hands specific VTXOs to a recipient, the operator
co-signs the checkpoint transactions that bind the transfer, and both sides
update their local wallet state. The whole exchange is off-chain in the happy
path: no block has to confirm anything for the funds to change hands.

This document describes how the client drives an OOR transfer, how it survives a
crash mid-transfer, and why the design runs many transfers at once instead of
one at a time. It is the design-of-record for the per-session refactor; see
**Status** at the end for what has landed.

## One actor per session

Each OOR transfer is a **session**, identified by the Ark transaction id. The
client runs one durable actor per session — an `OORSessionActor` — and a thin
`OORRegistryActor` that spawns those actors, dedups them, and restores the
in-flight ones on boot. This mirrors the `unroll` package, where one
`VTXOUnrollActor` owns one exit and an `UnrollRegistryActor` coordinates them.

The session actor owns one state machine. The state machine is the same
`protofsm` FSM the subsystem has always used; the actor is what changed. A
session advances through a fixed sequence of states — build, sign, submit, the
operator co-signs, sign again, finalize, mark the inputs spent, complete — and
the actor drives that sequence one durable message at a time.

The registry is a coordinator, not a chokepoint. A message for a live session
goes straight to that session's actor, addressed by a deterministic mailbox id
derived from the session id (`ActorIDForSession`). The registry only handles
admission of a new session, dedup of a repeated request, and restore on boot. It
never sits on the hot path relaying every event, because that would re-serialize
exactly what per-session actors exist to parallelize.

The registry's own inbound mailbox is durable. The serverconn ingress loop
acks (and deletes) the operator's envelope as soon as its dispatch Tell
returns, so the first hop after ingress must persist the message before
returning -- otherwise a crash between ingress and the per-session child loses
a server push (a finalize acceptance, a submit acceptance, an incoming hint)
that the operator will never re-send. With the durable inbox, a crash mid-turn
just replays the registry's spawn-and-forward: the forward into the child's
durable mailbox is idempotent, and a duplicate event is a no-op at the FSM.
Boot restore runs as a registry message (`RestoreNonTerminalRequest`) so the
active set is only ever touched on the registry goroutine, serialized with any
backlog the durable inbox redelivers at startup.

Live sessions also get a direct ingress fast path. Each spawned child is
registered with the receptionist under its deterministic per-session key
(`SessionServiceKey`), and the EventRouter's `ResolveKey` hook tells a
session-addressed `DriveEventRequest` straight into the child's durable
mailbox -- one persist instead of two. A miss (first contact, a reaped
session, or a not-yet-restored session after a restart) falls back to the
durable registry, which owns admission, the ownership gate, and the
self-transfer invariant; incoming hints always take the registry path for the
same reason.

## The session row is the single source of truth

Every session has one row in `oor_session_registry`. The row carries two kinds
of fields. The first kind is queryable control-plane state: the direction
(outgoing or incoming), the current phase, an optional idempotency key for
outgoing dedup, and a pending/completed/failed status. You can read a session's
state with a plain SQL query, and the registry restores in-flight sessions by
selecting the non-terminal rows.

The second kind is the resume snapshot: a single opaque blob holding the
material the session must replay byte-for-byte after a restart. The most
important part of that blob is the operator's co-signed checkpoint PSBTs, which
the client cannot recompute on its own and must persist verbatim once it has
them.

Because the row holds the full durable state, the OOR actor does **not** use the
generic actor-delivery `fsm_checkpoints` blob at all. The durable-actor
framework never checkpoints on its own; checkpointing is the behavior's job, and
this behavior writes its own row instead. One row per session, written in the
same transaction that consumes the message, with no second source of truth that
could drift from it.

### Upgrade precondition: no in-flight legacy sessions

The deleted global OOR actor persisted **all** session state in the
`fsm_checkpoints` blob under the `oor-client` actor id (state type
`oor.sessions`). The per-session registry reads session state only from
`oor_session_registry` and never reads that blob, so an upgrade performed while
a legacy session is still in flight would silently abandon it -- including a
session past the point of no return whose operator-co-signed checkpoint PSBTs
live only in the unread blob. To make this impossible to miss, the daemon fails
boot loudly when it finds a non-empty `oor.sessions` checkpoint
(`assertNoLegacyOORCheckpoint` in `darepod/server.go`). **Operators must drain
or complete all OOR sessions on the prior release before upgrading.** A clean
install or an already-cut-over daemon has no such checkpoint, so the guard is a
no-op there.

## The turn: read, do the work, commit

The session actor runs on the durable actor's Read/Commit execution path. A turn
loads the session, does its work with no database writer held, then commits the
new state and the message's consumption together in one short, lease-fenced
transaction. If the lease was lost while the actor worked, the commit rolls back
and the message replays against the already-durable prior state.

The work itself reads straight from the code. The FSM still emits outbox events,
but the actor handles them itself in one shared switch right after driving the
FSM, rather than routing them through a separate outbox-handler object. The
switch is short and you can follow the control flow by reading it:

- **Signing** (Ark, checkpoints) runs inline. The actor calls the signer
  directly and feeds the signed event straight back into the FSM. Signing is
  local and needs no database, so it runs with no writer held. This is what
  removed the old signing-effect actor, which used to take a separate write
  transaction and a round-trip through its own mailbox to do the same work.
- **Transport** (submit, finalize, indexer queries, the receive ack) is the one
  thing that genuinely crosses to another actor: the `serverconn` actor that
  talks to the operator. The actor collects these and enqueues them on the
  durable outbox inside the commit, so the state advance and the intent to send
  are one atomic unit. The outbox publisher delivers them after the commit, and
  the operator's response arrives later as a fresh message — a fresh turn.
- **Local persistence** (mark the inputs spent, persist the finalized package,
  record the spending reservations) joins the same commit transaction. The VTXO
  manager's spend completion joins through the request context, so SQLite sees
  one writer for the turn instead of two transactions contending.
- **Retries** schedule a timer through the timeout actor, handled in the same
  switch.

So the outbox is reserved for what it is good at — durable, exactly-once
delivery to other actors — and everything the session can do itself, it does
itself, in line, where you can see it.

## Outgoing transfer

A `StartTransferRequest` admits a new outgoing session. The actor builds the
deterministic submit package, signs the Ark inputs inline, and commits the
`AwaitingSubmitAccepted` snapshot together with one spending-reservation row per
input and the submit message on the outbox. It then waits.

The operator co-signs and its response returns as a fresh turn. The actor signs
the checkpoints inline and commits the `AwaitingFinalizeAccepted` snapshot —
which carries the verbatim co-signed PSBTs — together with the finalize message.
This is the point of no return: from here the client must resume with
byte-identical co-signed PSBTs, which is why they are in the persisted snapshot.

The finalize acceptance returns as the last turn. The actor marks the consumed
inputs spent, drives the FSM to completion, and commits the finalized package
and the terminal snapshot. After the commit it posts the outgoing-transfer entry
to the ledger.

## Incoming transfer

A receive starts from a lightweight hint at the transport boundary. The actor
records a `ReceiveResolving` snapshot and asks the indexer, as a durable query,
for the full Ark and checkpoint package. The package returns as a fresh turn; the
actor records `ReceiveNotified` and asks the indexer for authoritative metadata.
The metadata returns; the actor materializes the incoming VTXOs into the local
wallet, notifies the VTXO manager so it monitors them, and finally acks the
transfer to the operator. Each server round-trip is a durable query delivered
back as its own turn, so the actor never holds a database transaction across a
network call.

## Crash recovery and exactly-once

Every commit folds three things into one transaction: the session's new
snapshot, any cross-actor outbox messages, and the message's lease-fenced ack
plus dedup mark. State advance and message consumption are therefore one unit. A
crash before a commit redelivers the message, and the actor replays it against
the last durably committed snapshot. Because each advance is idempotent —
monotonic FSM state, deterministic reconstruction, downstream dedup on outbox
ids — replay converges rather than double-spends or double-sends.

## Concurrency

The old design ran one global OOR actor with one mailbox, and it rewrote a
single checkpoint blob holding every session on every mutation. Under a burst of
concurrent receives, that serialized the whole subsystem: every session's work
queued behind one mailbox, and every mutation paid one fsync-bound checkpoint
write whose cost grew with the number of in-flight sessions. Concurrent
throughput came out worse than serial (issue #605).

Per-session sharding removes both. Independent sessions run on independent
mailboxes, so they make progress in parallel, and each writes one small
fixed-size row instead of the whole-map blob. The drip-box benchmarks under
`db/actordelivery/store_bench_test.go` measure the per-write floor this
parallelizes.

## Status

Landed: the `oor_session_registry` store, the per-session `OORSessionActor` with
the complete outgoing flow, and the snapshot/restore bridge, all with tests; the
drip-box benchmarks. In progress: the incoming receive flow on the per-session
actor, the registry coordinator, the daemon cutover that re-points the OOR
service key and deletes the old global actor and the signing-effect actor, the
concurrent-receive systest, and the receive-throughput benchmark.

## See also

- [`oor/CLAUDE.md`](../oor/CLAUDE.md) — package types and invariants.
- [`docs/durable_actor_architecture.md`](durable_actor_architecture.md) — the
  CDC pattern, leases, and recovery the actor builds on.
