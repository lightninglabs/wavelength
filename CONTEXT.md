# darepocli Agent Context

> Operational guidance for AI agents driving `darepocli`. Read this
> before issuing commands.

## CLI shape

The CLI was flattened into "7 top-level verbs" plus parent commands for
power-user / raw RPC access:

| Top-level | Purpose | Requires walletrpc? |
|-----------|---------|---------------------|
| `getinfo` | Daemon status | no |
| `create` | Create a new wallet | yes |
| `unlock` | Unlock an existing wallet | yes |
| `balance` | Wallet balances | yes |
| `recv` | Receive (boarding addr / Lightning invoice) | yes |
| `send` | Send (Lightning invoice / onchain leave) | yes |
| `list` | Unified activity / VTXOs / onchain history | yes |
| `exit` | Unilateral exit a VTXO | no |
| `mcp` | MCP server | yes |
| `ark *` | Raw daemonrpc per-feature commands | no |
| `dev *` | Generated low-level method-by-method access | no |

The user-facing verbs (`balance`, `recv`, `send`, `list`, `create`,
`unlock`) route through the `walletrpc` subserver, which is gated by
the `walletrpc` build tag. Default builds (`make build`, `make arktest`)
**do not** enable it. Without the tag those verbs error with:

```
daemon was not built with -tags walletrpc;
rebuild with `make build-walletrpc` or see docs/walletrpc_build.md
```

The `ark *` and `dev *` subtrees never need that tag.

## Critical Rules

1. **Pick the right surface for the build.** If you can't be sure
   walletrpc is enabled, use `ark *` (named) or `dev daemon <Method>`
   (raw). The top-level verbs are nicer ergonomics but fail loudly when
   walletrpc isn't built.

2. **Always use `--json` for structured input.** Pass the full RPC
   request payload directly:
   ```bash
   darepocli ark send oor --json '{"recipient":{"pubkey":"..."},"amount_sat":50000}'
   ```

3. **Always `--dry_run` before mutating.** Send and refresh commands
   support `--dry_run`. Use it to validate inputs:
   ```bash
   darepocli ark vtxos refresh --outpoint txid:0 --dry_run
   ```

4. **Output is always JSON on stdout.** Diagnostics and prompts go to
   stderr. Parse stdout only.

5. **Never pass passwords as CLI arguments.** Use one of:
   - Pipe: `echo -n 'pass' | darepocli create`
   - Env: `DAREPOD_WALLET_PASSWORD=pass darepocli unlock`
   - File: `darepocli unlock --wallet_password_file=/path`
   - JSON: `darepocli unlock --json '{"wallet_password":"cGFzcw=="}'`
     (base64 bytes)

   These require `walletrpc`. With the default build there is no
   manual wallet-create step — the daemon initializes on startup.

## Command Reference

### Daemon status

```bash
darepocli getinfo
```

### Wallet bring-up (walletrpc only)

```bash
darepocli create
echo -n 'mypass' | darepocli unlock
darepocli balance
darepocli recv --onchain     # boarding address
darepocli recv --offchain --amt 5000 --memo "coffee"   # Lightning invoice
```

### Raw boarding / balance (no walletrpc needed)

```bash
darepocli dev daemon NewAddress             # fresh boarding address
darepocli dev daemon GetBalance             # balances
darepocli ark board                         # board confirmed boarding UTXOs
darepocli ark board --target-vtxo-count 4   # fan out into N VTXOs
```

### VTXO operations

```bash
darepocli ark vtxos list
darepocli ark vtxos list --status live --min_amount 10000
darepocli ark vtxos list --ndjson | jq '.amount_sat'

# Refresh — see BUGS_FOUND.md bug-1/bug-2; the refresh path is
# currently not landing on the operator.
darepocli ark vtxos refresh --outpoint txid:0 --dry_run
darepocli ark vtxos refresh --outpoint txid:0
```

### Send operations

```bash
# OOR (direct via operator).
darepocli ark oor receive                                   # recipient's pubkey
darepocli ark send oor --pubkey <hex> --amount 25000
darepocli ark send oor --pubkey <hex> --amount 25000 \
  --idempotency_key my-attempt-1                            # retry-safe

# In-round (waits for next round).
darepocli ark send inround --to <bech32m> --amount 50000 --dry_run
darepocli ark send inround --json '{
  "recipients": [
    {"address":"bcrt1p...","amount_sat":50000},
    {"address":"bcrt1p...","amount_sat":30000}
  ],
  "dry_run": false
}'
```

### Rounds / sweeps / fees / history

```bash
darepocli ark rounds list --page-size 5
darepocli ark rounds get --round_id <uuid>
darepocli ark sweep
darepocli ark sweep list
darepocli ark fees estimate --amount 50000
darepocli ark fees history
darepocli ark listtransactions --limit 25 --type oor
```

### Exit (unilateral exit, formerly "unroll")

```bash
darepocli exit --outpoint <txid:vout>
darepocli exit status --outpoint <txid:vout>
```

## Common workflows

### Board funds (regtest, no walletrpc)

```bash
# 1. Fresh boarding address.
ADDR=$(darepocli dev daemon NewAddress | jq -r '.address')

# 2. Fund it (any regtest faucet / bitcoin-cli sendtoaddress).
bitcoin-cli sendtoaddress "$ADDR" 0.01
# (mine 6 confirmations)

# 3. Confirm balance.
darepocli dev daemon GetBalance

# 4. Register into the next round.
darepocli ark board
```

### OOR transfer (no walletrpc)

```bash
PUBKEY=$(darepocli-bob ark oor receive | jq -r '.pubkey_xonly_hex')
darepocli-alice ark send oor --pubkey "$PUBKEY" --amount 25000
darepocli-bob ark vtxos list   # bob's new VTXO appears within seconds
```

### Unilateral exit

```bash
VTXO=$(darepocli ark vtxos list | jq -r '.vtxos[0].outpoint')
darepocli exit --outpoint "$VTXO"
# mine through the CSV delay
darepocli exit status --outpoint "$VTXO"   # eventually COMPLETED
```

## Error codes

Structured errors are written to stderr as JSON:

```json
{"error":{"code":"INVALID_STATUS","message":"invalid status \"bogus\", valid: live, ..."}}
```

| Code | Meaning |
|------|---------|
| `EXECUTION_FAILED` | Command execution error (incl. missing walletrpc) |
| `INVALID_STATUS` | Unknown VTXO status filter value |

## Global flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:10029` | Daemon gRPC address |
| `--tlscertpath` | (empty) | Path to daemon TLS cert |
| `--no-tls` | `false` | Disable TLS (dev/regtest) |
| `--json` | (empty) | Raw JSON request payload |

## Input validation

- **Outpoints** must be `txid:index` — reject embedded `?`, `#`, `%`.
- **Addresses** must not contain control characters (bytes < 0x20).
- **Amounts** must be positive integers.
- **Enum values** are validated against known proto values; errors list
  valid options.
