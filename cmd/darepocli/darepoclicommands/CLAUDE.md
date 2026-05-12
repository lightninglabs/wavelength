# darepoclicommands

## Purpose

Importable cobra command definitions for the `darepocli` CLI. Separated from
the `darepocli` main package so that test harnesses and other binaries can
embed the same command tree.

## Key Types

- `NewRootCmd()` — Creates the top-level cobra command with all subcommands.
- `getDaemonClient()` — Connects to the daemon's gRPC endpoint and returns a
  `daemonrpc.DaemonServiceClient`. TLS is enabled by default; `--no-tls`
  opts out for local development.
- `getSwapClient()` — Connects to the daemon's `swapclientrpc` subserver
  (built only with the `swapruntime` tag).
- `parseRequest()` — Generic JSON-or-flags request parser for proto messages.
- `methodRegistry()` / `schemaMethod` / `schemaParam` — Machine-readable
  schema for all CLI commands; shared source of truth for the `schema`
  subcommand and MCP tool definitions. Built from `baseMethodRegistry`,
  `vtxoLifecycleMethodRegistry`, and `sendMethodRegistry` sub-registries
  to stay within the `funlen` lint cap.

## Commands

### Core wallet and round commands

| Command | RPC | Description |
|---------|-----|-------------|
| `board` | `Board` | Trigger boarding with confirmed UTXOs |
| `rounds get` | `GetRound` | Fetch one round by server-assigned round id |
| `rounds list` | `ListRounds` | List round FSM states with pagination and optional state/time filters |
| `rounds watch` | `WatchRounds` | Stream round state updates |
| `vtxos list` | `ListVTXOs` | List wallet VTXOs |
| `send` | `SendVTXO` / `SendOOR` | Send to address |
| `balance` | `GetBalance` | Show wallet balances |

### OOR commands (`cmd_oor.go`)

| Command | RPC | Description |
|---------|-----|-------------|
| `oor receive` | `NewReceiveScript` | Register a new receive script and print the receive address |
| `oor get` | `GetOORSession` | Fetch one locally known OOR session by session id |
| `oor list` | `ListOORSessions` | List locally known OOR sessions; `--status` accepts `all`/`pending`/`completed`/`failed`; `--direction` accepts `all`/`outgoing`/`incoming` |

### Sweep commands (`cmd_sweep.go`)

| Command | RPC | Description |
|---------|-----|-------------|
| `sweep` | `SweepBoarding` | Scan mature boarding UTXOs; `--broadcast` publishes aggregate sweep. Flags: `--outpoint`, `--fee-rate-sat-per-vbyte`, `--conf-target`, `--sweep-address` |
| `sweep list` | `ListBoardingSweeps` | List tracked boarding sweep transactions with status, fee, confirm height, and per-input spend state |

### Transaction history (`cmd_transactions.go`)

| Command | RPC | Description |
|---------|-----|-------------|
| `listtransactions` | `ListTransactions` | Newest-first paginated transaction history with `--from`, `--to`, `--limit`, `--offset`, `--type` (boarding/round/oor/sweep) filters |

### Fees

| Command | RPC | Description |
|---------|-----|-------------|
| `fees estimate` | `EstimateFee` | Print an itemized fee breakdown for a given VTXO amount; flags a dust-level amount on stderr |
| `fees history` | `GetFeeHistory` | Paginate the client-side ledger entries and print the cumulative operator fee total |

### Unroll (unilateral exit)

| Command | RPC | Description |
|---------|-----|-------------|
| `unroll` | `Unroll` | Trigger a unilateral exit for the VTXO at `--outpoint txid:index`. Routes through the VTXO manager's `ForceUnrollRequest` path so the FSM transitions cleanly; the registry job is created async via the chain resolver seam. Response includes `Created` (false if already exiting) and the `ActorId` to poll. |
| `unroll status` | `GetUnrollStatus` | Query progress for an unroll job by `--outpoint`. Reads through to the live registry first and falls back to the persisted `unilateral_exit_jobs` table for evicted/terminal jobs; `Found=false` (not an error) distinguishes "no such job" from lookup failure. |

### Lightning swap commands (`cmd_swap_rpc.go`, `swapruntime` tag only)

These commands connect to the daemon-owned `SwapClientService` subserver.
The daemon owns the background worker; CLI exit does not cancel an admitted swap.

| Command | RPC | Description |
|---------|-----|-------------|
| `swap list` | `ListSwaps` | List persisted swap summaries; `--pending` filters to non-terminal |
| `swap show <hash>` | `GetSwap` | Fetch one swap summary by hex payment hash |
| `swap receive` | `StartReceive` | Create a receive swap and return the BOLT-11 invoice |
| `swap pay` | `StartPay` | Pay a Lightning invoice from Ark funds |
| `swap resume` | `ResumeSwap` | Wake up a persisted swap worker (idempotent) |
| `swap watch` | `SubscribeSwaps` | Stream coarse swap summary updates; `--existing` emits current rows first |

### Direct-client swap commands (`cmd_swap.go`, `swapdirect && !swapruntime` tag)

Standalone swap execution without a running daemon. Connects directly to the
swap server and an isolated SQLite database. Flags: `--swapserver`,
`--swapserver-tlscert`, `--swapserver-insecure`, `--swapdb`.

## Relationships

- **Depends on**: `daemonrpc` (generated gRPC client stubs for the main
  daemon service), `rpc/swapclientrpc` (generated gRPC stubs for the
  optional swap subserver, `swapruntime` tag only).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Deep Docs

- [docs/daemon_cli_guide.md](../../../docs/daemon_cli_guide.md) — Full CLI reference.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
