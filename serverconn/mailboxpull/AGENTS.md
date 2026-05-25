# serverconn/mailboxpull

## Purpose

Shared retry-and-backoff primitives for mailbox pull loops. Factors out
exponential backoff with jitter so the `serverconn` ingress loop and per-swap
event consumers in `sdk/swaps` share identical retry semantics: same curve,
same jitter strategy, and the same context cancellation contract.

## Key Types

- `BackoffConfig` — Controls exponential backoff with jitter. Fields:
  `BaseDelay` and `MaxDelay` (both: non-positive = use package defaults
  200 ms base / 30 s cap). Zero value selects the production defaults.
- `DefaultBackoffConfig()` — Returns the production defaults used by the
  `serverconn` ingress loop: 200 ms base, 30 s cap.
- `RetryDelay(cfg BackoffConfig, attempt int) time.Duration` — Returns the
  next backoff duration: `min(base * 2^(attempt-1), max) * U[0.5, 1.0)`.
  Uses non-cryptographic randomness for jitter. Attempt counter is
  caller-owned.
- `Sleep(ctx context.Context, cfg BackoffConfig, attempt *int)` — Increments
  `*attempt` and sleeps for the next backoff interval, respecting context
  cancellation. Multiple pull cycles may share one attempt counter so
  backoff grows across successive failures.
- `PullWithRetry(ctx, edge mailboxpb.MailboxServiceClient, req *mailboxpb.PullRequest, cfg BackoffConfig, log btclog.Logger)` —
  Calls `edge.Pull`, retrying transport errors with exponential backoff
  until success or context cancellation. Preserves cursor state across
  reconnects (req is unchanged across retries). Returns the context error on
  cancellation, distinguishing "caller gave up" from "endpoint flapping".
  Status-level failures (`resp.Status` with `Ok=false`) are returned to
  the caller rather than retried.

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient, PullRequest, PullResponse).
- **Depended on by**: `serverconn` (persistent identity-mailbox ingress loop),
  `sdk/swaps` (per-swap out-swap event consumers).

## Invariants

- Cursor state and attempt counter are caller-owned; this package is
  stateless. The ingress loop preserves its cursor position and grows
  backoff across successive failures using the same counter; a successful
  pull resets the counter.
- `PullWithRetry` retries only on transport errors (connection refused,
  stream reset). Application-level rejections (`resp.Status.Ok == false`)
  are surfaced to the caller immediately.
- The attempt counter is incremented by `Sleep` before the sleep, so
  attempt=0 on the first call yields `BaseDelay * U[0.5, 1.0)`.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package and ingress loop
  context.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
