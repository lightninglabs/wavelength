# Finite canonical activity reads

An app processing new receipts needs a saved position and a finite stopping
point. Reading the latest UI page can miss an older completion. A live
subscription alone has no end-of-replay frame. An incoming transport hint is
not evidence of completed payment.

`wavewalletdk.Client` exposes two embedded-only reads:

- `ActivityCursor(ctx)` captures the retained transition-log boundary. A new
  notification consumer uses it to ignore historical receipts **after** the
  host has completed its separate reconciliation pass.
- `ReadActivityBatch(ctx, ActivityBatchRequest)` reads a finite range of event
  positions and returns their **current canonical projections**. The first
  request can use `ThroughCursor = 0` to capture an upper boundary in the same
  database snapshot as its page. Reuse `HighWater` on subsequent pages.

```go
page, err := client.ReadActivityBatch(ctx, wavewalletdk.ActivityBatchRequest{
    AfterCursor: savedCursor, ThroughCursor: savedHighWater, Limit: 100,
})
// Handle err before advancing any cursor.
// Apply app policy and durable canonical-ID deduplication to page.Entries.
// Persist page.NextCursor, even if all entries were suppressed.
// If page.HasMore, continue through page.HighWater in another bounded call.
```

The limit is 1 through 256. The SQL query reads at most `limit + 1` transition
positions. Every result joins its current activity row. It never interprets an
old transition's serialized JSON as current truth. Removing an internal
projection also removes its events, so it cannot reappear. If reconciliation
deletes the newest retained event rows, a consumer's position still never moves
backwards. Future inserted events keep their monotonic sequence.

`HighWater` freezes the replay range, not the current values between pages.
Several transitions may return the same canonical ID. Deduplicate receipt
presentation by that identity. `Entry.Cursor` names its selected transition
position; `NextCursor` covers the full scanned range, including gaps and
filtered entries. Persisting only the last presented entry's cursor is wrong.

The mobile binding exposes `ActivityCursor()` as `{"cursor":number}` and
`ReadActivityBatch(requestJSON)` with this outer contract:

```json
{"after_cursor":0,"through_cursor":0,"limit":100}
```

```json
{"high_water":42,"next_cursor":42,"has_more":false,"entries":[]}
```

Entries retain the existing native SDK `Entry` JSON shape. Cursor fields are
JSON numbers, including zero. Remote `Connect` handles return
`ErrActivityReplayUnavailable`; existing List and Subscribe RPCs keep their
contracts. Builds without the wallet runtime keep compiling with their normal
unavailable decoder.

The native callback waits for wallet-service startup to publish the store.
Readiness does not certify projector catch-up. Both reads join the SDK's direct
callback admission, cancellation and final-access ownership. Closing a client
cancels them and waits before releasing the daemon.

These APIs do not query remote recipient feeds, reconcile pending receipt
sources, schedule notifications or prove safe suspension. Exact amount alerts
still need a completed canonical receive/deposit, presentation policy and
persistent deduplication. Their baseline must follow successful source
reconciliation, rather than startup readiness or mailbox emptiness.
