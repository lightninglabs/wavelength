# serverconn/mailboxpull

## Purpose

Shared retry-and-backoff primitives for mailbox pull loops. Provides uniform
reliability semantics across the persistent identity-mailbox ingress in
`serverconn` and the per-swap event consumers in the SDK, so every pull
loop handles transient transport errors consistently without duplicating
backoff logic.

## Key Types

- `BackoffConfig` — Exponential backoff parameters: `BaseDelay` (default
  200 ms) and `MaxDelay` (default 30 s).
- `DefaultBackoffConfig()` — Returns production defaults.
- `RetryDelay(cfg, attempt)` — Returns `min(base × 2^(attempt−1), max) ×
  U[0.5, 1.0)` with jitter. Attempt 1 is the first retry.
- `Sleep(ctx, cfg, attempt)` — Increments the attempt counter, sleeps the
  computed backoff, and returns early on context cancellation.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls
  `mailboxpb.MailboxServiceClient.Pull` on the provided edge client and
  retries on transport errors with exponential backoff until success or
  context done. Distinguishes caller cancellation from endpoint flapping.
  `nil` logger defaults to `btclog.Disabled`.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`), `btclog` (Logger).
- **Depended on by**: `serverconn` (identity-mailbox ingress pull loop),
  `sdk/swaps` (per-swap event consumer pull loop).
- **Sends/Receives**: none — utility library with no actor message flows.

## Invariants

- Status-level failures (`resp.Status` non-nil with `Ok=false`) are returned
  to the caller immediately and are NOT retried — they indicate a deterministic
  protocol error, not a transient transport issue.
- Transport errors trigger exponential backoff retry up to the caller's context
  deadline.
- Context cancellation is reported as the preferred error over the underlying
  transport error when both occur simultaneously.
- Non-cryptographic randomness (`math/rand/v2`) is safe for backoff jitter.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Primary consumer of this package.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Three-layer
  mailbox system overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
