# arktest

`arktest` is an itest-only developer harness for manual Ark testing with the
real `arkd`, `arkcli`, `darepod`, and `darepocli` command surfaces.

It starts a local regtest topology with:

- `bitcoind`, `electrs`, and an in-process `arkd`
- the operator's backing `lnd`, funded for round commitment fees
- one `darepod` per `--client`, each with its own `lnd` container so unroll's
  V3 ephemeral-anchor CPFP child has taproot fee inputs

The harness is for local development only. It creates throwaway regtest
wallets, containers, and state.

## Topology

`bitcoind` is the chain. `electrs` indexes it. Every LND (operator + per
client) connects to `bitcoind` for chain data. Every `darepod` also connects
to `bitcoind` directly so it can call `submitpackage` for V3 ephemeral-anchor
package relay during unroll. `arkd` runs **in-process** inside the `arktest`
binary; it talks to the operator's LND for round-tx wallet operations, and to
each `darepod` over its mailbox RPC.

```text
                              +----------------------+
                              | bitcoind  (regtest)  |
                              +----------+-----------+
                                         | RPC + ZMQ
   +--------------+--------------+-------+------+--------------+--------------+
   |              |              |              |              |              |
   v              v              v              v              v              v
+--------+  +-----------+  +-----------+  +-----------+  +-----------+  +-----------+
|electrs |  | operator- |  | alice-lnd |  |  bob-lnd  |  | alice-ark |  |  bob-ark  |
|(esplora|  |   lnd     |  |           |  |           |  | (darepod) |  | (darepod) |
| HTTP)  |  +-----+-----+  +-----+-----+  +-----+-----+  +-----+-----+  +-----+-----+
+--------+        | wallet/      | wallet       | wallet       |              |
                  | round-tx     | (taproot     | (taproot     | submitpackage|
                  v              |  UTXOs       |  UTXOs       |  for CPFP    |
            +----------+         |  for CPFP)   |  for CPFP)   v              v
            |  arkd    |<--------+              |        +------------+ +-----------+
            | in-proc  |  mailbox / round       |        | alice-cli  | |  bob-cli  |
            | inside   |<-----------------------+------->|(darepocli) | |(darepocli)|
            | arktest  |  mailbox / round                +------------+ +-----------+
            +----+-----+
                 ^
                 | admin RPC
                 |
            +----+-----+
            |  arkcli  |
            +----------+
```

Where the `darepod` ↔ `bitcoind` arrow matters: it's the
`bitcoindrpc.PackageSubmitter` the harness wires up so each client can
submit V3 parent + CPFP child as a package — the operator's `submitpackage`
isn't enough on its own.

Default liquidity:

| Bucket | Default | Where it lands |
|---|---:|---|
| operator-lnd | `2,000,000,000` sats | Pays round commitment-tx fees |
| each client's LND | `1,000,000,000` sats | Taproot UTXOs for unroll CPFP children |

Boarding outputs are **not** pre-funded — call `arktest board <client>`
explicitly when you want one. They are taproot scripts that LND owns but
cannot single-sign; if such a UTXO ends up in a client that later unrolls,
`selectFeeInput` could pick it as the smallest candidate and CPFP child
finalization would fail.

## Build

From the repo root:

```sh
make arktest
```

This produces three binaries side-by-side at the repo root:

- `./arktest` — the harness binary (built with the `itest` tag)
- `./arkcli` — admin CLI for the in-process `arkd`
- `./darepocli` — client CLI used by the per-daemon shell aliases

`./arktest aliases` discovers `./arkcli` and `./darepocli` by looking next to
itself, so the three binaries must stay co-located.

## Start

In one terminal:

```sh
./arktest start
```

The command blocks until interrupted with `Ctrl+C`. It writes a state file at
`<datadir>/current.json` (default `~/.arktest/current.json`) so the other
subcommands can find the running topology.

Common flags (`./arktest start --help` for the full list):

- `--client name` — logical name for a client daemon. Pass multiple times to
  start more than one (default `alice` and `bob`).
- `--client-wallet` — client wallet backend (`lnd`, `lwwallet`, `btcwallet`).
  Defaults to `lnd`. **Unroll requires `lnd`** — see "Unroll" below.
- `--operator-funds` — sats for the operator LND wallet (default 20 BTC).
- `--client-lnd-funds` — sats sent to each client's LND wallet for CPFP fee
  inputs (default 10 BTC). Only applied with `--client-wallet=lnd`.
- `--datadir` — directory for the state file (default `~/.arktest`).
- `--artifacts-dir` — directory for harness logs and per-component data dirs.
- `--logstdout` — also tee the harness/operator logs to stdout.

## Shell helpers

In a second terminal, load the generated helpers:

```sh
eval "$(./arktest aliases)"
```

This exports endpoint env vars and defines per-client wrapper functions:

| Alias | What it does |
|---|---|
| `arkcli ...` | `arkcli` against the operator's admin RPC |
| `<name>-cli ...` | `darepocli` against client `<name>`'s daemon RPC |
| `<name>-lncli ...` | `lncli` against client `<name>`'s LND |

The wrappers fill in `--rpcserver`, `--no-tls`, `--tlscertpath`, etc. from the
state file, so you can drive Ark commands directly:

```sh
arkcli info
alice-cli wallet balance
alice-cli board
arkcli trigger-batch
./arktest mine 6
alice-cli vtxos list
```

## Other subcommands

```sh
./arktest board <client> [amount-sat]   # fund <client>'s boarding addr
./arktest mine [n]                       # mine n regtest blocks (default 1)
./arktest info                           # endpoints + block height
./arktest aliases                        # the eval-able helper block
```

`board` is opt-in: you call it only for the clients you actually plan to
board. The default amount is 100,000,000 sat (1 BTC).

## Walkthrough — boarding + unroll

```sh
# Terminal 1
./arktest start

# Terminal 2
eval "$(./arktest aliases)"

./arktest board alice
alice-cli board
arkcli trigger-batch
./arktest mine 6

VTXO=$(alice-cli vtxos list | jq -r '.vtxos[0].outpoint')
alice-cli unroll --outpoint "$VTXO"

# Boarded VTXO needs ~144 blocks of CSV before the sweep can fire.
./arktest mine 150
alice-cli unroll status --outpoint "$VTXO"
# -> { "status": "UNROLL_JOB_STATUS_COMPLETED", "sweep_txid": "..." }

alice-lncli walletbalance
# confirmed_balance increases by the sweep amount
```

## Walkthrough — OOR send + receiver unroll

```sh
# Terminal 1
./arktest start --client alice --client bob

# Terminal 2
eval "$(./arktest aliases)"

# Only alice needs a boarding output — bob will receive via OOR.
./arktest board alice
alice-cli board
arkcli trigger-batch
./arktest mine 6

# Bob produces a fresh OOR receive pubkey.
PUBKEY=$(bob-cli oor receive | jq -r '.pubkey_xonly_hex')

# Alice sends 0.5 BTC OOR. Both VTXO lists update immediately.
alice-cli send oor --pubkey "$PUBKEY" --amount 50000000

# Bob unrolls the received VTXO. OOR receiver chains are deeper than a
# boarded VTXO, so plan for ~200 blocks of CSV.
C2_VTXO=$(bob-cli vtxos list | jq -r '.vtxos[0].outpoint')
bob-cli unroll --outpoint "$C2_VTXO"
./arktest mine 200
bob-cli unroll status --outpoint "$C2_VTXO"
bob-lncli walletbalance
```

## Why the LND wallet backend is the default

The unroll path broadcasts V3 ephemeral-anchor transactions. The CPFP child
that pays fees needs a taproot fee input (Schnorr-signed) — `lwwallet` and
`btcwallet` provide p2wkh fee inputs whose ECDSA signatures are rejected
inside V3 transactions, so unroll fails at the first parent broadcast.

The LND backend hands out p2tr addresses by default, which works. Each
client gets its own LND container so identity_pubkeys (and therefore mailbox
IDs) stay distinct.

## Cleanup

`./arktest start` cleans up its own containers when it exits via Ctrl+C. If
a crash leaves containers behind:

```sh
docker ps -a --format '{{.Names}}' | grep -E 'bitcoind|electrs|lnd|postgres|alice|bob' | xargs -r docker rm -f
```
