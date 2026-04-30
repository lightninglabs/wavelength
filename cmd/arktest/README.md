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

Once the topology is usable, `start` prints a sparse ready banner instead of
requiring you to scan daemon logs:

```text
[18:43:52.410] arktest starting clients=[alice bob] wallet=lnd artifacts=/tmp/arktest-artifacts
[18:43:52.411] starting bitcoind, electrs, operator lnd, and arkd
[18:44:02.101] operator arkd ready admin=127.0.0.1:52444 rpc=127.0.0.1:52445
[18:44:02.101] funding operator lnd amount=2000000000
[18:44:03.532] operator lnd funded amount=2000000000
[18:44:03.533] starting client alice wallet=lnd
[18:44:05.901] client alice ready rpc=127.0.0.1:52451
[18:44:05.902] funding client alice lnd wallet amount=1000000000
[18:44:07.283] client alice lnd wallet funded amount=1000000000
[18:44:07.284] starting client bob wallet=lnd
[18:44:09.672] client bob ready rpc=127.0.0.1:52462
[18:44:09.673] funding client bob lnd wallet amount=1000000000
[18:44:11.022] client bob lnd wallet funded amount=1000000000
[18:44:11.024] arktest ready
[18:44:11.024] state: /Users/me/.arktest/current.json
[18:44:11.024] artifacts: /path/to/arktest-artifacts/arktest/20260429184402
[18:44:11.024] operator admin rpc: 127.0.0.1:52444
[18:44:11.024] operator client rpc: 127.0.0.1:52445
[18:44:11.024] clients: [alice bob]
```

That sparse stream is meant to be actionable while the topology is still
booting: it names the component being started, the wallet being funded, and the
RPC endpoint that became ready. The detailed daemon logs are still preserved in
the artifact directory and can be tailed later with `arktest logs`.

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
./arktest logs [component]               # list or tail component logs
```

`board` is opt-in: you call it only for the clients you actually plan to
board. The default amount is 100,000,000 sat (1 BTC).

`logs` resolves component names from the state file and tails their artifact
logs:

```sh
./arktest logs                 # list known log targets and paths
./arktest logs operator        # arkd.log
./arktest logs bitcoind        # bitcoind debug.log
./arktest logs alice           # alice darepod.log
./arktest logs alice-lnd       # alice's lnd.log
./arktest logs client05 -f     # follow client05 darepod.log
```

`events` is also a log target. It points at the sparse JSON-lines event stream
written by `arktest start` and `arktest stress`.

## Stress

`arktest stress` is a sparse-log monkey runner. It starts one topology, creates
zero-padded clients (`client01`, `client02`, ...), boards every client into an
initial round, and then randomly performs:

- OOR payments between clients
- refresh rounds for random clients
- graceful client daemon restarts
- client crash/recover events
- graceful operator restarts, followed by client daemon reconnects

`--concurrency` is a global worker limit, not a per-client limit. Normal client
RPC work is intentionally allowed to overlap on the same daemon so the stress
runner can exercise concurrent payments, balance/list calls, receive-script
creation, and refresh requests the way a real client process can see them.
Lifecycle events still serialize the harness-side daemon handle replacement
needed for restart/crash/recover bookkeeping.

The stress startup path uses the same sparse event style as `start`, but it is
more explicit about bootstrap progress. A healthy run shows the operator being
funded, every stress client starting, every client wallet being funded, each
boarding address being funded, clients submitting board requests, clients
sending round registrations, the bootstrap batch trigger, and the final
confirmed bootstrap round.

By default, each stress client boards into one VTXO. Use
`--board-vtxos-per-client=N` to fan each client's `--board-amount` into N
boarded VTXOs during bootstrap. This creates N independent live VTXO outpoints
per client, giving high-concurrency payment runs more real spend lanes without
serializing the workload. The split is done through the daemon's board flow, so
the stress run still exercises the normal multi-output boarding path.

Example:

```sh
./arktest stress \
  --clients 10 \
  --max-payments 200 \
  --max-rounds 20 \
  --max-restarts 10 \
  --duration 15m \
  --seed 42
```

The terminal output stays sparse:

```text
[18:44:02.101] arktest stress starting
[18:44:02.102] funding operator lnd amount=2000000000
[18:44:03.461] operator lnd funded amount=2000000000
[18:44:03.462] starting client client01 wallet=lnd
[18:44:05.812] client client01 ready rpc=127.0.0.1:52451
[18:44:05.813] funding client client01 lnd wallet amount=1000000000
[18:44:07.164] client client01 lnd wallet funded amount=1000000000
[18:44:14.008] client client01 boarding address funded
[18:44:20.431] client client01 board intent ready
[18:44:20.432] client client01 triggered round registration
[18:44:21.998] bootstrap batch triggered round=019ddd98-...
[18:44:42.615] bootstrap round confirmed round=019ddd98-...
[18:44:42.616] arktest stress ready clients=10 artifacts=/tmp/... seed=42
[18:44:43.008] payment 1 client03 -> client08 amount=12000
[18:44:43.433] payment 1 settled latency=425ms session=...
[18:44:44.882] client restarting client=client05
[18:44:46.104] client ready client=client05 latency=1.222s
[18:44:47.191] client crashing client=client02
[18:44:48.490] client recovered client=client02 latency=1.299s
[18:44:49.500] operator restarting
[18:44:54.220] operator ready latency=4.72s rpc=127.0.0.1:52445
```

Daemon logs stay in the artifact directory and can be inspected with
`arktest logs <component>`. The stress runner also writes:

- `events.jsonl` — timestamped sparse events with structured fields
- `summary.json` — seed, duration, result layers, failure classes, payment
  counts, round counts, restarts, recovery checks, and artifact path

Payment errors are recorded in the event log and summary instead of failing
the first random operation. Bootstrap and readiness failures still abort the
run because they mean the test topology itself did not become usable.

When a payment is skipped because no sender has enough live spendable balance,
the terminal block includes the sender scan totals: clients checked, RPC
failures, clients below `--min-payment`, candidates, maximum live balance,
total live balance, runner-reserved VTXO balance, available balance, the
minimum payment target, and a capped per-client scan:

```text
[18:45:02.001] payment 24 skipped: no funded sender
	checked=5 rpc_failed=1 below_min=4 candidates=0
	max_live=812 total_live=1500 reserved=688 max_available=812 total_available=812 min_payment=1000
	scan:
		client03 status=rpc_failed class=connection_closing expected=true
		client04 status=below_min live=812 reserved=0 available=812 vtxos=1
```

The matching
`payment_skip` record in `events.jsonl` also includes a per-client breakdown
with each client's scan status, live VTXO count, live balance, runner-reserved
VTXO balance, available balance, failure class, expected flag, and RPC error
when one was observed.

High-concurrency runs deliberately create ordinary stress failures. For example,
many clients start with one large boarded VTXO, so one in-flight payment can
make that whole VTXO unavailable until the daemon settles the spend/change
state. The stress runner mirrors this by reserving whole VTXO outpoints before
starting a payment RPC, which avoids queuing additional runner-created payments
against the same VTXO. Restarts can still close RPC connections while payments
are in flight, refreshes can temporarily move VTXOs out of the live set, and a
random amount can leave a below-dust OOR change output. Those are recorded as
payment failures and kept in the sparse timeline. A `PASS` process exit means
the runner completed and wrote its artifacts; it does not mean every random
workload operation succeeded.

When you want payment concurrency pressure without immediately exhausting each
client's single VTXO lane, fan out bootstrap boarding:

```sh
./arktest stress \
  --clients 10 \
  --concurrency 20 \
  --max-payments 100 \
  --max-rounds 0 \
  --max-restarts 0 \
  --client-restarts=false \
  --operator-restarts=false \
  --client-crashes=false \
  --board-amount 1250000 \
  --board-vtxos-per-client 10 \
  --duration 5m \
  --seed 424242
```

This does not make balances infinite: each VTXO is still reserved as a whole
outpoint while a payment is in flight. It changes bootstrap from one large
spend lane per client to N spend lanes per client, which better matches agents
or wallets that issue bursts of concurrent payments.

The summary separates runner health from workload outcomes:

- `HARNESS` reports whether the stress harness itself completed. If bootstrap
  fails or the process panics, the run does not reach the summary.
- `WORKLOAD` reports whether random operations succeeded, failed only in ways
  expected for the chosen stress shape, or hit unexpected failures.
- `INVARIANTS` reports whether unexpected failures or recovery failures were
  observed.
- `RECOVERY` reports whether the final quiet probe could still query the
  operator and every client after all workload workers drained.

Refresh failures are also workload outcomes. The summary distinguishes
`rounds_confirmed` from `rounds_failed`, and the detailed daemon logs in the run
directory are the place to inspect why a refresh round did not reach broadcast
or confirmation.

At the end, the terminal prints a high-signal banner so failures stand out
without opening the JSON artifact:

```text
========== ARKTEST STRESS SUMMARY ==========
HARNESS=PASS WORKLOAD=EXPECTED_FAILURES INVARIANTS=PASS RECOVERY=PASS
payments settled=197/200 failed=3 expected=3 unexpected=0 success=98.5%
failure classes: connection_closing=2 dust_change=1
payment latency avg=244ms p50=180ms p95=901ms max=1800ms
throughput 2.18 settled payments/sec duration=1m30s concurrency=6
rounds confirmed=19/20 failed=1 client_restarts=3 client_crashes=4 operator_restarts=3
artifacts:
  run_dir=/tmp/arktest-stress-artifacts/arktest/20260430184402
  events_jsonl=/tmp/.../events.jsonl
  summary_json=/tmp/.../summary.json
  harness_log=/tmp/.../harness.log
  operator_log=/tmp/.../arkd/arkd.log
  operator_lnd_log=/tmp/.../lnd.log
  bitcoind_log=/tmp/.../debug.log
client logs: run `arktest logs` to list component targets
============================================
```

`--seed` controls workload generation: event type, selected clients, and
amounts. The system remains timing-dependent because RPC scheduling, rounds,
mailbox delivery, and wallet backends are concurrent, so the seed should be
treated as a reproducible workload recipe rather than a bit-for-bit replay.

Useful smoke shapes:

```sh
# Payment-only concurrency smoke. This is good for quick sender-selection,
# idempotency, and VTXO reservation pressure.
./arktest stress \
  --clients 5 \
  --concurrency 20 \
  --max-payments 60 \
  --max-rounds 0 \
  --max-restarts 0 \
  --client-restarts=false \
  --operator-restarts=false \
  --client-crashes=false \
  --duration 5m \
  --seed 7575

# Disruption-heavy smoke. This keeps payments flowing while clients and the
# operator restart underneath in-flight RPCs.
./arktest stress \
  --clients 5 \
  --concurrency 20 \
  --max-payments 60 \
  --max-rounds 2 \
  --max-restarts 4 \
  --client-restarts=true \
  --operator-restarts=true \
  --client-crashes=true \
  --duration 8m \
  --seed 7575
```

Client crash/recover events simulate a mobile app or wallet process being
abruptly killed by dropping the public RPC connection and cancelling the
in-process `darepod` root context before relaunching against the same data
directory. Because the current harness runs client daemons in-process, this is
not an OS-level `SIGKILL`; it is the closest crash analogue available until
`arktest` grows an external-process daemon mode.

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
