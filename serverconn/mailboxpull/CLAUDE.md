# serverconn/mailboxpull

## Purpose

Shared retry+backoff primitives for mailbox pull loops. Factors out the
reliability semantics common to the persistent identity-mailbox ingress loop in
`serverconn` and per-swap event consumers in the SDK so both use identical
backoff behavior when the shared mailbox endpoint is flapping.

## Key Types

- `BackoffConfig` — exponential backoff parameters (BaseDelay, MaxDelay). Zero
  value selects package defaults (200 ms base, 30 s cap).
- `DefaultBackoffConfig` — returns the production defaults used by the
  serverconn ingress loop.
- `RetryDelay` — computes one exponential backoff duration with ±50% jitter for
  a given attempt count.
- `Sleep` — increments an attempt counter and sleeps for the next backoff
  interval, respecting context cancellation.
- `PullWithRetry` — calls `edge.Pull`, retrying transport errors with
  exponential backoff until success or ctx cancellation. Preserves the caller's
  cursor state across reconnects. Returns status-level failures to the caller
  rather than retrying them (status errors indicate deterministic protocol
  problems, not transient network blips).

## Relationships

- **Depends on**: `mailbox/pb` (MailboxServiceClient and PullRequest/Response).
- **Depended on by**:
  - `serverconn` (identity-mailbox ingress loop)
  - `sdk/swaps` (per-swap vHTLC poll loop)
- **Sends**: nothing — utility package.
- **Receives**: nothing — called synchronously by callers.

## Invariants

- `PullWithRetry` only retries transport errors. Status-level failures
  (`resp.Status.Ok == false`) are returned immediately so callers can surface
  deterministic protocol errors rather than spinning indefinitely.
- Context cancellation is returned as-is (not wrapped), so callers can
  distinguish "caller gave up" from "endpoint is flapping."
- The attempt counter is owned by the caller so successive failures grow the
  delay; the caller resets it on success.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
