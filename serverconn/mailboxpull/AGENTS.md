# serverconn/mailboxpull

## Purpose

Shared retry and backoff primitives for mailbox pull loops. Factors out
the common reliability shape (exponential backoff with jitter, context
cancellation, cursor preservation) so both the persistent identity-mailbox
ingress loop in `serverconn` and per-swap event consumers in `sdk/swaps`
share uniform retry semantics.

## Key Types

- `BackoffConfig` — `BaseDelay time.Duration` (initial increment; zero
  → 200 ms default) and `MaxDelay time.Duration` (cap; zero → 30 s
  default).
- `DefaultBackoffConfig() BackoffConfig` — Returns production defaults:
  200 ms base, 30 s cap.
- `RetryDelay(cfg BackoffConfig, attempt int) time.Duration` —
  Exponential backoff with jitter: `min(base * 2^(attempt-1), max) *
  U[0.5, 1.0)`. Uses `math/rand/v2` (not `crypto/rand`) to spread
  concurrent reconnects.
- `Sleep(ctx context.Context, cfg BackoffConfig, attempt *int)` —
  Increments the counter and sleeps for the next backoff interval;
  returns immediately on context cancellation. Multiple pull cycles
  can share one counter; callers reset it on success.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull`,
  retrying transport errors with backoff until success or `ctx` done.
  Preserves the cursor field across reconnects. Returns `ctx.Err()` on
  cancellation (distinguishing "caller gave up" from "endpoint
  flapping"). Status-level failures (`resp.Status.Ok=false`) are
  returned to the caller; only transport errors trigger retry.

## Relationships

- **Depends on**: `mailbox/pb` (generated `MailboxServiceClient` and
  `PullRequest`/`PullResponse` types), `btclog/v2` (optional logger).
- **Depended on by**:
  - `serverconn` (persistent identity-mailbox pull loop).
  - `sdk/swaps` (per-swap event receiver pull loop via
    `MailboxOutSwapEventReceiver`).

## Invariants

- A nil `log` is treated as `btclog.Disabled`; callers must not pass
  a nil logger and expect output.
- Context cancellation takes precedence: if `ctx` is done while a
  transport error is being retried, `PullWithRetry` returns
  `ctx.Err()` rather than the transport error.
- `RetryDelay` jitter is `U[0.5, 1.0)` — the delay is never less than
  half the base. This guarantees minimum spacing while spreading
  concurrent callers.
- The attempt counter is owned by the caller (`*int`). Callers that
  want a fresh backoff schedule on success should reset it to 0.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
