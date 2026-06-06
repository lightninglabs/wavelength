# serverconn/mailboxpull

## Purpose

Shared retry-and-backoff primitives for mailbox pull loops. Factors out the
same retry shape needed by the persistent identity-mailbox ingress loop in
the parent `serverconn` package and by per-swap event consumers in the SDK,
so reliability semantics stay uniform across both consumers.

## Key Types

- `BackoffConfig` — Exponential backoff parameters: `BaseDelay` (default
  200 ms) and `MaxDelay` (default 30 s). Zero value selects package
  defaults so callers that do not care about tuning can pass
  `BackoffConfig{}` without producing a tight retry loop.
- `DefaultBackoffConfig()` — Returns the production defaults (200 ms base,
  30 s cap) mirroring `serverconn.DefaultConnectorConfig` so daemon ingress
  and SDK pull loops back off identically against the same endpoint.
- `RetryDelay(cfg, attempt) time.Duration` — Exponential backoff with
  jitter: `min(base * 2^(attempt-1), max) * U[0.5, 1.0)`. Jitter uses
  non-cryptographic randomness; backoff timing is not security-sensitive.
- `Sleep(ctx, cfg, *attempt)` — Increments `*attempt` and sleeps for the
  next backoff interval, respecting context cancellation. The attempt
  counter is caller-owned so multiple pull cycles share one schedule.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `edge.Pull` with
  exponential-backoff retry on transport errors. On context cancellation
  returns the **context error** (not the underlying transport error) so
  callers can distinguish "caller gave up" from "endpoint is flapping".
  Status-level failures (`resp.Status.Ok == false`) are returned to the
  caller without retry — those indicate deterministic protocol-level
  problems, not transient network blips.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`), `btclog/v2` (caller-supplied logger; nil → disabled).
- **Depended on by**: `serverconn` (daemon ingress pull loop), `sdk/swaps`
  (per-swap event consumers).

## Invariants

- `req` is never mutated by `PullWithRetry`; cursor state is preserved
  across reconnects by the caller owning the request struct.
- A nil logger is silently coerced to `btclog.Disabled`; callers never
  need to guard against nil.
- `Sleep` is the only retry-delay site — callers must not implement their
  own sleep loop around `PullWithRetry`.

## Deep Docs

- [serverconn/CLAUDE.md](../CLAUDE.md) — Parent package: unified mailbox
  connector, ingress polling, and egress actor.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
