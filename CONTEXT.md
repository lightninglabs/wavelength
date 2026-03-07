# darepocli Agent Context

> Operational guidance for AI agents driving `darepocli`. Read this
> before issuing commands.

## Critical Rules

1. **Always use `--json` for structured input.** Pass the full RPC
   request payload directly — zero translation loss:
   ```bash
   darepocli send oor --json '{"address":"tb1p...","amount_sat":50000}'
   ```

2. **Always `--dry-run` before mutating.** All send and refresh
   commands support `--dry-run`. Use it to validate inputs before
   committing:
   ```bash
   darepocli send inround --json '{"recipients":[...],"dry_run":true}'
   ```

3. **Output is always JSON on stdout.** Diagnostics and prompts go to
   stderr. Parse stdout only.

4. **Never pass passwords as CLI arguments.** Use one of:
   - Pipe: `echo -n 'pass' | darepocli wallet create`
   - Env: `DAREPOD_WALLET_PASSWORD=pass darepocli wallet unlock`
   - File: `darepocli wallet unlock --wallet_password_file=/path`
   - JSON: `darepocli wallet unlock --json '{"wallet_password":"cGFzcw=="}'`
     (base64-encoded bytes)

## Command Reference

### Daemon Status

```bash
darepocli getinfo
```

### Wallet Operations

```bash
# Create wallet (interactive — generates seed, prompts for password)
darepocli wallet create

# Create wallet (agent path — supply full InitWalletRequest)
darepocli wallet create --json '{"mnemonic":["word1","word2",...],"wallet_password":"cGFzcw==","seed_passphrase":""}'

# Unlock wallet
echo -n 'mypass' | darepocli wallet unlock

# Check balance
darepocli wallet balance

# Generate boarding address
darepocli wallet newaddress
```

### VTXO Operations

```bash
# List all VTXOs
darepocli vtxos list

# List with filters (bespoke flags)
darepocli vtxos list --status live --min-amount 10000

# List with filters (JSON payload)
darepocli vtxos list --json '{"status_filter":"VTXO_STATUS_LIVE","min_amount_sat":10000}'

# Refresh VTXOs (extend expiry in next round)
darepocli vtxos refresh --all --dry-run
darepocli vtxos refresh --outpoint txid:0,txid:1
```

### Send Operations

```bash
# In-round send (bespoke flags)
darepocli send inround --to tb1p... --amount 50000 --dry-run

# In-round send (JSON — supports multiple recipients natively)
darepocli send inround --json '{
  "recipients": [
    {"address": "tb1p...", "amount_sat": 50000},
    {"address": "tb1p...", "amount_sat": 30000}
  ],
  "dry_run": false
}'

# Out-of-round send
darepocli send oor --to tb1p... --amount 25000
```

## Common Workflows

### Board Funds (Regtest)

```bash
# 1. Get a boarding address.
ADDR=$(darepocli wallet newaddress | jq -r '.address')

# 2. Fund it (via bitcoin-cli or faucet).
bitcoin-cli sendtoaddress "$ADDR" 0.01

# 3. Wait for confirmation, then check balance.
darepocli wallet balance
```

### Refresh Expiring VTXOs

```bash
# 1. Find expiring VTXOs.
darepocli vtxos list --status expiring

# 2. Refresh all of them.
darepocli vtxos refresh --all --dry-run   # validate first
darepocli vtxos refresh --all             # execute
```

## Error Codes

Structured errors are written to stderr as JSON:

```json
{"error":{"code":"INVALID_STATUS","message":"invalid status \"bogus\", valid: live, ..."}}
```

| Code | Meaning |
|------|---------|
| `EXECUTION_FAILED` | Command execution error |
| `INVALID_STATUS` | Unknown VTXO status filter value |

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--rpcserver` | `localhost:10029` | Daemon gRPC address |
| `--tlscertpath` | (empty) | Path to daemon TLS cert |
| `--no-tls` | `false` | Disable TLS (dev/regtest) |
| `--json` | (empty) | Raw JSON request payload |

## Input Validation

- **Outpoints** must be `txid:index` format — reject embedded `?`, `#`, `%`.
- **Addresses** must not contain control characters (bytes < 0x20).
- **Amounts** must be positive integers.
- **Enum values** are validated against known proto values; errors list
  valid options.
