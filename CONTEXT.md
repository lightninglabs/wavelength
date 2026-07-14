# wavecli Agent Context

> Operational guidance for AI agents driving `wavecli`. Read this
> before issuing commands.

## CLI shape

The CLI was flattened into "7 top-level verbs" plus parent commands for
power-user / raw RPC access:

| Top-level | Purpose | Requires walletdkrpc? |
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
| `ark *` | Raw waverpc per-feature commands | no |
| `dev *` | Generated low-level method-by-method access | no |

The user-facing verbs (`balance`, `recv`, `send`, `list`, `create`,
`unlock`) route through the `walletdkrpc` subserver, which is gated by
the `walletdkrpc` build tag. Default builds (`make build`, `make arktest`)
**do not** enable it. Without the tag those verbs error with:

```
daemon was not built with -tags walletdkrpc;
rebuild with `make build-walletdkrpc` or see docs/walletdkrpc_build.md
```

The `ark *` and `dev *` subtrees never need that tag.

## Critical Rules

1. **Pick the right surface for the build.** If you can't be sure
   walletdkrpc is enabled, use `ark *` (named) or `dev daemon <Method>`
   (raw). The top-level verbs are nicer ergonomics but fail loudly when
   walletdkrpc isn't built.

2. **Always use `--json` for structured input.** Pass the full RPC
   request payload directly:
   ```bash
   wavecli ark send oor --json '{"recipient":{"pubkey":"..."},"amount_sat":50000}'
   ```

3. **Always `--dry_run` before mutating.** Send and refresh commands
   support `--dry_run`. Use it to validate inputs:
   ```bash
   wavecli ark vtxos refresh --outpoint txid:0 --dry_run
   ```

4. **Output is always JSON on stdout.** Diagnostics and prompts go to
   stderr. Parse stdout only.

5. **Never pass passwords as CLI arguments.** Use one of:
   - Pipe: `echo -n 'pass' | wavecli create`
   - Env: `WAVED_WALLET_PASSWORD=pass wavecli unlock`
   - File: `wavecli unlock --wallet_password_file=/path`
   - JSON: `wavecli unlock --json '{"wallet_password":"cGFzcw=="}'`
     (base64 bytes)

   These require `walletdkrpc`. With the default build there is no
   manual wallet-create step — the daemon initializes on startup.

## Command Reference

### Daemon status

```bash
wavecli getinfo
```

### Wallet bring-up (walletdkrpc only)

```bash
wavecli create
echo -n 'mypass' | wavecli unlock
wavecli balance
wavecli recv --onchain     # boarding address
wavecli recv --offchain --amt 5000 --memo "coffee"   # Lightning invoice
```

### Raw boarding / balance (no walletdkrpc needed)

```bash
wavecli dev daemon NewAddress             # fresh boarding address
wavecli dev daemon GetBalance             # balances
wavecli ark board                         # board confirmed boarding UTXOs
wavecli ark board --target-vtxo-count 4   # fan out into N VTXOs
```

### VTXO operations

```bash
wavecli ark vtxos list
wavecli ark vtxos list --status live --min_amount 10000
wavecli ark vtxos list --ndjson | jq '.amount_sat'

# Refresh — see BUGS_FOUND.md bug-1/bug-2; the refresh path is
# currently not landing on the operator.
wavecli ark vtxos refresh --outpoint txid:0 --dry_run
wavecli ark vtxos refresh --outpoint txid:0
```

### Send operations

```bash
# OOR (direct via operator).
wavecli ark oor receive                                   # recipient's pubkey
wavecli ark send oor --pubkey <hex> --amount 25000
wavecli ark send oor --pubkey <hex> --amount 25000 \
  --idempotency_key my-attempt-1                            # retry-safe

# In-round (waits for next round).
wavecli ark send inround --to <bech32m> --amount 50000 --dry_run
wavecli ark send inround --json '{
  "recipients": [
    {"address":"bcrt1p...","amount_sat":50000},
    {"address":"bcrt1p...","amount_sat":30000}
  ],
  "dry_run": false
}'
```

### Rounds / sweeps / fees / history

```bash
wavecli ark rounds list --page-size 5
wavecli ark rounds get --round_id <uuid>
wavecli ark sweep
wavecli ark sweep list
wavecli ark fees estimate --amount 50000
wavecli ark fees history
wavecli ark listtransactions --limit 25 --type oor
```

### Exit (unilateral exit, formerly "unroll")

```bash
wavecli exit --outpoint <txid:vout>
wavecli exit status --outpoint <txid:vout>
```

## Common workflows

### Board funds (regtest, no walletdkrpc)

```bash
# 1. Fresh boarding address.
ADDR=$(wavecli dev daemon NewAddress | jq -r '.address')

# 2. Fund it (any regtest faucet / bitcoin-cli sendtoaddress).
bitcoin-cli sendtoaddress "$ADDR" 0.01
# (mine 6 confirmations)

# 3. Confirm balance.
wavecli dev daemon GetBalance

# 4. Register into the next round.
wavecli ark board
```

### OOR transfer (no walletdkrpc)

```bash
PUBKEY=$(wavecli-bob ark oor receive | jq -r '.pubkey_xonly_hex')
wavecli-alice ark send oor --pubkey "$PUBKEY" --amount 25000
wavecli-bob ark vtxos list   # bob's new VTXO appears within seconds
```

### Unilateral exit

```bash
VTXO=$(wavecli ark vtxos list | jq -r '.vtxos[0].outpoint')
wavecli exit --outpoint "$VTXO"
# mine through the CSV delay
wavecli exit status --outpoint "$VTXO"   # eventually COMPLETED
```

## Error codes

Structured errors are written to stderr as JSON:

```json
{"error":{"code":"INVALID_STATUS","message":"invalid status \"bogus\", valid: live, ..."}}
```

| Code | Meaning |
|------|---------|
| `EXECUTION_FAILED` | Command execution error (incl. missing walletdkrpc) |
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
