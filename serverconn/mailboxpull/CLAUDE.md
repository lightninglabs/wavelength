# serverconn/mailboxpull

## Purpose

Shared retry and exponential-backoff primitives for mailbox pull loops.
Factors out the reliability semantics needed by both the daemon's
identity-mailbox ingress loop (`serverconn`) and per-swap SDK event
consumers, ensuring uniform backoff behavior across callers.

## Key Types

- `BackoffConfig` — Tunable exponential backoff with jitter. `BaseDelay`
  (default 200 ms) and `MaxDelay` (default 30 s); non-positive values
  select the defaults automatically.
- `DefaultBackoffConfig() BackoffConfig` — Returns production defaults.
- `RetryDelay(cfg, attempt) time.Duration` — Exponential formula:
  `min(base × 2^(attempt−1), max) × U[0.5, 1.0)`. Jitter in [0.5, 1.0)
  spreads concurrent retries.
- `Sleep(ctx, cfg, *attempt)` — Blocks for the next backoff interval,
  increments the caller-owned attempt counter, and respects context
  cancellation. Sharing the attempt pointer across pull cycles lets
  successes reset the counter while failures grow the delay.
- `PullWithRetry(ctx, edge, req, cfg, log) (*mailboxpb.PullResponse,
  error)` — Main retry wrapper. Calls `edge.Pull` repeatedly on
  transport errors with exponential backoff. Key behaviors:
  - Preserves the caller's cursor across retries (`req` is not mutated).
  - Retries only on transport/network errors; response-level failures
    (`resp.Status.Ok == false`) are returned immediately.
  - Checks `ctx.Err()` before issuing a fresh Pull after cancellation,
    avoiding wasted round trips during shutdown.
  - Returns `ctx.Err()` in preference to a transport error when context
    is already done — callers distinguish "daemon shutting down" from
    "endpoint flapping".

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient, PullRequest,
  PullResponse).
- **Depended on by**: `serverconn` (ingress pull loop),
  `sdk/swaps` (per-swap out-event receivers).

## Invariants

- Context cancellation always takes precedence: after a transport error,
  if `ctx` becomes done, `PullWithRetry` returns `ctx.Err()` rather than
  the underlying transport error.
- Cursor is never modified across retries; the caller's state is
  authoritative.
- Status-level errors are not retried; only network/RPC-layer failures
  trigger backoff.
- Zero-value `BackoffConfig{}` automatically selects production defaults
  (200 ms / 30 s).

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
