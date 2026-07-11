# darepoclicommands

## Purpose

Importable cobra command definitions for the `darepocli` CLI. Separated from
the `darepocli` main package so test harnesses and other binaries can
embed the same command tree.

## CLI Surface

The CLI surface is organized into cobra command groups that shape the
default `--help` face:

1. **Wallet verbs (group "Wallet", implicit, no parent)** — the everyday
   commands that map 1:1 to what a user does day-to-day. All are
   walletdkrpc-backed.
2. **Daemon introspection (group "Introspection")** — getinfo, schema, mcp
   (the built-in `help` command is grouped here too).
3. **Advanced subtrees (`ark`, `dev`, `recovery`)** — raw
   daemonrpc/devrpc commands for power users and operator runbooks.
   Hidden from the default `--help` via cobra `Hidden` (not a build tag),
   so they stay compiled and fully runnable in the shipped binary;
   `DAREPO_DEV=1` reveals them under an "Advanced" group. The env var only
   changes visibility — it never gates execution. `ark.*` stays on the
   `schema`/MCP surfaces; `dev` is reachable only as the generated (hidden)
   CLI subtree — it is not registered in `schema`/MCP — and `recovery` is
   exposed on neither.

The `swap.*` verbs were retired: `send`/`recv --offchain` and `activity`
cover them (the swapruntime daemon runtime that powers those verbs is
unchanged).

### Top-level wallet verbs

| Command | RPC | Description |
|---------|-----|-------------|
| `create` | `walletdkrpc.Create` | Initialize a new wallet (proxies GenSeed + InitWallet). Password from stdin / DAREPOD_WALLET_PASSWORD / --wallet_password_file. `--recover --mnemonic-file PATH` imports an existing aezeed mnemonic and recovers Ark wallet state instead of generating a fresh one |
| `unlock` | `walletdkrpc.Unlock` | Unlock an existing wallet (proxies UnlockWallet) |
| `send <dest>` | `walletdkrpc.Send` | Outbound payment. `--offchain` (default) for a BOLT-11 invoice via the swap subsystem; `--onchain` for an atomic on-chain send (`--sweep-all` drains). No prefix sniff. Blocks until the send reaches a terminal state by default (printing phase transitions and, for Lightning, the payment preimage); `--no-wait` returns immediately after dispatch |
| `recv` | `walletdkrpc.Recv` / `walletdkrpc.Deposit` | Inbound. `--offchain` (default) returns a Lightning invoice; `--onchain` returns a boarding address |
| `activity` | `walletdkrpc.List` | Unified wallet activity view. Defaults to table output; `--format json` returns structured JSON. `--pending` and `--kind` narrow rows |
| `activity inspect <id>` | `walletdkrpc.InspectActivity` | Correlated swap/VTXO/ledger detail for one activity entry |
| `balance` | `walletdkrpc.Balance` | Flat balance (confirmed_sat, pending_in_sat, pending_out_sat) |
| `exit --outpoint TXID:VOUT` | `walletdkrpc.Exit` | Queue a cooperative leave by default; unilateral unroll only fires with `--force-unroll-ack I_KNOW_WHAT_I_AM_DOING` |
| `exit status --outpoint TXID:VOUT` | `walletdkrpc.ExitStatus` | Query an exit/unroll job's status (proxies GetUnrollStatus). `--detailed` (default true) includes tree/CSV progress, block countdown, and fee breakdown; `--detailed=false` returns a coarse phase-only status |
| `exit summary` | `walletdkrpc.ExitSummary` | Aggregate totals across all in-progress exits |
| `exit plan --outpoint ...` | `walletdkrpc.GetExitPlan` | Preview backing-wallet funding readiness for one or more exits |
| `wallet-sweep --destination ADDR` | `walletdkrpc.SweepWallet` | Preview, or with `--broadcast` publish, a sweep of confirmed backing-wallet UTXOs (boarding outputs excluded; see `ark sweep`) |

### Daemon introspection

| Command | RPC | Description |
|---------|-----|-------------|
| `getinfo` | `daemonrpc.GetInfo` | Daemon readiness, version, network, wallet state |
| `schema` | (local) | JSON method registry — single source of truth for CLI commands and MCP tools |
| `mcp` | (local) | MCP server exposing the schema as tools |

### `ark.*` advanced commands

The everyday wallet verbs compose `walletdkrpc` end-to-end; the `ark`
parent surfaces the raw daemonrpc methods underlying them for callers
who want direct access.

| Command | RPC | Description |
|---------|-----|-------------|
| `ark vtxos {list,refresh,leave}` | `ListVTXOs` / `RefreshVTXOs` / `LeaveVTXOs` | VTXO inventory and lifecycle |
| `ark rounds {get,join,list,watch}` | `GetRound` / (join) / `ListRounds` / `WatchRounds` | Round FSM state; `join` commits queued intents into the next round (`vtxos refresh`/`leave` call it automatically) |
| `ark oor {receive,get,list}` | `NewReceiveScript` / `GetOORSession` / `ListOORSessions` | OOR session inspection |
| `ark board` | `Board` | Trigger boarding with confirmed UTXOs |
| `ark sweep [list]` | `SweepBoardingUTXOs` / `ListBoardingSweeps` | Boarding-timeout sweeps |
| `ark fees {estimate,history}` | `EstimateFee` / `GetFeeHistory` | Fee estimation and history |
| `ark listtransactions` | `ListTransactions` | Raw paginated transaction history |
| `ark send {inround,oor}` | `SendVTXO` / `SendOOR` | Raw in-round / OOR send (superseded by `send` for the wallet shape) |

### `recovery.*` advanced commands

Manual control of daemon-owned vHTLC recovery rows; normal swap clients
let the swap FSM arm and cancel recovery automatically.

| Command | RPC | Description |
|---------|-----|-------------|
| `recovery list` | `ListVHTLCRecoveries` | List recovery rows (ARMED rows are dormant) |
| `recovery status [id]` | `GetVHTLCRecoveryStatus` | Show one recovery row |
| `recovery escalate [id]` | `EscalateVHTLCRecovery` | Start on-chain unroll for an armed row; requires `--yes` on non-TTY stdin |
| `recovery cancel [id]` | `CancelVHTLCRecovery` | Record that cooperative settlement won; drop the armed row |

### `dev.*` advanced commands

Generated low-level daemon RPC CLI; see [`devrpc/`](devrpc/). Hidden from
the default `--help` (revealed with `DAREPO_DEV=1`) but always runnable.

## Key Helpers

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/cmd/darepocli/darepoclicommands.<Symbol>`.

- `NewRootCmd()` — top-level cobra command with all subcommands
  registered.
- `getDaemonConn()` / `getDaemonClient()` — TLS- and macaroon-by-default
  daemon gRPC dial; `--no-tls` / `--no-macaroons` opt out for local dev.
  `--tlscertpath` / `--macaroonpath` default to
  `<datadir>/data/<network>/{tls.cert,admin.macaroon}` (via
  `daemonTLSCertPath()` / `daemonMacaroonPath()`), expanding a leading `~`.
- `withWalletClient()` — maps `codes.Unimplemented` to
  `errWalletRPCDisabled` (with a pointer to `docs/walletdkrpc_build.md`)
  for daemons built without the walletdkrpc tag.
- `parseRequest()` — generic JSON-or-flags proto request parser
  (consumed by `ark.*` commands).
- `methodRegistry()` / `schemaMethod` / `schemaParam` —
  machine-readable schema for all CLI commands; shared source of
  truth for `schema` and MCP tool definitions. Built from the
  `walletAdmin`/`walletPayment`/`walletQuery`/`arkBase`/`arkVTXO`/
  `arkSend`/`arkObservable` sub-registries.
- `buildMCPServer()` — constructs the MCP server and registers every
  exposed RPC as a typed tool; split from `mcpServe` (which owns the
  daemon dial and stdio transport) so the tool surface is testable.
- `readPassword()` — reads wallet password from
  `DAREPOD_WALLET_PASSWORD` → `--wallet_password_file` → stdin → TTY.
  **Never from CLI args.**
- `validateDestination()` / `validateOutpoint()` /
  `validateFreeText()` — input hardening shared across the top-level
  wallet verbs (reject control chars, query/fragment chars, malformed
  outpoints, ambiguous flag combos).

## Relationships

- **Depends on**:
  - `rpc/walletdkrpc` (generated stubs for the top-level wallet verbs;
    `WalletService` / `WalletInspectionService` clients).
  - `daemonrpc` (generated stubs for `ark.*`, `recovery.*`, getinfo).
  - `rpcauth` (macaroon dial option for `getDaemonConn`).
  - `cmd/darepocli/darepoclicommands/devrpc` (the generated `dev`
    subtree; its registry references `swapclientrpc` service
    descriptors).
- **Depended on by**: `cmd/darepocli` (main entry point).

## Invariants

- The top-level wallet verbs ALWAYS register at the root regardless
  of build tags; if the daemon lacks the walletdkrpc tag, gRPC
  `Unimplemented` is mapped to `errWalletRPCDisabled` with an
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
- `exit` defaults to a cooperative leave; it only starts a unilateral
  on-chain unroll when `--force-unroll-ack` matches the literal string
  `I_KNOW_WHAT_I_AM_DOING`, and that flag is mutually exclusive with
  `--onchain-address`.
- `recovery escalate` refuses to run on non-interactive stdin unless
  `--yes` is passed — it never blocks on a y/N prompt an agent can't
  answer.

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
