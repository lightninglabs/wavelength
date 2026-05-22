---
name: bug-report
description: >
  File a structured GitHub issue with daemon logs, config, and environment
  state. Use when a test session hits an error or unexpected behavior —
  auto-detects the active network (mainnet/testnet/signet/regtest/simnet)
  and chain backend (lwwallet+Esplora / btcwallet+neutrino / lnd), then
  collects the current session's WRN/ERR log lines, build commit, config,
  and backend reachability.
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

### 2. Detect the active network and chain backend

Do **not** assume a network (mainnet/testnet/signet/regtest/simnet) or a
chain backend (lwwallet+Esplora / btcwallet+neutrino / lnd). Detect both
from config first, then cross-check against the most recent daemon log.

```bash
CONF=~/.darepod/darepod.conf

# Helper: read a key from the conf file (uncommented), strip whitespace and
# surrounding quotes. Returns empty if missing.
conf_get() {
  grep -E "^[[:space:]]*$1[[:space:]]*=" "$CONF" 2>/dev/null \
    | tail -1 | cut -d= -f2- \
    | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/^"(.*)"$/\1/'
}

# Config-derived values. Defaults match darepod/config.go:
#   network defaults to mainnet, wallet.type defaults to lwwallet.
NETWORK=$(conf_get network)
NETWORK=${NETWORK:-mainnet}
WALLET_TYPE=$(conf_get wallet.type)
WALLET_TYPE=${WALLET_TYPE:-lwwallet}

# Cross-check against the actual log directory tree. If the config-derived
# network has no log file but another network's log does, prefer the
# directory that has a recent log — the operator may have switched
# networks without updating the conf, or the conf may be elsewhere.
LOG_ROOT=~/.darepod/logs
LOG="$LOG_ROOT/$NETWORK/darepod.log"
if [ ! -f "$LOG" ]; then
  ALT=$(ls -t "$LOG_ROOT"/*/darepod.log 2>/dev/null | head -1)
  if [ -n "$ALT" ]; then
    LOG="$ALT"
    # Re-derive network from the directory name so the report is
    # consistent with the log we actually read.
    NETWORK=$(basename "$(dirname "$ALT")")
  fi
fi
```

### 3. Collect session logs

```bash
if [ -f "$LOG" ]; then
  # Line where the most recent daemon session started. Fall back to line
  # 1 if no marker is found — a partial log is better than none.
  START=$(grep -n "Starting darepod" "$LOG" | tail -1 | cut -d: -f1)
  SESSION=$(tail -n +${START:-1} "$LOG")

  BUILD=$(echo "$SESSION" | grep "Starting darepod" | tail -1)
  ERRORS=$(echo "$SESSION" | grep -E '\[WRN\]|\[ERR\]' | tail -40)
  FIRST_TS=$(echo "$SESSION" | head -1 | cut -c1-19)
  LAST_TS=$(echo "$SESSION"  | tail -1 | cut -c1-19)

  # The "Starting darepod" line carries network=X wallet_type=Y. Prefer
  # those values over the config when present — they reflect what the
  # daemon actually came up with after CLI flags and env overrides.
  if [ -n "$BUILD" ]; then
    LOG_NET=$(echo "$BUILD" | grep -oE 'network=[a-z]+' | cut -d= -f2)
    LOG_WALLET=$(echo "$BUILD" | grep -oE 'wallet_type=[a-z]+' \
      | cut -d= -f2)
    NETWORK=${LOG_NET:-$NETWORK}
    WALLET_TYPE=${LOG_WALLET:-$WALLET_TYPE}
  fi
else
  SESSION="" BUILD="" ERRORS="" FIRST_TS="" LAST_TS=""
fi
```

If the log file does not exist, the variables are empty — the report will
note "no log file found" in the affected sections instead of aborting.

### 4. Collect environment

```bash
OS=$(uname -srvm)
GO_VER=$(go version 2>/dev/null)
# Portable IPv6 check: any non-link-local, non-loopback inet6 address.
# ifconfig works on macOS and most Linux distros.
IPV6=$(ifconfig 2>/dev/null | awk '/inet6/ && !/fe80|::1/ {print $2; exit}')
IPV6=${IPV6:-none}
DAEMON_PID=$(pgrep -x darepod || echo "not running")

# Backend-specific reachability. Each wallet type has a different chain
# data source — only probe the one that's actually in use.
case "$WALLET_TYPE" in
  lwwallet)
    BACKEND_NAME="Esplora"
    BACKEND_URL=$(conf_get wallet.esploraurl)
    if [ -n "$BACKEND_URL" ]; then
      BACKEND_STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
        --connect-timeout 5 "${BACKEND_URL}/blocks/tip/height" \
        2>/dev/null || echo "unreachable")
    else
      BACKEND_STATUS="esploraurl not set in config"
    fi
    ;;
  btcwallet)
    BACKEND_NAME="neutrino (btcwallet)"
    # neutrino is a P2P backend — no HTTP probe. Report configured peers
    # and the fee endpoint reachability if set.
    PEERS=$(conf_get wallet.btcwallet_peers)
    ADDPEERS=$(conf_get wallet.btcwallet_addpeers)
    BACKEND_URL="peers=${PEERS:-<dns-seed>} addpeers=${ADDPEERS:-none}"
    FEE_URL=$(conf_get wallet.feeurl)
    if [ -n "$FEE_URL" ]; then
      FEE_STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
        --connect-timeout 5 "$FEE_URL" 2>/dev/null \
        || echo "unreachable")
      BACKEND_STATUS="feeurl=$FEE_URL → HTTP $FEE_STATUS"
    else
      BACKEND_STATUS="P2P only (no feeurl configured)"
    fi
    ;;
  lnd)
    BACKEND_NAME="lnd"
    LND_HOST=$(conf_get lnd.host)
    BACKEND_URL=${LND_HOST:-localhost:10009}
    # Best-effort TCP reachability — lnd's gRPC port is not HTTP.
    if command -v nc >/dev/null 2>&1; then
      host=${BACKEND_URL%:*}
      port=${BACKEND_URL##*:}
      if nc -z -w 3 "$host" "$port" 2>/dev/null; then
        BACKEND_STATUS="tcp reachable"
      else
        BACKEND_STATUS="tcp unreachable"
      fi
    else
      BACKEND_STATUS="unknown (nc not installed)"
    fi
    ;;
  *)
    BACKEND_NAME="$WALLET_TYPE"
    BACKEND_URL="(unknown backend type)"
    BACKEND_STATUS="not probed"
    ;;
esac
```

### 5. Read and redact config

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

### 6. Collect live wallet state

Only if `pgrep -x darepod` returns a PID. The RPC host can be overridden in
the config — read it before assuming the default.

```bash
RPC_HOST=$(conf_get rpc.listen)
RPC_HOST=${RPC_HOST:-127.0.0.1:10029}

~/go/bin/darepocli --rpcserver "$RPC_HOST" --no-tls balance 2>&1
~/go/bin/darepocli --rpcserver "$RPC_HOST" --no-tls list 2>&1 | head -30
```

If the daemon is not running, record "daemon not running at time of report".

### 7. Detect affected subsystems

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

Always include the `bug` label. Add subsystem labels on top. Also add a
network label (`network/signet`, `network/testnet`, etc.) derived from
`$NETWORK` so issues can be triaged per network.

### 8. Build the issue body

```
## Summary

{user's one-line description}

## Environment

- **OS**: {OS}
- **Go**: {GO_VER}
- **Daemon**: {commit= and version= from BUILD line}
- **Network**: {NETWORK}
- **Wallet type**: {WALLET_TYPE}
- **Chain backend**: {BACKEND_NAME} — {BACKEND_URL} → {BACKEND_STATUS}
- **IPv6**: {IPV6}
- **Daemon PID**: {DAEMON_PID}
- **Log file**: {LOG}
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

### 9. Preview and confirm

Print the full title and body to the terminal. Then ask:
**"File this issue now? [y/N]"**

If **yes**, run:

```bash
gh issue create \
  --title "{description}" \
  --body  "{body}" \
  --label "bug" \
  --label "network/{NETWORK}" \
  --label "{each detected subsystem label}"
```

Print the resulting issue URL.

If **no**, print the body so the user can copy-paste it manually and note:
`Open a new issue at: https://github.com/lightninglabs/darepo-client/issues/new`

### 10. If `gh` is not available or not authenticated

Skip the filing step. Print the body and the manual URL above. Do not treat
this as an error — a copy-pasteable report is still a good outcome.
