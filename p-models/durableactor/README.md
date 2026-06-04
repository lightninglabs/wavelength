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
idempotent enqueue, ack deletion, dead-letter removal, and the durable actor's
Read/Commit consume step (`eDurableMailboxCommit`). The test suite proves that
the legacy profile permits a same-key overtake after a nack/backoff, while the
new profile blocks same-key overtakes without blocking other keys, other
mailboxes, or unkeyed rows.

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
- **Keep `created_at` unique within a lane.** The claim order mirrors the SQL
  `ORDER BY m.priority DESC, m.available_at ASC, m.created_at ASC`. The model
  adds a final `id` tie-breaker only for determinism (the SQL leaves
  equal-`created_at` ties unordered). Giving each row a distinct `created_at`
  keeps the model and the SQL congruent and avoids relying on that fallback.

## Run

```shell
./p-models/scripts/check.sh
```

The script compiles `durableactor/infra.pproj`, checks
`tcMailboxCorrelationKeyFIFO`, then runs the Go bridge tests.

To demonstrate that the model would have found the original bug, run:

```shell
p check PGenerated/PChecker/net8.0/MailboxInfraModels.dll \
  --testcase tcMailboxLegacyReorderCounterexample \
  --schedules 1 \
  --max-steps 200
```

That intentionally checks the ideal same-key FIFO property against the legacy
available-at claim profile and should report one bug.
