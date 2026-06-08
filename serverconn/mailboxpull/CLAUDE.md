# serverconn/mailboxpull

## Purpose

Shared exponential-backoff retry primitives for mailbox pull loops. Both
the persistent identity-mailbox ingress loop in `serverconn` and per-swap
event consumers in the SDK need the same retry shape; this package factors
that shape out so reliability semantics stay uniform across both consumers.

## Key Types

- `BackoffConfig` — Controls exponential backoff with jitter. `BaseDelay`
  (non-positive → 200 ms default) and `MaxDelay` (non-positive → 30 s default).
  The zero value selects package defaults so callers that do not care about
  tuning can pass `BackoffConfig{}`.
- `DefaultBackoffConfig()` — Returns the production defaults (200 ms base,
  30 s cap) matching `serverconn.DefaultConnectorConfig` so daemon and SDK
  pull loops back off identically when the shared mailbox endpoint is flapping.
- `RetryDelay(cfg, attempt) time.Duration` — Exponential backoff with jitter:
  `min(base × 2^(attempt-1), max) × U[0.5, 1.0)`. Non-cryptographic randomness
  (security-insensitive timing).
- `Sleep(ctx, cfg, *attempt)` — Increments the caller-owned attempt counter and
  sleeps for the next backoff interval, respecting context cancellation. Sharing
  a single counter across pull cycles grows the delay on successive failures
  and resets it implicitly when the caller resets the counter on success.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull`, retrying
  transport errors with exponential backoff until success or ctx cancellation.
  Returns ctx error (not the transport error) on cancellation so callers
  distinguish "caller gave up" from "endpoint is flapping". Status-level
  failures (`resp.Status.Ok == false`) are returned as-is; only transport
  errors trigger retry.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`).
- **Depended on by**: `serverconn` (identity-mailbox ingress loop), `sdk/swaps`
  (per-swap event consumer pull loops).

## Invariants

- Backoff parameters default to the same values as
  `serverconn.DefaultConnectorConfig` so daemon and SDK pull loops back off
  identically under flapping mailbox endpoints.
- Context cancellation surfaces as the context error, not the underlying
  transport error — callers test `errors.Is(err, context.Canceled)` rather
  than inspecting gRPC status codes.
- The attempt counter is caller-owned so a single counter can span multiple
  consecutive pull cycles (successive failures grow the delay; a successful
  cycle allows the caller to reset it).

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package: identity-mailbox
  ingress loop that uses this package.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
