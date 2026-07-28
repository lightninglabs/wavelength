# Postgres transaction isolation

This document records the isolation level policy for the Postgres backend,
the reasoning behind it, and the audit that decided where the boundary sits.
The policy itself lives in `txIsolationLevel` in
[`db/interfaces.go`](../db/interfaces.go).

## Current policy

| Transaction | Postgres | SQLite |
|-------------|----------|--------|
| Read-only (`ReadTxOption`) | `REPEATABLE READ`, `READ ONLY` | `SERIALIZABLE` (ignored by the driver) |
| Read-write (`WriteTxOption`) | `SERIALIZABLE` | `SERIALIZABLE` (ignored by the driver) |

SQLite is deliberately left alone. It admits only a single writer, so it is
already effectively serializable and there is nothing to gain. The modernc
driver ignores the requested isolation level outright, so the gate documents
intent rather than dodging an error.

## Why read-only transactions run at REPEATABLE READ

Under Postgres' serializable snapshot isolation, a `SERIALIZABLE` transaction
takes `SIRead` predicate locks and joins the serialization conflict graph even
when it only reads. Such a transaction can suffer a `40001` abort itself, and
it can act as the pivot that causes a concurrent writer to be aborted.

The daemon is overwhelmingly read heavy. A signet measurement found 2,035
transaction commits per second with 99.6% of them changing no rows at all,
against a Postgres instance sitting at 794m of a one core limit largely on
predicate-lock bookkeeping.

A read-only `REPEATABLE READ` transaction reads from a single consistent
snapshot taken when its first statement runs, which is exactly what the read
paths already consume. It takes no predicate locks, can never raise a
serialization failure, and no longer appears in anyone else's conflict graph.

The `READ ONLY` access mode is load bearing rather than decorative. Postgres
only skips predicate lock acquisition for a transaction that is genuinely
declared read only, so the isolation level alone would buy nothing.

An audit of all 80 read-only call sites found no site that writes, and no site
whose body depends on observing a concurrent commit part way through the
transaction. Every polling loop in the tree sits outside its transaction and
reopens per attempt, so no loop can pin a stale snapshot across polls.

### Operator note on long-lived read transactions

Some read transactions are long lived. The ancestry resolver in
[`db/oor_unroll_resolver.go`](../db/oor_unroll_resolver.go) walks a tree, and
several round and VTXO listings iterate large result sets. Each holds one
snapshot for its whole duration, which has two consequences.

Postgres cannot vacuum row versions still visible to an open snapshot, so a
very slow read transaction delays cleanup and can bloat tables. Such a session
also sits `idle in transaction` whenever the daemon is computing between
queries, so `idle_in_transaction_session_timeout` must be generous enough to
cover a full pass or Postgres will terminate the transaction mid-flight. The
same caution applies to `statement_timeout`.

None of these transactions perform network calls or actor sends while open,
so their duration is bounded by database and CPU work rather than by a remote
peer.

## Why read-write transactions stay at SERIALIZABLE

Moving writers to `REPEATABLE READ` would be a much larger change than it
looks, and the measured upside is small. Both halves of that judgement matter.

The upside is small because writers are a rounding error in the workload. If
99.6% of transactions change nothing, then relaxing the remaining 0.4% cannot
recover much of the predicate-lock cost that the read-only change already
removes. The read side was the whole problem.

The risk is large because snapshot isolation withdraws exactly the guarantee
that several write paths lean on. Under `REPEATABLE READ` there are no
predicate locks, so only write-write conflicts on the same row are detected.
Any invariant enforced by reading a set, deciding from what is absent, and
then writing becomes unsound, and the failure is usually silent rather than an
error.

An audit of all 64 write closures found roughly a dozen that genuinely depend
on SSI. That is a far denser hit rate than the comparable audit in lnd, which
found four such shapes across about 340 closures, and several of the sites
here sit in fund-critical paths.

Two structural problems compound this. Under `REPEATABLE READ` the same-row
`40001` becomes the only remaining conflict signal, but
`TxAwareActorDeliveryStore.ExecTx` in
[`db/actordelivery/store_impl.go`](../db/actordelivery/store_impl.go) has no
retry loop, and `TransactionExecutor.ExecTx` skips its own retry loop entirely
when it joins an ambient actor transaction. That ambient path is the normal
case for the ledger, audit, credit and activity stores. So today the signal
that would have to absorb the change is not retried on the dominant code path.

Separately, a lost creation race that SSI reports as a retryable `40001`
becomes a `23505` unique violation under `REPEATABLE READ`, and the retry loop
correctly refuses to retry it. Several write paths perform a plain insert that
can lose such a race.

### No configuration knob

lnd shipped an opt-in enum for the write isolation level. We deliberately do
not, because a knob whose non-default setting silently enables about a dozen
known anomalies is a loaded gun with a safety catch rather than a feature. The
level should become configurable, or simply change, once the paths below are
hardened, not before.

## Write-path audit results

The classification below is what a move to `REPEATABLE READ` would have to
address first. It is recorded here so the work does not have to be redone.
Every one of these is currently masked by SSI, so none is a live bug today.

Shapes: **A** is read-check-then-write or aggregate-then-write. **B** is a
lost creation race that would surface as a non-retryable `23505`. **C** is
write skew between two transactions that each read what the other writes.

| Site | Shape | Note |
|------|-------|------|
| `RootKey` ([`db/macaroons.go`](../db/macaroons.go)) | B | Reads the key, and on a miss generates and plain-inserts a new one. Two racers insert different random keys and the loser gets a non-retryable `23505`. |
| `upsertAncestryPaths` ([`db/ancestry_codec.go`](../db/ancestry_codec.go)), via `SaveVTXO` and `SaveVTXOs` | B | `InsertVTXOAncestryPath` is the only plain insert in its scope, and the round store and VTXO manager are designed to write the same outpoint. |
| `ArmRecovery` ([`db/vhtlc_recovery_store.go`](../db/vhtlc_recovery_store.go)) | A + B | The whole idempotency contract is decided from two reads of absence, followed by an insert with no `ON CONFLICT`. |
| `MarkBoardingSweepInputSpent` ([`db/boarding_sweep_store.go`](../db/boarding_sweep_store.go)) | A | `CountUnresolvedBoardingSweepInputs == 0` gates the resolution cascade. Two racers on different inputs of one sweep can each skip it, leaving the sweep unresolved forever with no error. |
| `MarkBoardingSweepFailed` (same file) | A | Rolls back intents from a snapshot of the input set; an input inserted after the snapshot survives in `pending` and permanently occupies the partial unique index. |
| `CreatePendingBoardingSweep` (same file) | B | The `ON CONFLICT` target is the primary key, which does not cover `idx_boarding_sweep_inputs_active_outpoint`. |
| `CommitState` ([`db/round_store.go`](../db/round_store.go)) | A + C | The orphan sweeps delete on `NOT EXISTS` over `pending_intent_anchors`, inside the point-of-no-return round checkpoint. |
| `UpsertPendingIntent` ([`db/pending_intent_store.go`](../db/pending_intent_store.go)) | C | The retention guard reads `pending_intents.status`, which a different transaction writes. |
| `FailForfeitIntents` ([`db/round_store.go`](../db/round_store.go)) | A | Selects its targets through a snapshot read of the anchor mapping. |
| `UpsertOperation` ([`db/credit_operation_store.go`](../db/credit_operation_store.go)) | B | The `ON CONFLICT` target is `op_id`, which does not cover the partial unique `idx_credit_operations_op_key`. |
| `ProjectEntry` ([`db/activity_store.go`](../db/activity_store.go)) | A | Rescued only accidentally, because the projection happens to write the row it read. |
| `UpsertBinding` ([`db/oor_artifact_store.go`](../db/oor_artifact_store.go)) | A | Preconditions on a `vtxos` row that this closure never writes. |
| `LeaseNextMessage` ([`db/actordelivery/store_impl.go`](../db/actordelivery/store_impl.go)) | A | The per-correlation-key FIFO anti-join is an absence check over a range. A weakening rather than a break, since SSI never guaranteed the ordering either. |

The actor delivery store is otherwise clean. Its schema declares no unique
index other than primary keys, so the entire lost-creation-race class is
structurally impossible there, and its claim queries all update the row their
subquery selected, which stays safe under `REPEATABLE READ`.

### Partial unique indexes

A conflict target that does not match a partial index predicate will not fire,
and the upsert silently degrades to a plain insert. The schema has six partial
unique indexes, so any new `ON CONFLICT` needs checking against this list.

| Index | Table | Predicate |
|-------|-------|-----------|
| `idx_boarding_sweep_inputs_active_outpoint` | `boarding_sweep_inputs` | `status IN ('pending', 'published')` |
| `idx_oor_session_registry_idempotency_key` | `oor_session_registry` | `idempotency_key IS NOT NULL AND status != 2` |
| `idx_client_ledger_idempotent_round` | `ledger_entries` | `round_id IS NOT NULL AND idempotency_key IS NULL` |
| `idx_client_ledger_idempotent_session` | `ledger_entries` | `session_id IS NOT NULL` |
| `idx_client_ledger_idempotent_key` | `ledger_entries` | `idempotency_key IS NOT NULL` |
| `idx_credit_operations_op_key` | `credit_operations` | `status != 2` |

A targetless `ON CONFLICT DO NOTHING` arbitrates over every unique index,
partial ones included, which is why the ledger and UTXO audit inserts are safe
as written.

## What would have to be true to move writers

1. The retry gap closes, so a same-row `40001` is retried on the ambient actor
   transaction path as well as the standalone one.
2. Every **B** site is rephrased as a no-op upsert whose conflict target
   matches the index that can actually fire, or translates the violation into
   a domain-level "already exists".
3. Every **A** and **C** site either folds its decision into a single SQL
   statement, takes an explicit row lock, or is made to touch a shared row so
   that an ordinary write-write conflict replaces the SSI dependency.
4. The `PgError` detail surfacing added alongside this work is used to confirm
   from production logs which paths still abort with an SSI reason code, since
   that is the direct evidence of a remaining dependency.
