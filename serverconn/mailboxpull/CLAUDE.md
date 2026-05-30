# serverconn/mailboxpull

## Purpose

Shared retry-with-backoff primitives for mailbox pull loops. Factors out the
identical reliability shape needed by the persistent identity-mailbox ingress
loop in the parent `serverconn` package and by per-swap event consumers in
`sdk/swaps`, so both consumers back off identically when the shared mailbox
endpoint is flapping.

## Key Types

- `BackoffConfig` — Controls exponential backoff with jitter. `BaseDelay`
  (initial increment) and `MaxDelay` (cap). The zero value selects defaults
  (200 ms base, 30 s cap) so callers that don't care about tuning pass
  `BackoffConfig{}` without producing a tight retry loop.
- `DefaultBackoffConfig() BackoffConfig` — Returns the production defaults
  (200 ms / 30 s) matching `serverconn.DefaultConnectorConfig`.
- `RetryDelay(cfg, attempt) time.Duration` — Computes one backoff interval:
  `min(base * 2^(attempt-1), max) * U[0.5, 1.0)`. Non-cryptographic jitter.
- `Sleep(ctx, cfg, attempt *int)` — Increments `*attempt` and sleeps for the
  next interval, respecting context cancellation. The caller owns the attempt
  counter so multiple pull cycles share a single backoff schedule.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull`, retrying
  transport errors with exponential backoff until success or context
  cancellation. Status-level failures (`resp.Status.Ok == false`) are
  returned directly without retry; context cancellation returns the context
  error rather than the underlying transport error.

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient, PullRequest,
  PullResponse); `btclog/v2` (structured logger for retry warnings).
- **Depended on by**: `serverconn` (identity-mailbox ingress loop), `sdk/swaps`
  (per-swap mailbox event consumers).

## Invariants

- Only transport errors are retried. Status-level failures (deterministic
  protocol rejections from the server) are returned to the caller as-is.
- Context cancellation always wins: `PullWithRetry` returns `ctx.Err()`,
  not the last transport error, when the caller gives up.
- A nil logger is treated as `btclog.Disabled`; no nil-check required by
  callers.
- `Sleep` increments the attempt counter before sleeping so the first
  retry uses `attempt=1` (not 0) and the backoff is at least `base/2`.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package that owns the
  identity-mailbox ingress loop.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
