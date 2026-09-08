# Structured Logging Guide

## Required Format

All logging **must** use structured log methods (ending in `S`) with static
messages.

**Method signature:**
1. First parameter: `context.Context`
2. Second parameter: static string (no `fmt.Sprintf`)
3. For `WarnS`/`ErrorS`/`CriticalS`: third parameter is the `error` being
   logged (may be `nil`); `InfoS`/`DebugS`/`TraceS` have no `error` param.
4. Remaining parameters: key-value pairs

**Key-value helpers:** `slog.Int()`, `slog.String()`, `btclog.Fmt()`,
`btclog.Hex()`, etc.

## Example

```go
log.InfoS(ctx, "Channel open performed",
	slog.Int("user_id", userID),
	btclog.Fmt("amount", "%.8f", 0.00154))
```

**Formatting rules:**
- One key-value pair per line for readability.
- Lines can exceed 80 chars for structured logging.
- Closing `)` stays on the same line as the last attribute.

## Production Severity Contract

Production `Warn`, `Error`, and `Critical` logs alert a human. Choose the
level from the action the operator must take, not from whether a Go call
returned an error.

| Scenario | Level |
|----------|-------|
| Immediate funds, security, or process-integrity threat | `critical` |
| Internal invariant, durable-state failure, or exhausted recovery that requires intervention | `error` |
| Actionable degradation or first occurrence of a dependency failure | `warn` |
| Expected lifecycle, policy rejection, safe fallback, or recovery | `info` |
| Repeated retry, high-volume protocol detail, or diagnostic state | `debug` |

Client validation failures, capacity admission, idempotent duplicates, stale
requests, peer behavior, and a fallback that repairs the condition in the same
call are not alerts. Keep them at `Info` or `Debug`.

For recurring work:

1. Warn on the first actionable failure.
2. Warn again if the failure cause changes materially.
3. Log identical retries at debug.
4. Log recovery at info.
5. Escalate to error only at an explicit retry, time, or safety threshold.

Every new `WarnS`, `ErrorS`, or `CriticalS` must have a bounded trigger and
an operator action. Explain both in the commit message or PR description.
Static messages define alert identity; put variable values in attributes so
one incident cannot create unbounded alert groups.

Do not add unit tests that only pin a log level or message. Explain pure
severity changes in the commit and PR. Add tests when the change introduces
classification, retry, threshold, or recovery logic.
