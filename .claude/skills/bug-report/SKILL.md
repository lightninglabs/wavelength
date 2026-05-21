---
name: bug-report
description: >
  File a structured GitHub issue with daemon logs, config, and environment
  state. Use when a test session hits an error or unexpected behavior —
  collects the current session's WRN/ERR log lines, build commit, config,
  and Esplora reachability automatically.
argument-hint: "[one-line description of what went wrong]"
allowed-tools: Bash, Read
---

# Bug Report

Collect runtime state from the current darepod test session and file a
structured GitHub issue against this repository.

## Steps

### 1. Get the description

If the user provided an argument, use it as the issue title. If not, ask for
one sentence describing what went wrong before collecting anything.

### 2. Collect session logs

```bash
# Line number where the most recent daemon session started
START=$(grep -n "Starting darepod" ~/.darepod/logs/signet/darepod.log \
  | tail -1 | cut -d: -f1)

# Full log for the current session
SESSION=$(tail -n +${START} ~/.darepod/logs/signet/darepod.log)

# Build line (commit hash, network, wallet type)
BUILD=$(echo "$SESSION" | grep "Starting darepod" | tail -1)

# Warnings and errors from this session, capped at 40 lines
ERRORS=$(echo "$SESSION" | grep -E '\[WRN\]|\[ERR\]' | tail -40)

# Session window
FIRST_TS=$(echo "$SESSION" | head -1 | cut -c1-19)
LAST_TS=$(echo "$SESSION"  | tail -1 | cut -c1-19)
```

If `~/.darepod/logs/signet/darepod.log` does not exist, note that no log
file was found and continue — do not abort.

### 3. Collect environment

```bash
OS=$(sw_vers 2>/dev/null | tr '\n' ' ')
GO_VER=$(go version 2>/dev/null)
IPV6=$(networksetup -getinfo Wi-Fi 2>/dev/null | grep -E "IPv6")
ESPLORA_URL=$(grep 'esploraurl' ~/.darepod/darepod.conf 2>/dev/null \
  | cut -d= -f2)
ESPLORA_HTTP=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 \
  "${ESPLORA_URL}/blocks/tip/height" 2>/dev/null || echo "unreachable")
DAEMON_PID=$(pgrep -x darepod || echo "not running")
```

### 4. Read config

Read `~/.darepod/darepod.conf` verbatim. It contains no secrets — no
redaction needed.

### 5. Collect live wallet state

Only if `pgrep -x darepod` returns a PID:

```bash
~/go/bin/darepocli --rpcserver 127.0.0.1:10029 --no-tls balance 2>&1
~/go/bin/darepocli --rpcserver 127.0.0.1:10029 --no-tls list 2>&1 | head -30
```

If the daemon is not running, record "daemon not running at time of report".

### 6. Detect affected subsystems

Scan the session's `[WRN]`/`[ERR\]` lines for these log prefixes and collect
every one that appears:

| Prefix | Label      |
|--------|------------|
| LWWL   | lwwallet   |
| ARKW   | wallet     |
| WRPC   | walletrpc  |
| ROND   | round      |
| OORC   | oor        |
| SVRC   | serverconn |
| DRPD   | daemon     |
| SWAP   | swap       |

Always include the `bug` label. Add subsystem labels on top.

### 7. Build the issue body

```
## Summary

{user's one-line description}

## Environment

- **OS**: {OS}
- **Go**: {GO_VER}
- **Daemon**: {commit= and wallet_type= fields from BUILD line}
- **IPv6**: {IPV6}
- **Esplora**: {ESPLORA_URL} → HTTP {ESPLORA_HTTP}
- **Daemon PID**: {DAEMON_PID}
- **Session window**: {FIRST_TS} → {LAST_TS}

## Config (~/.darepod/darepod.conf)

\`\`\`
{darepod.conf contents}
\`\`\`

## Warnings and Errors (this session)

\`\`\`
{ERRORS — or "none" if the session has no WRN/ERR lines}
\`\`\`

## Wallet State

\`\`\`
{balance and list output, or "daemon not running at time of report"}
\`\`\`

## Reproduction Steps

<!-- What were you doing when this happened? -->
1.
2.

## Full Session Log

<details>
<summary>Expand</summary>

\`\`\`
{SESSION — truncated to 300 lines if longer, with a note of the total
line count}
\`\`\`

</details>
```

### 8. Preview and confirm

Print the full title and body to the terminal. Then ask:
**"File this issue now? [y/N]"**

If **yes**, run:

```bash
gh issue create \
  --title "{description}" \
  --body  "{body}" \
  --label "bug" \
  --label "{each detected subsystem label}"
```

Print the resulting issue URL.

If **no**, print the body so the user can copy-paste it manually and note:
`Open a new issue at: https://github.com/lightninglabs/darepo-client/issues/new`

### 9. If `gh` is not available or not authenticated

Skip the filing step. Print the body and the manual URL above. Do not treat
this as an error — a copy-pasteable report is still a good outcome.
