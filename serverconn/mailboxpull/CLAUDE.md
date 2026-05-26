# serverconn/mailboxpull

## Purpose

Shared retry and exponential-backoff primitives for mailbox pull loops.
Factors out the common transport-reliability shape used by both the persistent
identity-mailbox ingress loop in the parent `serverconn` package and by
per-swap event consumers in the SDK, so backoff semantics stay uniform across
all consumers.

## Key Types

- `BackoffConfig` — Exponential-backoff tuning: `BaseDelay` and `MaxDelay`.
  The zero value selects package defaults (200 ms base, 30 s cap) so callers
  that do not care about tuning can pass `BackoffConfig{}` without creating a
  tight retry loop.
- `DefaultBackoffConfig() BackoffConfig` — Returns the production defaults
  (200 ms base, 30 s cap) matching `serverconn.DefaultConnectorConfig`.

## Key Functions

- `RetryDelay(cfg BackoffConfig, attempt int) time.Duration` — Computes
  `min(base * 2^(attempt-1), max) * U[0.5, 1.0)` with jitter to spread
  concurrent retries across the same endpoint.
- `Sleep(ctx context.Context, cfg BackoffConfig, attempt *int)` — Increments
  the attempt counter and sleeps for the next backoff interval, respecting
  context cancellation. The caller owns `attempt` so multiple pull cycles can
  share a single growing backoff schedule.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull`, retrying
  on transport errors with exponential backoff until the call succeeds or ctx
  is cancelled. Status-level failures (`resp.Status.Ok == false`) are returned
  to the caller; only transport errors trigger retry.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`).
- **Depended on by**: `serverconn` (ingress pull loop), `sdk/swaps` (per-swap
  event consumers).

## Invariants

- `PullWithRetry` only retries transport errors, not status-level failures.
  Status errors are protocol-level problems that retry cannot fix; the caller
  must decide how to surface them.
- Context cancellation returns the context error, not the underlying transport
  error, so callers can distinguish "caller gave up" from "endpoint flapping".
- Jitter uses non-cryptographic randomness (`math/rand/v2`) — backoff timing
  is not security-sensitive.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
