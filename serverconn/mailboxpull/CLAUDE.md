# serverconn/mailboxpull

## Purpose

Shared retry and exponential-backoff primitives for mailbox pull loops.
Factors out the common retry shape used by both the daemon's persistent
identity-mailbox ingress loop (`serverconn`) and per-swap event consumers
in the SDK, so reliability semantics stay uniform across both consumers.

## Key Types

- `BackoffConfig` — exponential backoff parameters (BaseDelay, MaxDelay).
  Zero value selects defaults (200 ms base, 30 s cap).
- `DefaultBackoffConfig` — returns the production defaults used by the
  serverconn ingress loop.
- `RetryDelay` — computes `min(base*2^(attempt-1), max) * U[0.5, 1.0)`
  for one retry attempt.
- `Sleep` — increments the attempt counter and sleeps for the next backoff
  interval, respecting context cancellation.
- `PullWithRetry` — calls `mailboxpb.MailboxServiceClient.Pull` with
  transparent retry on transport errors, exponential backoff, and
  context-aware cancellation. Returns on first success or context done.

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient, PullRequest/Response).
- **Depended on by**: `serverconn` (ingress pull loop),
  `sdk/swaps` (out-swap HTLC mailbox receiver via
  `MailboxOutSwapEventReceiver`).

## Invariants

- Status-level failures (`resp.Status` non-nil with `Ok=false`) are
  returned immediately to the caller; only transport errors trigger retry.
- On context cancellation the context error is returned, not the underlying
  transport error, so callers can distinguish "caller gave up" from
  "endpoint is flapping".
- Backoff uses non-cryptographic randomness (`math/rand/v2`) because timing
  is not security-sensitive.
- The default backoff parameters (200 ms / 30 s) mirror
  `serverconn.DefaultConnectorConfig` intentionally so both consumers
  back off identically when the shared mailbox endpoint is flapping.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package using this package
  for its ingress loop.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
