# batchschedule

## Purpose

UTC arithmetic for scheduled batches: the operator's anchored timetable
(`Schedule`), the client's immutable view of published slots (`Published`), and
the slot identity signed into a join (`ID`, `Selection`). Every instant is a
whole UTC second held as `time.Time`; Unix seconds appear only at the wire and
database boundaries. Design and flow: `docs/scheduled_batches.md`.

## Key Types

- `Schedule` — `anchor + n*interval` cutoffs with a registration window. `Next`
  returns the first cutoff strictly after an instant; `At` validates that a
  cutoff lies on the grid. Its `ID` hashes the anchor, interval, and window.
- `Published` — a validated list of at most 32 ordered, non-overlapping slots.
  `Next` selects only listed cutoffs and returns `ErrScheduleExhausted` instead
  of extrapolating. It carries the operator's clock reading and, once attached
  by the caller, the estimated clock offset.
- `ID` — opaque 32-byte timing-policy identity (`IDFromBytes`, `IsZero`,
  `String`).
- `Selection` — schedule ID plus cutoff; `Matches` compares instants with
  `time.Time.Equal`, and `SelectionFromUnix`/`CutoffUnix` convert the wire form.
- `EstimateClockOffset`, `WakeBounds` — offset from one discovery round trip,
  and the `[lo, hi]` range after a window opens in which a client sends.

## Invariants

- Registration is `[Opens, Cutoff)`. The cutoff starts quoting; it promises
  nothing about broadcast or confirmation.
- Windows are whole seconds from ten seconds to five minutes, no longer than
  the interval for generated schedules. Published lists may have gaps.
- The identity preimage format is fixed; changing it invalidates every
  outstanding selection.
- `WakeBounds` keeps `lo = 2s` and `hi = window/2` for validated windows.

## Relationships

`arkrpc` converts `BatchSchedule` to and from `Published`. `rpc/roundpb` and the
join-auth TLV codec in `lib/types` encode `Selection`. `round` pins one
selection per attempt, and `waved` times the discovery refresh that feeds the
clock offset.
