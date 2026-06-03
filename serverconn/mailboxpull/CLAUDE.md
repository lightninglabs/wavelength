# mailboxpull

## Purpose

Shared retry-and-backoff primitives for mailbox pull loops. Factors out
the common shape needed by the persistent identity-mailbox ingress loop in
`serverconn` and by per-swap event consumers in `sdk/swaps`, so reliability
semantics (delay schedule, jitter, context-cancel precedence) stay uniform
across both consumers.

## Key Types

- `BackoffConfig` — Exponential backoff parameters (`BaseDelay`, `MaxDelay`);
  zero value selects safe defaults (200 ms base, 30 s cap).
- `RetryDelay(cfg, attempt)` — Returns an exponential backoff duration with
  jitter in [0.5, 1.0), capped at `MaxDelay`.
- `Sleep(ctx, cfg, attempt*)` — Increments the attempt counter and sleeps for
  the next backoff interval, aborting on context cancellation.
- `PullWithRetry(ctx, edge, req, cfg, log)` — Calls `MailboxServiceClient.Pull`
  in a loop, retrying transport errors with exponential backoff until a
  successful response or context done.
- `DefaultBackoffConfig()` — Returns production defaults (200 ms / 30 s),
  mirroring `serverconn.DefaultConnectorConfig` so both pull paths back off
  identically when the shared mailbox endpoint is flapping.

## Relationships

- **Depends on**: `mailbox/pb` (`MailboxServiceClient`, `PullRequest`,
  `PullResponse`), `btclog` (structured warning on retry).
- **Depended on by**: `serverconn` (ingress loop in `ingress.go`),
  `sdk/swaps` (per-swap event consumer in `out_swap_mailbox.go`).
- **Sends**: nothing — pure utility, no actor messages.
- **Receives**: nothing — callers pass in a ready `MailboxServiceClient`.

## Invariants

- Only transport errors trigger retries; status-level failures
  (`resp.Status` non-nil with `Ok=false`) are returned to the caller
  unchanged — the caller decides whether to surface or retry.
- On context cancellation, `PullWithRetry` returns `ctx.Err()`, not the
  underlying transport error, even when the cancel arrives between a
  failed Pull and the next sleep. This asymmetry is intentional: callers
  further up the stack treat `context.Canceled` as a clean shutdown, not
  a swap failure.
- The attempt counter is owned and reset by the caller; a success should
  reset it to 0 so the next failure starts from the base delay rather
  than an already-accumulated exponent.

## Deep Docs

- [serverconn/README.md](../README.md) — Parent package architecture, ingress
  loop design.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) —
  Three-layer mailbox system.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
