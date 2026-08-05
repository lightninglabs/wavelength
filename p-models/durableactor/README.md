# Durable Mailbox P Models

This directory contains a focused P model for the durable actor mailbox
ordering rule introduced by the per-correlation-key FIFO claim path.

The model is intentionally infrastructure-only. It captures the claim inputs
that matter for the bug:

- mailbox id
- UUIDv7-like row id ordering
- optional correlation key
- priority
- `available_at`
- `lease_until`
- `attempts` / `max_attempts`
- `created_at`

`src/mailbox_fifo.p` keeps both the old available-at ordering profile and the
new per-correlation-key FIFO profile. It also includes a stateful
`DurableMailboxSpec` machine with token ownership, lease expiry, nack retry,
idempotent enqueue, ack deletion, dead-letter removal, the leaseless
single-worker peek path, and the durable actor's Read/Commit consume step
(`eDurableMailboxCommit`). The test suite proves that the legacy profile
permits a same-key overtake after a nack/backoff, while the new profile blocks
same-key overtakes without blocking other keys, other mailboxes, or unkeyed
rows.

### Leaseless peek consume

`eDurableMailboxPeekNext`, `eDurableMailboxAckByID`, and
`eDurableMailboxNackByID` model `SingleWorkerLeaseless`: one runtime owner
drains a mailbox with a read-only peek instead of a lease-grant write. Peek
uses the same eligibility and ordering rule as `LeaseNext`, but it does not
write a lease token, deadline, or attempt increment. The actor-layer delivery
must therefore carry `NoLeaseToken()` even if the persisted row still has stale
expired lease metadata from an older leased claim.

The by-ID nack is the matching failure edge: because peek did not pre-increment
attempts, nack increments attempts while clearing any stale lease metadata. The
green `TestDurableMailboxSpec_LeaselessPeekMasksStaleLease` scenario exercises
the case that caught the review bug: a row is leased once, its lease deadline
passes without a maintenance cleanup, then the leaseless path peeks it as an
empty-token delivery and nacks it by ID.

### Read/Commit consume step

`eDurableMailboxCommit` models the durable actor's Read/Commit execution path
(`baselib/actor`): a behavior does its side-effect IO outside the writer
transaction, then Commit folds the behavior effect, the dedup mark, and the
lease-fenced ack into one atomic unit. The model exercises the scenario the
fence exists for: a consumer leases a row and starts IO, its lease expires
mid-IO, and a second consumer reclaims and reprocesses the same row. Under the
fenced design the first consumer's stale Commit is an `ErrLeaseLost` no-op, so
the behavior effect is applied exactly once. The `fenced` flag on the commit
request also selects an unfenced profile for the counterexample, where the
effect is applied regardless of the lease token and a stale consumer
double-applies it under reclaim.

### Stage (early durable write)

`eDurableMailboxStage` models the durable actor's Stage primitive
(`baselib/actor` `Exec.Stage`): a short, **lease-fenced** writer transaction that
advances behavior state *before* the side-effect IO, while the message is only
consumed later by Commit. The unroll actor uses it to persist a sweep
transaction before handing it to txconfirm (persist-before-broadcast). Because
the staged write commits in its own transaction, it survives a later
lease-lost Commit — in the model `checkpoint`/`sweepId` are spec-machine state
that the fenced Commit (which only touches `rows`) never rolls back.

The stage request carries two design knobs. `stable` selects how the broadcast
id is chosen on replay (persisted-and-reused vs freshly-derived). `fenced`
selects whether the checkpoint write validates the lease token first, mirroring
the production Stage that fences on `ExtendLease`. A checkpoint write is an
overwrite (`SaveCheckpoint` replaces the row), so under the fence only the live
lease holder, which holds the newest state, writes, and the checkpoint stays
monotone. An unfenced stale consumer would overwrite a newer checkpoint with an
older level: the lost-update / checkpoint regression the fence prevents.

The scenario the Stage path is checked against is a crash between the Stage and
the Commit: a consumer leases a row, Stages its checkpoint and broadcasts, then
crashes; a second consumer reclaims the same durable row and replays the same
event. The green case also has the stale consumer wake up and try to Stage with
its now-reclaimed token, which the fence rejects. Two counterexamples drive the
two ways it can go wrong: the unstable profile re-derives a fresh broadcast id
on replay (a second distinct broadcast), and the unfenced profile lets the stale
consumer overwrite a newer checkpoint with an older level (a regression).

### Ingress dispatch deferral and redrive

`src/ingress_deferral.p` models the connection actor's ingress dispatch
pipeline (`serverconn/ingress.go`, `dispatch_deferral.go`,
`dispatch_replay.go`). Where `src/ingress_fold.p` treats every dispatch as a
durable enqueue that lives and dies with the write transaction, this model
takes the four destinations apart, because only one of them is transactional:

| Kind | When it lands | Survives a rollback |
|---|---|---|
| `IngressDurableTarget` | inside the write transaction | no |
| `IngressMemoryTarget` | `TryTell` into a fixed-capacity mailbox, from inside the transaction but not in it | yes |
| `IngressNonTxRequest` | answered over the network before the transaction opens | yes, and it left the process |
| `IngressWaiterResponse` | handed to a live in-memory waiter before the transaction | yes |

The bounded in-memory target is the one that can refuse. A full mailbox
defers: the cursor commits at the deferred envelope's own sequence number, so
the prefix is acked and the deferred envelope is not, and the redrive re-pulls
from there with the batch clamped and the served-request watermark applied.

`IngressPipelineConfig` is the profile switch that makes the model falsifiable.
The production profile is `(NonParkingDeferral, track_tx_deliveries = true,
served_watermark = true)`; each counterexample flips exactly one field:

- `ParkingBlockingSend` is the shipped bug — a blocking send into a full
  fixed-capacity mailbox from inside the write transaction, which parks the
  process's only mailbox puller with the database's single writer held, and
  logs nothing.
- `track_tx_deliveries = false` removes `deliveredOutsideTx`, so a store that
  replays its transaction body after a retryable error hands the same envelope
  to the bounded target once per attempt. The durable half replays harmlessly
  because the rollback undid it; the `TryTell` does not.
- `served_watermark = false` removes `redriveState.servedNonTxSeq`, so a
  hoisted request that sits in the pull window while an unrelated actor stays
  wedged is answered once per backoff cycle.

The model states the residual honestly rather than asserting it away: a nonTx
request is answered ahead of the commit, so a crash in between genuinely
repeats it. `IngressNonTxRequestServedOncePerIncarnation` keys on the process
incarnation for exactly that reason, and rejects only a repeat within one
lifetime.

It also declines to flatter the implementation. Removing the redrive clamp
while keeping the watermark breaks no property here, because the second answer
the clamp would prevent is the one the watermark suppresses. The clamp earns
its place as a bound on repeated work, not as a second safety net.

### Spec monitors

P does **not** activate spec monitors globally; each test case attaches the
ones it wants with `assert <spec> in { ... }`.

- `SameKeyFIFOClaimsRespectLiveHead` is the global safety contract. It
  reconstructs the live per-lane row set from the enqueue/claim/removal stream
  and, on every keyed claim, asserts that no earlier-id row in the same
  `(mailbox_id, correlation_key)` lane is still live (present, with retry budget
  remaining). This is stronger than checking that claim ids merely never go
  backwards: the production failure mode (a successor claimed while an earlier
  same-key row sits in nack/backoff) keeps claim ids monotonic, so a
  backwards-only check would pass on the exact bug.
  `tcMailboxMonitorCatchesLegacyReorder` runs the legacy reorder with **no**
  in-machine assertion and is expected to fail solely on this monitor, proving
  it catches the bug on its own.
- `MailboxKeyedWorkEventuallyDrains` is the liveness half of the FIFO
  trade-off: per-key blocking must delay, never permanently starve. The liveness
  driver enqueues a same-key pair plus a cross-key row, then leases-and-acks in
  a loop; a model in which a row could never be claimed would leave the monitor
  hot forever. It is checked by `tcMailboxLiveness`.
- `LeaseFencedCommitAppliesEffectAtMostOnce` is the safety contract for the
  Read/Commit consume step: a row's behavior effect must be applied at most once
  even when its lease expires mid-IO and the row is reclaimed and reprocessed.
  `tcMailboxReadCommitFence` checks the fenced design holds it; the negative
  `tcMailboxUnfencedCommitCounterexample` runs the unfenced profile with no
  in-machine assertion, so the double-apply is raised solely by this monitor.
  This monitor deliberately verifies the lease fence is sufficient *in
  isolation*: it does not model the receiver-side `ON CONFLICT (id) DO NOTHING`
  dedup that production also has as a downstream backstop. That omission is
  intentional (it proves the fence alone enforces exactly-once at the source);
  it is not a claim that the downstream dedup is unnecessary in every flow.
- `StagedEffectAppliedAtMostOnceUnderReplay` is the safety contract for the
  Stage path: across a Stage'd-but-unacked crash and replay, a row must never
  broadcast two *distinct* downstream effects. `tcMailboxStageCommitExactlyOnce`
  checks the stable design holds it; the negative
  `tcMailboxStagedDoubleBroadcastCounterexample` runs the unstable profile with
  no in-machine assertion, so the double-broadcast is raised solely by this
  monitor — the exact failure the persist-before-broadcast / sweep-reuse rule
  prevents.
- `CheckpointAdvancesMonotonically` guards the other half of the Stage
  contract: a staged checkpoint never moves backward. Because every stage write
  is an overwrite, the fence is what keeps it monotone (only the live owner
  writes). `tcMailboxStageCommitExactlyOnce` checks the fenced design holds it;
  the negative `tcMailboxStaleStageRegressesCounterexample` runs the unfenced
  profile with no in-machine assertion, so the checkpoint regression is raised
  solely by this monitor — the lost-update the lease fence prevents.
- `IngressCursorCoversOnlyCommittedEnvelopes` is the ingress cursor contract:
  a persisted cursor never covers an envelope whose local enqueue did not
  commit. `tcIngressFoldNoLoss` checks it; the two negatives
  `tcIngressEagerCursorCounterexample` and
  `tcIngressCheckpointFirstCounterexample` run the two ordering bugs it exists
  to catch.
- `IngressCursorCoversOnlyHandledEnvelopes` is its delivery-side companion,
  strengthened to cover the three non-transactional paths as well: the
  committed cursor may only cover envelopes some target actually received.
  This is what rejects a cursor running past an envelope a full mailbox
  refused, which is how backpressure turns into loss.
- `IngressMemoryTargetNoTxScopedDuplicate` and
  `IngressMemoryTargetAtLeastOnceBounded` bound the duplicates. The first says
  a bounded in-memory target never sees an envelope twice within one folded
  dispatch, however many times the store replays the transaction body; the
  second says redelivery across redrives and crashes happens at most once per
  epoch, so the duplicate count is bounded by how many times the loop went
  around. Both catch the missing `deliveredOutsideTx` record independently,
  via `tcIngressUntrackedRetryDuplicateCounterexample` and
  `tcIngressMonitorCatchesRetryDuplicate`.
- `IngressNonTxRequestServedOncePerIncarnation` bounds the one delivery that
  leaves the process before the commit. Removing the watermark
  (`tcIngressUnwatermarkedServeCounterexample`) gets a second answer sent to a
  request the operator had already had answered.
- `IngressWriterNeverParks` is the headline property: delivery into a bounded
  in-memory mailbox must never block the ingress goroutine, because it is the
  process's only consumer of the remote mailbox and it holds the database's
  single writer while it works. `tcIngressParkedWriterCounterexample` reaches
  the park on the first full mailbox.
- `IngressBacklogEventuallyDrains` is the progress half: deferring must delay
  the stream, never stop it. `tcIngressDeferralLiveness` checks it holds, and
  `tcIngressParkedWriterStarvesCounterexample` runs the same wedge under it so
  the failure also shows up the way an operator sees it — the cursor stops
  advancing and the stream never drains.

The Go bridge in `bridge/` replays JSON model traces from `traces/` against the real
`db/actordelivery` SQLite store. This keeps the P model tied to the SQL claim
implementation rather than only to a handwritten abstraction.

### Trace authoring notes

Two representational details matter when writing P scenarios or bridge traces:

- **Backoff is absolute in P, relative in the bridge.** The P `nack` request
  carries an absolute `available_at` timestamp, while the bridge `nack` op
  carries a relative `retry_after` duration (seconds added to the current
  clock). The two express the same backoff; they are not interchangeable field
  values, so port a scenario by recomputing the delay, not by copying the
  number.
- **Peek always expects an empty token.** Bridge `peek` events assert the
  production `PeekNextMessage` adapter surface, not the raw DB row. Omitting
  `expect_token` means the expected token is empty, matching the actor-layer
  contract even when the row still has stale expired lease metadata.
- **Keep `created_at` unique within a lane.** The claim order mirrors the SQL
  `ORDER BY m.priority DESC, m.available_at ASC, m.created_at ASC`. The model
  adds a final `id` tie-breaker only for determinism (the SQL leaves
  equal-`created_at` ties unordered). Giving each row a distinct `created_at`
  keeps the model and the SQL congruent and avoids relying on that fallback.

## Run

```shell
./p-models/scripts/check.sh
```

The script compiles `durableactor/infra.pproj`, runs every green test case
and every counterexample test case, then runs the Go bridge tests. A
counterexample that finds no bug fails the script.

To demonstrate that the model would have found the original bug, run:

```shell
p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll \
  --testcase tcMailboxLegacyReorderCounterexample \
  --schedules 1 \
  --max-steps 200
```

That intentionally checks the ideal same-key FIFO property against the legacy
available-at claim profile and should report one bug.
