# darepoclicommands

## Purpose

Importable cobra command definitions for the `darepocli` CLI. Separated from
the `darepocli` main package so test harnesses and other binaries can
embed the same command tree.

## CLI Surface

The CLI surface is split into three tiers:

1. **Top-level wallet verbs (implicit, no parent)** — the seven everyday
   commands that map 1:1 to what a user does day-to-day. All seven are
   walletdkrpc-backed.
2. **Daemon introspection at root** — getinfo, schema, mcp, dev.
3. **Advanced subtrees (`ark`, `swap`)** — raw daemonrpc/swapclientrpc
   commands for power users and operator runbooks.

### Top-level wallet verbs

| Command | RPC | Description |
|---------|-----|-------------|
| `create` | `walletdkrpc.Create` | Initialize a new wallet (proxies GenSeed + InitWallet). Password from stdin / DAREPOD_WALLET_PASSWORD / --wallet_password_file |
| `unlock` | `walletdkrpc.Unlock` | Unlock an existing wallet (proxies UnlockWallet) |
| `send <dest>` | `walletdkrpc.Send` | Outbound payment. `--offchain` (default) for Lightning invoice, `--onchain` for cooperative leave. No prefix sniff |
| `recv` | `walletdkrpc.Recv` / `walletdkrpc.Deposit` | Inbound. `--offchain` (default) returns a Lightning invoice; `--onchain` returns a boarding address |
| `activity` | `walletdkrpc.List` | Unified wallet activity view. Defaults to table output; `--format json` returns structured JSON. `--pending` and `--kind` narrow rows |
| `balance` | `walletdkrpc.Balance` | Flat balance (confirmed_sat, pending_in_sat, pending_out_sat) |
| `exit --outpoint TXID:VOUT` | `walletdkrpc.Exit` | Queue a cooperative leave by default; starts unilateral unroll only with `--force-unroll-ack` |
| `exit status --outpoint TXID:VOUT` | `walletdkrpc.ExitStatus` | Query an exit job's status (proxies GetUnrollStatus) |
| `exit summary` | `walletdkrpc.ExitSummary` | Wallet-wide portfolio of in-progress exits: amount recovering, estimated fees, and estimated net recoverable |
| `exit plan --outpoint TXID:VOUT` | `walletdkrpc.GetExitPlan` | Preview backing-wallet funding readiness for one or more exits (repeatable `--outpoint`) |

### Daemon introspection

| Command | RPC | Description |
|---------|-----|-------------|
| `getinfo` | `daemonrpc.GetInfo` | Daemon readiness, version, network, wallet state |
| `schema` | (local) | JSON method registry — single source of truth for CLI commands and MCP tools |
| `mcp` | (local) | MCP server exposing the schema as tools |
| `dev *` | dev RPC | Generated dev RPC CLI (see `devrpc/`) |

### `ark.*` advanced commands

The everyday wallet verbs compose `walletdkrpc` end-to-end; the `ark`
parent surfaces the raw daemonrpc methods underlying them for callers
who want direct access.

| Command | RPC | Description |
|---------|-----|-------------|
| `ark vtxos {list,refresh,leave}` | `ListVTXOs` / `RefreshVTXOs` / `LeaveVTXOs` | VTXO inventory and lifecycle |
| `ark rounds {get,list,watch}` | `GetRound` / `ListRounds` / `WatchRounds` | Round FSM state |
| `ark oor {receive,get,list}` | `NewReceiveScript` / `GetOORSession` / `ListOORSessions` | OOR session inspection |
| `ark board` | `Board` | Trigger boarding with confirmed UTXOs |
| `ark sweep [list]` | `SweepBoardingUTXOs` / `ListBoardingSweeps` | Boarding-timeout sweeps |
| `ark fees {estimate,history}` | `EstimateFee` / `GetFeeHistory` | Fee estimation and history |
| `ark listtransactions` | `ListTransactions` | Raw paginated transaction history |
| `ark send {inround,oor}` | `SendVTXO` / `SendOOR` | Raw in-round / OOR send (superseded by `send` for the wallet shape) |

### `swap.*` advanced commands (swapruntime build tag)

| Command | RPC | Description |
|---------|-----|-------------|
| `swap list` | `swapclientrpc.ListSwaps` | List persisted swap summaries |
| `swap show <hash>` | `GetSwap` | Fetch one swap by payment hash |
| `swap receive` | `StartReceive` | Create a receive swap |
| `swap pay` | `StartPay` | Pay a Lightning invoice from Ark funds |
| `swap resume` | `ResumeSwap` | Wake a persisted swap worker |
| `swap watch` | `SubscribeSwaps` | Stream swap summary updates |

## Key Helpers

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands.<Symbol>`.

- `NewRootCmd()` — top-level cobra command with all subcommands
  registered.
- `getDaemonConn()` / `getDaemonClient()` — TLS-by-default daemon
  gRPC dial; `--no-tls` opts out for local dev.
- `withWalletClient()` — maps `codes.Unimplemented` to
  `errWalletRPCDisabled` (with a pointer to `docs/walletdkrpc_build.md`)
  for daemons built without the walletdkrpc tag.
- `getSwapClient()` — daemon `swapclientrpc` dial (`swapruntime`
  tag only).
- `parseRequest()` — generic JSON-or-flags proto request parser
  (consumed by `ark.*` commands).
- `methodRegistry()` / `schemaMethod` / `schemaParam` —
  machine-readable schema for all CLI commands; shared source of
  truth for `schema` and MCP tool definitions. Built from the
  `walletAdmin`/`walletPayment`/`walletQuery`/`arkBase`/`arkVTXO`/
  `arkSend` sub-registries.
- `readPassword()` — reads wallet password from
  `DAREPOD_WALLET_PASSWORD` → `--wallet_password_file` → stdin → TTY.
  **Never from CLI args.**
- `validateDestination()` / `validateOutpoint()` /
  `validateFreeText()` — input hardening shared across the seven
  top-level verbs (reject control chars, query/fragment chars,
  malformed outpoints, ambiguous flag combos).

## Relationships

- **Depends on**:
  - `rpc/walletdkrpc` (generated stubs for the seven top-level verbs;
    `WalletService` client).
  - `daemonrpc` (generated stubs for `ark.*` commands and getinfo).
  - `rpc/swapclientrpc` (generated stubs for `swap.*` commands,
    `swapruntime` tag only).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Invariants

- The seven top-level wallet verbs ALWAYS register at the root
  regardless of build tags; if the daemon lacks the walletdkrpc tag,
  gRPC `Unimplemented` is mapped to `errWalletRPCDisabled` with an
  actionable message pointing at the build doc.
- The `--offchain` / `--onchain` flags on `send` and `recv` are
  mutually exclusive; if neither is set, offchain is the default. The
  CLI does NOT sniff the destination string — the daemon performs
  the authoritative parse.
- The wallet password is NEVER read from argv. The supported sources
  are `DAREPOD_WALLET_PASSWORD` (highest priority), then
  `--wallet_password_file`, then piped stdin, then interactive prompt.
- JSON output (`stdout`) and diagnostic output (`stderr`) are kept on
  separate streams so shell pipelines can consume the JSON body while
  a human reading the terminal sees informative warnings.

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI
  reference.
- [docs/walletdkrpc_build.md](../../../docs/walletdkrpc_build.md) — Build
  modes and what the walletdkrpc tag enables.
- [rpc/walletdkrpc/CLAUDE.md](../../../rpc/walletdkrpc/CLAUDE.md) — Proto
  schema and per-message invariants.
- [swapwallet/CLAUDE.md](../../../swapwallet/CLAUDE.md) — Daemon-side
  implementation of the wallet verbs.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
