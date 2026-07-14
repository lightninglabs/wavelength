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

Collect runtime state from the current waved test session and file a
structured GitHub issue against this repository.

## Body discipline (read before collecting anything)

The point of this skill is to give the team a report they can act on in
under a minute, not a dump of everything the daemon knows. Target the
final issue body at **≤ 5 KB / ~100 lines**. If a section is bigger
than the failure it's documenting, trim it.

Concrete rules:

- The body leads with **what failed and why** (Summary + Failure
  sequence), not with environment metadata.
- The "Failure sequence" is a *curated* excerpt of the log — the 5–10
  lines that actually trace the bug. Strip routine state-machine
  ticks, polling loops, and unrelated subsystems.
- The full session log goes in a **gist**, not in the body (see step
  8a). The body links to it. If gist upload fails, note the local log
  path and stop — do not paste hundreds of lines inline.
- Wallet state: only the failing entry's relevant fields. Do not paste
  the whole `list` JSON.
- Environment: platform + arch + go version + daemon commit + network
  + wallet backend. Not the full `uname -srvm`, not PID, not IPv6
  address unless the bug is network-layer.
- Reproduction steps: write the actual steps that produced the bug. If
  the only honest thing you can write is a TODO, ask the user before
  filing.
- Cut secondary observations into separate issues. One issue, one bug.

## Steps

### 1. Get the description

If the user provided an argument, use it as the issue title. If not, ask for
one sentence describing what went wrong before collecting anything.

### 2. Detect the active network and chain backend

Do **not** assume a network (mainnet/testnet/signet/regtest/simnet) or a
chain backend (lwwallet+Esplora / btcwallet+neutrino / lnd). Detect both
from config first, then cross-check against the most recent daemon log.

```bash
CONF=~/.waved/waved.conf

# Helper: read a key from the conf file (uncommented), strip whitespace and
# surrounding quotes. Returns empty if missing.
conf_get() {
  grep -E "^[[:space:]]*$1[[:space:]]*=" "$CONF" 2>/dev/null \
    | tail -1 | cut -d= -f2- \
    | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//; s/^"(.*)"$/\1/'
}

# Config-derived values. Defaults match waved/config.go:
#   network defaults to mainnet, wallet.type defaults to lwwallet.
NETWORK=$(conf_get network)
NETWORK=${NETWORK:-mainnet}
WALLET_TYPE=$(conf_get wallet.type)
WALLET_TYPE=${WALLET_TYPE:-lwwallet}

# Cross-check against the actual log directory tree. If the config-derived
# network has no log file but another network's log does, prefer the
# directory that has a recent log — the operator may have switched
# networks without updating the conf, or the conf may be elsewhere.
LOG_ROOT=~/.waved/logs
LOG="$LOG_ROOT/$NETWORK/waved.log"
if [ ! -f "$LOG" ]; then
  ALT=$(ls -t "$LOG_ROOT"/*/waved.log 2>/dev/null | head -1)
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
  START=$(grep -n "Starting waved" "$LOG" | tail -1 | cut -d: -f1)
  SESSION=$(tail -n +${START:-1} "$LOG")

  BUILD=$(echo "$SESSION" | grep "Starting waved" | tail -1)
  ERRORS=$(echo "$SESSION" | grep -E '\[WRN\]|\[ERR\]' | tail -40)
  FIRST_TS=$(echo "$SESSION" | head -1 | cut -c1-19)
  LAST_TS=$(echo "$SESSION"  | tail -1 | cut -c1-19)

  # The "Starting waved" line carries network=X wallet_type=Y. Prefer
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
DAEMON_PID=$(pgrep -x waved || echo "not running")

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

Read `~/.waved/waved.conf` and redact secrets before including in the
report. Replace the value of any line whose key matches one of these
patterns with `<REDACTED>`:

- `bitcoind.user`, `bitcoind.pass`, `bitcoind.rpcuser`, `bitcoind.rpcpass`,
  `bitcoind.rpcpassword`
- any key ending in `password`, `pass`, `secret`, `token`, or `apikey`

```bash
CONFIG=$(sed -E '
  s/^([[:space:]]*bitcoind\.(user|pass|rpcuser|rpcpass(word)?))[[:space:]]*=.*/\1=<REDACTED>/;
  s/^([[:space:]]*[^=]*(password|secret|token|apikey))[[:space:]]*=.*/\1=<REDACTED>/I
' ~/.waved/waved.conf 2>/dev/null)
```

Use `$CONFIG` (not the raw file) in the report body.

### 6. Collect live wallet state

Only if `pgrep -x waved` returns a PID. The RPC host can be overridden in
the config — read it before assuming the default.

```bash
RPC_HOST=$(conf_get rpc.listen)
RPC_HOST=${RPC_HOST:-127.0.0.1:10029}

~/go/bin/wavecli --rpcserver "$RPC_HOST" --no-tls balance 2>&1
~/go/bin/wavecli --rpcserver "$RPC_HOST" --no-tls list 2>&1 | head -30
```

If the daemon is not running, record "daemon not running at time of report".

### 7. Detect affected subsystems

Scan the session's `[WRN]`/`[ERR\]` lines for these log prefixes and collect
every one that appears:

| Prefix | Label      |
|--------|------------|
| LWWL   | lwwallet   |
| ARKW   | wallet     |
| WRPC   | walletdkrpc |
| ROND   | round      |
| OORC   | oor        |
| SVRC   | serverconn |
| DRPD   | daemon     |
| SWAP   | swap       |

Always include the `bug` label. Add subsystem labels on top. Also add a
network label (`network/signet`, `network/testnet`, etc.) derived from
`$NETWORK` so issues can be triaged per network.

### 8a. Upload the full session log as a secret gist

The body links to the gist — do not paste session logs inline.

```bash
GIST_URL=""
if [ -f "$LOG" ] && [ -s "$LOG" ]; then
  GIST_URL=$(gh gist create "$LOG" \
    --desc "waved session log — $NETWORK — for lightninglabs/wavelength issue" \
    2>/dev/null | tail -1)
fi
```

Notes:

- `gh gist create` produces a secret (unlisted) gist by default —
  anyone with the URL can read it, but it isn't indexed or shown on
  the user's profile. Do **not** pass `--public`; testnet/signet
  logs still contain wallet pubkeys and node identifiers worth
  keeping out of search results.
- If `gh gist create` fails (missing `gist` scope, network error,
  etc.) `GIST_URL` will be empty. The template in step 8 handles that
  — it falls back to "available on request, local path `{LOG}`" and
  the team can ask for it directly.

### 8. Build the issue body

````
## Summary

{2–4 sentence description of the failure. Lead with the user-visible
symptom; name the component that rejected; flag any disagreement
between layers (e.g. inner FSM Failed but outer entry still Pending).}

## Environment

- **OS / arch**: {short — e.g. "macOS Darwin 25.2.0, arm64"}
- **Go**: {go version, no `go version ` prefix}
- **Daemon**: version={…} commit={…}
- **Network**: {NETWORK}
- **Wallet backend**: {WALLET_TYPE} → {BACKEND_URL} ({BACKEND_STATUS})

## Config (`~/.waved/waved.conf`, secrets redacted)

```
{CONFIG}
```

## Failure sequence

```
{5–10 curated log lines, in order, that trace the bug. Use ellipses
on long hashes so the block stays scannable. Include the line that
carries the rejection reason, the FSM transition into Failed, and
the parent submit/funding line that anchors what was attempted.}
```

{If the rejection's full ErrorReason isn't visible in the line above,
quote it on its own:}

> `{full ErrorReason}`

{One short paragraph if the failure is logged below WRN, surfaces no
failure_reason on the user-facing entry, or has any other
observability gap worth flagging.}

## Correlation IDs

- **mailbox_id / identity_pubkey**: `{…}`
- **session_id** (if applicable): `{…}`
- **payment_hash / invoice_hash** (if applicable): `{…}`
- **input VTXO / outpoint** (if applicable): `{…}`
- **operator key from getinfo**: `{…}`

{Include only the IDs that are relevant. Drop the bullet if N/A.}

## Hypothesis (optional)

{One paragraph. Only include if there is a plausible cause and you
can name what would falsify it. If you don't have one, omit this
section — don't speculate.}

## Wallet state at failure

```
balance: confirmed_sat={…} pending_in_sat={…} pending_out_sat={…}

{failing entry, key fields only — id, kind, status, amount_sat,
failure_reason. Do not paste the entire activity JSON.}
```

## Reproduction steps

1. {actual step}
2. {actual step}
3. {observed outcome — include the exact log signature the team
   can grep for}

## Logs

Full session log: {GIST_URL — or, if gist upload failed,
"available on request, local path `{LOG}`"}
````

### 9. Preview and confirm

Before previewing, check the body size. If it exceeds ~6 KB or ~120
lines, stop and trim — the discipline goal is much tighter than
GitHub's 65 KB hard limit. Usual culprits: pasted JSON, uncurated log
excerpts, secondary observations that belong in their own issue.

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
`Open a new issue at: https://github.com/lightninglabs/wavelength/issues/new`

### 10. If `gh` is not available or not authenticated

Skip the filing step. Print the body and the manual URL above. Do not treat
this as an error — a copy-pasteable report is still a good outcome.
