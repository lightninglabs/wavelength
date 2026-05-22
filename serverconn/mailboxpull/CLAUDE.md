# mailboxpull

## Purpose

Provides shared retry-with-exponential-backoff primitives for mailbox pull
loops. Factors out the pull-and-retry pattern so the daemon's persistent
identity-mailbox ingress loop (`serverconn`) and per-swap event consumers
(`sdk/swaps`) share identical reliability semantics.

## Key Types

- `BackoffConfig` — base delay and cap for exponential backoff with jitter.
- `PullWithRetry` — calls `MailboxServiceClient.Pull`, retrying transport
  errors with backoff until success or context cancellation.
- `RetryDelay` — computes the next backoff duration given attempt count.
- `Sleep` — increments the attempt counter and blocks for the computed delay,
  respecting context cancellation.

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient interface for Pull calls)
- **Depended on by**: `serverconn` (identity-mailbox ingress loop),
  `sdk/swaps` (per-swap event consumer)
- **Sends**: none
- **Receives**: none

## Invariants

- Transport errors are retried; status-level failures (`resp.Status` non-nil
  with `Ok=false`) are returned to the caller as-is.
- Context cancellation always takes precedence over a pending transport error:
  `ctx.Err()` is checked both before issuing `Pull` and immediately after
  receiving a transport error, so callers get `context.Canceled` rather than
  the underlying transport error.
- `BackoffConfig{}` (zero value) is valid and selects package defaults
  (200 ms base, 30 s cap).

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
