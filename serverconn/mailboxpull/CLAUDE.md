# serverconn/mailboxpull

## Purpose

Shared retry and backoff primitives for mailbox pull loops. Factors out the
exponential-backoff-with-jitter retry shape common to the daemon's persistent
identity-mailbox ingress loop and per-swap event consumers in the SDK, keeping
reliability semantics uniform across both consumers.

## Key Types

- `BackoffConfig` — `BaseDelay` and `MaxDelay` for exponential backoff. Zero
  value uses package defaults (200 ms base, 30 s cap).
- `DefaultBackoffConfig()` — Returns the production defaults used by the
  `serverconn` ingress loop.
- `RetryDelay(cfg, attempt)` — Computes one exponential backoff duration with
  jitter (`min(base * 2^(attempt-1), max) * U[0.5, 1.0)`). Non-cryptographic
  randomness is intentional — timing is not security-sensitive.
- `Sleep(ctx, cfg, *attempt)` — Increments the attempt counter and sleeps for
  the next backoff interval, respecting context cancellation.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull` and retries on
  transport errors with exponential backoff until success or `ctx` cancellation.
  Status-level failures are returned to the caller; only transport errors are
  retried.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`).
- **Depended on by**: `serverconn` (identity-mailbox ingress pull loop),
  `sdk/swaps` (per-swap event consumer pull loop).

## Invariants

- `PullWithRetry` retries only transport errors. A `resp.Status` with
  `Ok=false` is returned to the caller — it indicates a deterministic
  protocol-level problem, not a transient network blip.
- On context cancellation the context error is returned, not the underlying
  transport error, so callers can distinguish "caller gave up" from "endpoint
  is flapping".
- `Sleep` increments `*attempt` before computing the delay, so the first
  retry uses `attempt=1` and grows with each successive failure.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package: durable egress,
  ingress polling, unary RPC facade.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
