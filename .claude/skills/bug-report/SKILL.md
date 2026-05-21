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
LOG=~/.darepod/logs/signet/darepod.log

if [ -f "$LOG" ]; then
  # Line where the most recent daemon session started. Fall back to line
  # 1 if no marker is found — a partial log is better than none.
  START=$(grep -n "Starting darepod" "$LOG" | tail -1 | cut -d: -f1)
  SESSION=$(tail -n +${START:-1} "$LOG")

  BUILD=$(echo "$SESSION" | grep "Starting darepod" | tail -1)
  ERRORS=$(echo "$SESSION" | grep -E '\[WRN\]|\[ERR\]' | tail -40)
  FIRST_TS=$(echo "$SESSION" | head -1 | cut -c1-19)
  LAST_TS=$(echo "$SESSION"  | tail -1 | cut -c1-19)
else
  SESSION="" BUILD="" ERRORS="" FIRST_TS="" LAST_TS=""
fi
```

If the log file does not exist, the variables are empty — the report will
note "no log file found" in the affected sections instead of aborting.

### 3. Collect environment

```bash
OS=$(uname -srvm)
GO_VER=$(go version 2>/dev/null)
# Portable IPv6 check: any non-link-local, non-loopback inet6 address.
# ifconfig works on macOS and most Linux distros.
IPV6=$(ifconfig 2>/dev/null | awk '/inet6/ && !/fe80|::1/ {print $2; exit}')
IPV6=${IPV6:-none}
ESPLORA_URL=$(grep 'esploraurl' ~/.darepod/darepod.conf 2>/dev/null \
  | cut -d= -f2)
ESPLORA_HTTP=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 \
  "${ESPLORA_URL}/blocks/tip/height" 2>/dev/null || echo "unreachable")
DAEMON_PID=$(pgrep -x darepod || echo "not running")
```

### 4. Read and redact config

Read `~/.darepod/darepod.conf` and redact secrets before including in the
report. Replace the value of any line whose key matches one of these
patterns with `<REDACTED>`:

- `bitcoind.user`, `bitcoind.pass`, `bitcoind.rpcuser`, `bitcoind.rpcpass`,
  `bitcoind.rpcpassword`
- any key ending in `password`, `pass`, `secret`, `token`, or `apikey`

```bash
CONFIG=$(sed -E '
  s/^([[:space:]]*bitcoind\.(user|pass|rpcuser|rpcpass(word)?))[[:space:]]*=.*/\1=<REDACTED>/;
  s/^([[:space:]]*[^=]*(password|secret|token|apikey))[[:space:]]*=.*/\1=<REDACTED>/I
' ~/.darepod/darepod.conf 2>/dev/null)
```

Use `$CONFIG` (not the raw file) in the report body.

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

## Config (~/.darepod/darepod.conf, secrets redacted)

\`\`\`
{CONFIG}
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
