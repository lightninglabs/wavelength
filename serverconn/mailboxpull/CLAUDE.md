# serverconn/mailboxpull

## Purpose

Shared retry-and-backoff primitives for mailbox pull loops. Factors out the
uniform exponential-backoff-with-jitter pattern used by both the persistent
identity-mailbox ingress loop in `serverconn` and per-swap event consumers in
`sdk/swaps`, so reliability semantics stay consistent across callers.

## Key Types

- `BackoffConfig` — Exponential backoff parameters: `BaseDelay` (default 200 ms)
  and `MaxDelay` (default 30 s). Zero-value selects the package defaults, so
  `BackoffConfig{}` is valid and produces a non-busy retry loop.
- `DefaultBackoffConfig()` — Returns the production defaults matching
  `serverconn.DefaultConnectorConfig` so the daemon ingress loop and SDK pull
  loops back off identically.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull` in a loop,
  retrying transport errors with exponential backoff until success or ctx
  cancellation. Preserves the caller's cursor across retries; returns
  `ctx.Err()` (not the transport error) on cancellation so callers can
  distinguish shutdown from a flapping endpoint. Status-level failures
  (`resp.Status` non-nil with `Ok=false`) are passed through without retry.
- `RetryDelay(cfg, attempt)` — Computes one backoff duration: `min(base *
  2^(attempt-1), max) * U[0.5, 1.0)`. Non-cryptographic jitter spreads
  concurrent retriers to prevent thundering-herd on a shared endpoint.
- `Sleep(ctx, cfg, attempt)` — Increments `*attempt` and sleeps for the next
  backoff interval, respecting ctx cancellation.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`).
- **Depended on by**: `serverconn` (identity-mailbox ingress loop),
  `sdk/swaps` (per-swap out-swap HTLC event pull loop via
  `MailboxOutSwapEventReceiver`).

## Invariants

- `PullWithRetry` never mutates `req`; cursor state is owned by the caller
  and survives across reconnects.
- Context cancellation always takes precedence over transport errors: the
  post-Pull `ctx.Err()` check runs before the backoff sleep so a
  cancellation mid-retry surfaces as `ctx.Err()`, not the transport error.
- `BackoffConfig{}` (zero value) is valid and uses the package defaults
  (200 ms base, 30 s cap) — callers that do not care about tuning need not
  initialize the struct.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package; wires the ingress
  loop that consumes this helper.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
