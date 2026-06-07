# serverconn/mailboxpull

## Purpose

Shared exponential backoff and retry primitives for mailbox pull loops.
Factors out consistent retry semantics — distinguishing transport errors
(endpoint flapping, retried) from caller cancellation (context error,
returned) and from RPC-level failures (non-OK status, returned to caller) —
so both the persistent identity-mailbox ingress loop in `serverconn` and
per-swap event consumers in the SDK use identical backoff behavior.

## Key Types

- `BackoffConfig` — tuning for exponential backoff with `BaseDelay` (default
  200 ms) and `MaxDelay` (default 30 s).
- `DefaultBackoffConfig()` — returns production defaults.
- `RetryDelay(cfg BackoffConfig, attempt int) time.Duration` — computes
  `min(base × 2^(attempt−1), max) × U[0.5, 1.0)` (exponential with jitter).
- `Sleep(ctx context.Context, cfg BackoffConfig, attempt *int)` — increments
  the attempt counter, sleeps for the next backoff duration, and returns early
  on context cancellation.
- `PullWithRetry(ctx, edge, req, cfg, log)` — retries `edge.Pull` on transport
  errors using `BackoffConfig`; returns on success or context cancellation;
  non-OK status responses are returned to the caller without retry.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient` gRPC stub).
- **Depended on by**: `serverconn` (ingress pull loop), `sdk/swaps`
  (per-swap event consumer).

## Invariants

- Transport errors (gRPC connection-level failures) are retried indefinitely
  with backoff; the loop stops only on context cancellation or status-level
  failure.
- `PullWithRetry` distinguishes "caller gave up" (context error returned) from
  "endpoint flapping" (transport error, logged and retried).
- Non-OK gRPC status responses (e.g. PERMISSION_DENIED) are surfaced directly
  to the caller — they are not transport errors and must not be silently
  retried.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Ingress/egress connector overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
