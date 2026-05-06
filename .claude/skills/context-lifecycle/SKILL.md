---
name: context-lifecycle
description: >
  Review goroutines, timers, callbacks, actor handoffs, and async cleanup for
  context lifetime bugs. Use when code starts work that may outlive the caller,
  especially from RPC handlers, wallet startup, daemon startup, actors, or
  notification registrations.
argument-hint: "[package-path or file]"
allowed-tools: Read, Grep, Glob, Bash(rg *)
---

# Context Lifecycle Review

Use this skill to catch async work that accidentally captures a short-lived
caller context.

The failure mode is subtle: `ctx` is usually the nearest variable, so a
goroutine, timer, callback, or actor handoff may inherit an RPC/test/request
context even though the work must continue after that caller returns. When the
caller exits, the context is cancelled and the background work can silently stop
too early.

## Core Question

For every asynchronous handoff, ask:

> Who owns this work, and when should it stop?

The answer determines which context is correct.

## Search

Start with targeted searches instead of reading whole packages:

```bash
rg -n 'go func|AfterFunc|time\.AfterFunc|context\.AfterFunc' \
  --glob '*.go' [path]

rg -n 'Done\(|WithCancel\(|WithTimeout\(|WithDeadline\(' \
  --glob '*.go' [path]

rg -n '\.Tell\(|\.Ask\(|Register|Subscribe' \
  --glob '*.go' [path]
```

Then inspect each hit in context.

## Classify The Owner

Classify each async handoff before suggesting a fix.

### Caller-Owned Work

The caller waits for the goroutine before returning, or the goroutine only
performs request-scoped work.

Using the caller context is usually correct.

Examples:

- a goroutine joined by `WaitGroup.Wait` before return
- a worker that sends one result and the function waits on that result
- a helper that should abort when the RPC deadline expires

### Component-Owned Work

The work is started by a caller but owned by a daemon, wallet, actor, or
service after startup.

Do not capture the RPC/request context. Use a component root context, a
registration-owned context, or an explicit context with its own cancel path.

Examples:

- wallet sync loops started by `UnlockWallet`
- daemon background workers started from an RPC
- actor goroutines started during actor construction
- server connection runtimes started by daemon setup

### Registration-Owned Work

The work forwards notifications until an explicit returned `Cancel`, `Stop`, or
subscription teardown is invoked.

Use the registration lifecycle, not the caller context that created the
registration. Make the ownership visible in names and comments.

Examples:

- block epoch notifications
- spend notifications
- confirmation notifications
- mailbox subscriptions

### Timer-Owned Work

The callback runs after the scheduling actor turn has completed.

Do not use the actor receive context if cancellation of that receive turn would
incorrectly cancel the future callback. Use a component-owned context, a fresh
bounded context inside the callback, or document the intentional detached root.

Examples:

- retry timers
- recurring ticks
- delayed cleanup

### Bounded Cleanup Work

The work intentionally outlives the caller to clean up reservations, release
state, or reconcile durable actor results.

Use an explicit timeout. Prefer a long but finite ceiling over an unbounded
`context.Background()` wait.

Examples:

- best-effort VTXO unlock after detached durable work
- async release of custom input reservations
- deferred cleanup after a caller deadline

## Red Flags

Treat these as likely bugs until proven otherwise:

- `go func()` in an RPC handler captures the handler's `ctx`
- a goroutine selects on `ctx.Done()` after the parent function returns
- `context.WithCancel(ctx)` or `context.WithTimeout(ctx, ...)` is created for
  a daemon worker that should outlive the request
- a timer callback closes over `ctx` from the scheduling call
- a callback or subscription uses a test context but is stopped by `t.Cleanup`
- a `//nolint:contextcheck` comment explains the linter, but not the owner

## Good Fixes

Prefer explicit ownership over generic `context.Background()`:

- use `serverCtx`, `daemonCtx`, `actorCtx`, `runCtx`, or `notifyCtx`
- pass a component root context into constructors that start long-lived work
- derive registration contexts from a cancel returned to the caller
- use Go 1.21's `context.WithoutCancel(ctx)` when work must survive caller
  cancellation but should preserve values such as trace IDs or scoped loggers
- use `context.WithTimeout(context.Background(), limit)` for bounded cleanup
- derive test helper lifecycles from `context.WithCancel(context.Background())`
  and call the cancel function from `t.Cleanup` when cleanup owns the work
- keep request contexts on work that really should stop with the request
- add a short ownership comment at intentional lifecycle boundaries

## Bad Pattern

```go
func (s *Server) UnlockWallet(ctx context.Context,
	req *UnlockWalletRequest) (*UnlockWalletResponse, error) {

	go func() {
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.syncWallet(ctx)
			}
		}
	}()

	return &UnlockWalletResponse{}, nil
}
```

The goroutine is meant to survive after `UnlockWallet` returns, but it captures
the RPC context.

## Better Pattern

```go
func (s *Server) UnlockWallet(ctx context.Context,
	req *UnlockWalletRequest) (*UnlockWalletResponse, error) {

	s.walletSyncOnce.Do(func() {
		s.startWalletSync(s.daemonCtx)
	})

	return &UnlockWalletResponse{}, nil
}

func (s *Server) startWalletSync(runCtx context.Context) {
	go func() {
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				s.syncWallet(runCtx)
			}
		}
	}()
}
```

The worker now uses the daemon-owned lifecycle.

## Review Checklist

1. Find goroutines, timers, callbacks, registrations, and actor handoffs.
2. For each one, decide whether it is caller-owned, component-owned,
   registration-owned, timer-owned, or bounded cleanup.
3. If it outlives the caller, verify it does not capture the caller context.
4. If it uses `context.Background()`, ask whether a named owner context would be
   clearer or whether the work needs a finite timeout.
5. If it has `//nolint:contextcheck`, require the comment to state the owner.
6. Check tests that use `t.Context()` with `t.Cleanup`; cleanup-owned work often
   needs a test-owned actor or explicit cancel path, not the request context.
7. Suggest a focused test when cancellation was the bug: cancel the caller,
   then assert the background worker keeps running or shuts down as intended.

## Acceptable Review Outcomes

- **Bug**: the async work captures a caller context but must outlive the caller.
- **Correct**: the caller waits for the async work before returning.
- **Correct with comment**: the work intentionally has a detached owner and the
  code names or comments that owner.
- **Needs bound**: detached cleanup is correct, but an unbounded wait could leak
  goroutines or reservations.
