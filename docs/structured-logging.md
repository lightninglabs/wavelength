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

## Error Log Levels

**CRITICAL**: Only use `error` level for **internal errors never expected during
normal operation**.

| Scenario | Level |
|----------|-------|
| Internal bug, impossible state | `error` |
| RPC failure to external service | `warn` |
| Chain backend disconnection | `warn` |
| Peer disconnect | `info` or `debug` |
| User-triggered condition | `info` or `debug` |

**Rule of thumb:** if a user or external system could cause it, it is not an
error-level log.
