# wavelength

`wavelength` is a self-custodial Bitcoin wallet system written in Go. It
unifies three layers that usually live in separate tools: an Ark client, a
Lightning swap engine, and an on-chain wallet. All of it runs behind one
long-running daemon (`waved`) and a companion CLI (`wavecli`), so a user can
board into Ark, hold and transfer VTXOs, swap into and out of the Lightning
Network, send and receive on-chain, and unilaterally exit to the chain at
any time, all while keeping sole custody of their coins.

The daemon is a self-contained, restart-safe process that talks to an Ark
operator over a durable mailbox transport, manages its own on-chain wallet,
and exposes a typed gRPC + REST API for host applications.

---

## Highlights

- **Self-contained daemon.** One binary, embedded wallet, durable state.
  Crash-safe across restarts via durable actor mailboxes.
- **Three wallet backends.**
  - `lwwallet`: in-process btcwallet + Esplora REST (no external node).
  - `btcwallet`: in-process btcwallet + Neutrino (compact block filters).
  - `lnd`: uses an existing lnd node for signing and chain access.
- **Full Ark client surface.** Boarding, in-round transfers, out-of-round
  transfers, refresh, cooperative leave, and unilateral exit.
- **Lightning swaps.** Optional swap subsystem (`swapruntime`) provides
  Lightning-to-Ark and Ark-to-Lightning atomic swaps with a durable FSM.
- **Wallet RPC facade.** Optional flat, swap-vocabulary-free wallet API
  (`wavewalletrpc`) exposes seven core verbs: `create`, `unlock`, `send`,
  `recv`, `activity`, `balance`, `exit`.
- **Host-app SDK.** `sdk/ark`, `sdk/swaps`, and `sdk/wavewalletdk` embed the
  daemon in-process and expose typed Go APIs over a private transport.
- **gRPC + REST.** Every RPC is reachable over gRPC or via grpc-gateway HTTP.
- **MCP integration.** `wavecli mcp serve` exposes the daemon to AI agents
  as typed tool calls.

---

## Quick Start

```bash
# Clone, build, and install waved + wavecli with the wallet RPC surface
# enabled (recommended; gives you the top-level wallet verbs).
git clone https://github.com/lightninglabs/wavelength.git
cd wavelength
make install-wavewalletrpc

# Start the daemon against a local regtest Ark operator + Esplora. The local
# and remote mailbox IDs are derived from the client and operator pubkeys, so
# there are no mailbox-id flags to set.
waved \
  --network=regtest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000 \
  --wallet.password_file=/path/to/password_file \
  --server.host=localhost:10010 \
  --server.insecure \
  --rpc.listenaddr=localhost:10029

# In another shell. wavecli needs TLS + the daemon's admin macaroon; for
# this local regtest daemon use plaintext instead (no TLS, no macaroon) on
# the regtest network. See INSTALL.md for the TLS setup a real instance uses.
alias wave='wavecli --no-tls --no-macaroons --network=regtest'
wave create
wave recv   --onchain
wave balance
wave ark board
```

For a full end-to-end regtest walkthrough (with `nigiri` and a funded
boarding UTXO), see [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md).

Detailed installation instructions (prerequisites, build variants, backend
requirements, troubleshooting) live in [`INSTALL.md`](INSTALL.md).

---

## Build Variants

`waved` is intentionally modular. The default build is minimal; optional
subsystems are gated behind build tags so hosts that do not need them pay
nothing in binary size or surface area.

| Target                     | Build tags                  | What it adds                                              |
|----------------------------|-----------------------------|-----------------------------------------------------------|
| `make build`               | _(none)_                    | Core Ark client. Power-user `ark *` and `dev *` CLI only. |
| `make build-swapruntime`   | `swapruntime`               | + Lightning swap subsystem (`sdk/swaps`) powering `send`/`recv --offchain`. |
| `make build-wavewalletrpc`   | `wavewalletrpc swapruntime`   | + Wallet RPC subserver and top-level wallet verbs.        |
| `make install`             | _(none)_                    | Installs the core build to `$GOPATH/bin`.                 |
| `make install-swapruntime` | `swapruntime`               | Installs the swap-enabled build.                          |
| `make install-wavewalletrpc` | `wavewalletrpc swapruntime`   | Installs the full wavewalletrpc-enabled build (recommended).|

The `wavewalletrpc` build is a strict superset of `swapruntime`. Most users
want `make install-wavewalletrpc`.

See [`docs/wavewalletrpc_build.md`](docs/wavewalletrpc_build.md) for the full
matrix, what each tag enables, and what the CLI surface looks like in each
mode.

---

## CLI at a Glance

The everyday **Wallet** verbs and daemon **Introspection** are the default
`--help` face. The advanced `ark` / `dev` / `recovery` subtrees are hidden
from `--help` (set `WAVELENGTH_DEV=1` to reveal them) but stay fully runnable —
`wavecli ark …` works with or without the env var.

```
wavecli
├── getinfo                   daemon status                          (all builds)
├── balance / recv / send     unified wallet verbs                   (wavewalletrpc)
├── create / unlock           wallet bring-up                        (wavewalletrpc)
├── activity [inspect]        unified wallet activity history        (wavewalletrpc)
├── exit [status]             cooperatively exit a VTXO              (all builds)
├── mcp serve                 MCP server for AI agents               (wavewalletrpc)
├── schema                    JSON dump of all CLI methods           (all builds)
├── ark ...                   power-user Ark RPCs         (hidden; all builds)
│   ├── board                 board confirmed boarding UTXOs
│   ├── vtxos {list|refresh|leave}
│   ├── oor   {receive|get|list}
│   ├── send  {oor|inround}
│   ├── rounds {get|list|watch|join}
│   ├── sweep [list]
│   ├── fees   {estimate|history}
│   └── listtransactions
├── recovery {list|status|escalate|cancel}          (hidden; all builds)
└── dev <service> <Method>    raw gRPC, e.g. dev daemon GetBalance (hidden; all builds)
```

`swap` is no longer a CLI verb: `send`/`recv --offchain` and `activity`
cover it, and a stale `wavecli swap …` fails with a hint toward
`send`/`recv`. The `swapruntime` daemon runtime that powers the offchain
verbs is unchanged.

Full CLI reference: [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md).

---

## Configuration

All daemon flags can also be set in `~/.waved/waved.conf` or via
environment variables (`WAVED_*`). The canonical sample is
[`sample-waved.conf`](sample-waved.conf), and the full flag reference
is in [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md#daemon-flags-reference).

Common knobs:

| Flag                       | Default            | Purpose                                  |
|----------------------------|--------------------|------------------------------------------|
| `--datadir`                | `~/.waved`       | Root data directory.                     |
| `--network`                | `mainnet`          | `mainnet`, `testnet`, `testnet4`, `signet`, `regtest`, `simnet`. |
| `--wallet.type`            | `lwwallet`         | `lwwallet`, `btcwallet`, or `lnd`.       |
| `--wallet.esploraurl`      |                    | Esplora REST URL (`lwwallet`).           |
| `--lnd.host`               | `localhost:10009`  | lnd gRPC (`lnd` backend).                |
| `--server.host`            | network default    | Ark operator address override.           |
| `--server.transport`       | `grpc`             | Ark operator transport: `grpc` or `rest`. |
| `--swap.serveraddress`     | network default    | Swap server address override.            |
| `--rpc.listenaddr`         | `localhost:10029`  | Daemon gRPC listen address.              |

---

## Architecture

```
waved (orchestrator)
├── round       Ark round participation FSM
├── vtxo        VTXO lifecycle FSM
├── oor         Out-of-round transfer coordination
├── wallet      Boarding wallet actor
├── ledger      Double-entry fee accounting
├── unroll      Per-target unilateral-exit actor
├── txconfirm   Broadcast + CPFP + confirmation actor
├── serverconn  Durable mailbox transport to the operator
├── chainsource Pluggable chain backend (lnd | lwwallet | btcwbackend)
└── db          SQLite or PostgreSQL persistence
```

The full system map, dependency graph, key types, and FSM diagrams live in
[`ARCHITECTURE.md`](ARCHITECTURE.md). Each major package additionally has its
own `CLAUDE.md` / `AGENTS.md` with package-local context.

---

## Documentation

| Topic                            | Document                                                              |
|----------------------------------|-----------------------------------------------------------------------|
| System architecture              | [`ARCHITECTURE.md`](ARCHITECTURE.md)                                  |
| Install, configure, operate      | [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md)                |
| Public test-network endpoints    | [`docs/signet.md`](docs/signet.md)                                    |
| Build-tag matrix                 | [`docs/wavewalletrpc_build.md`](docs/wavewalletrpc_build.md)                  |
| Durable actor pattern            | [`docs/durable_actor_architecture.md`](docs/durable_actor_architecture.md) |
| Mailbox transport                | [`docs/mailbox_architecture.md`](docs/mailbox_architecture.md)        |
| SDK layering                     | [`docs/sdk_layered_architecture.md`](docs/sdk_layered_architecture.md)|
| Wallet SDK (`sdk/wavewalletdk`)      | [`docs/wavewalletdk_integration.md`](docs/wavewalletdk_integration.md)        |
| Fee ledger                       | [`docs/fee_ledger.md`](docs/fee_ledger.md)                            |
| arkscript spec                   | [`docs/arkscript_spec.md`](docs/arkscript_spec.md)                    |
| Full doc index                   | [`docs/index.md`](docs/index.md)                                      |

---

## Development

Prerequisites and build setup: [`INSTALL.md`](INSTALL.md).

Common workflows:

```bash
make fmt-changed              # format changed Go files (goimports + llformat)
make lint-changed-local       # fast local linter against origin/main
make unit                     # run all unit tests
make unit pkg=round case=TestFoo
make systest                  # system integration tests (sqlite)
make systest db=postgres      # system integration tests (postgres)
make rpc                      # regenerate protobuf stubs
make sqlc                     # regenerate type-safe DB queries
make help                     # list every make target
```

Style guide, commit format, generated-code rules, and the pre-commit
checklist are documented in:

- [`docs/development_guidelines.md`](docs/development_guidelines.md)
- [`docs/testing-guide.md`](docs/testing-guide.md)
- [`docs/commit-tooling.md`](docs/commit-tooling.md)
- [`CLAUDE.md`](CLAUDE.md): quick map for agent-assisted development.

---

## Project Status

`wavelength` is under active development. Mainnet operation requires the
explicit `--allow-mainnet` flag; the default network is treated as a safety
guard. RPC surfaces, on-disk schema, and CLI commands may still change
across minor versions.

## License

A `LICENSE` file has not yet been published in this repository. Until one is
added, treat the code as all-rights-reserved and contact the maintainers
before redistributing.
