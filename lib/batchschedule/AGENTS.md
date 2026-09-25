# batchschedule

## Purpose

Operator-side UTC arithmetic, immutable published opportunities, and
authenticated slot identity for discovery and round registration.

## Invariants

- An interval operator hashes its anchor, interval, and window into its ID.
  Clients treat published IDs as opaque and never reconstruct the timetable.
- `Published` accepts at most 32 ordered, nonoverlapping windows. `Next`
  selects only listed cutoffs and reports exhaustion instead of extrapolating.
- Registration includes opening and excludes cutoff. Cutoff begins quoting;
  it does not promise transaction broadcast or confirmation.
- Durations use whole seconds. Registration is at most five minutes and no
  longer than the interval for generated schedules. Published windows can
  vary and have gaps; durable waiting belongs outside this attempt.
- `Next` returns a strictly future cutoff; `At` validates grid alignment.
- `Selection` binds a schedule ID and cutoff into join authorization.

## Relationships

`arkrpc` publishes eight windows from the operator cursor and validates
received lists independently of interval arithmetic. `lib/types` and
`rpc/roundpb` encode selections. `round` pins one selection per attempt and
rechecks its deadline after preparation; a missed attempt never slides slots.
