# Seed Restore Recovery

This guide covers restoring a self-managed `waved` wallet from its mnemonic
and asking the daemon to recover Ark state. It applies to embedded wallet
backends such as `lwwallet` and `btcwallet`.

## What Recovery Does

`wavecli create --recover` imports an existing mnemonic into a fresh daemon
data directory. With Ark-state recovery enabled, the daemon scans deterministic
keys and reconstructs local wallet state from the chain, the backing wallet,
and the operator indexer.

The scan covers three key families:

- Boarding keys: rebuilds boarding scripts, imports them into the backing
  wallet, persists missing boarding addresses, and lets wallet balance
  discovery find confirmed boarding UTXOs.
- VTXO owner keys: rebuilds standard VTXO owner scripts and queries indexed
  VTXOs from the operator indexer.
- Out-of-round receive keys: rebuilds receive scripts, matches owned scripts
  registered with the indexer, and fetches recipient events for those scripts.

The recovery response reports how much state was materialized:

- `recovery_ran`
- `recovered_boarding_addresses`
- `recovered_boarding_utxos`
- `recovered_vtxos`
- `recovered_oor_receive_scripts`
- `recovered_oor_events`

Recovery is idempotent. Re-running it against already persisted scripts or
VTXOs should not fail because the state already exists.

## Before You Restore

Stop the original daemon before restoring the same mnemonic into another
daemon. The seed controls the wallet identity used for mailbox and indexer
requests. Running two daemons with the same seed at the same time can make
responses arrive at the wrong process.

Use a fresh daemon data directory for the restored wallet. The restore password
protects the local seed file in that new directory; it does not have to match
the password used by the original wallet. The optional aezeed seed passphrase,
if one was used when the wallet was created, must match.

Pick a recovery window large enough to include every key index you care about.
`--recovery-window=N` scans indexes `0..N-1` in each recovery family. A value of
`0` uses the daemon's configured `wallet.recoverywindow`.

## CLI Restore

Create a wallet and record the mnemonic:

```sh
export WAVED_WALLET_PASSWORD='replace-with-a-local-wallet-password'

wavecli create --print-mnemonic-json > create.json
jq -r '.mnemonic[]' create.json > wallet.mnemonic
chmod 600 wallet.mnemonic
```

Generate receive surfaces as usual:

```sh
wavecli recv --onchain
wavecli recv --offchain --amt 10000 --memo 'restore smoke'
```

After stopping the original daemon, start a fresh daemon with an empty data
directory and restore the seed:

```sh
export WAVED_WALLET_PASSWORD='replace-with-a-new-local-wallet-password'

wavecli create --recover \
  --mnemonic-file wallet.mnemonic \
  --recovery-window 100
```

Inspect the response counters. Then check the public wallet surface:

```sh
wavecli balance
wavecli vtxos list
```

For unboarded boarding funds, top-level `wavecli balance` reports the amount
under `pending_in_sat`. Once funds are boarded into VTXOs, they contribute to
`confirmed_sat`.

## Local arktest Smoke

The root `lumos` repository contains the `arktest` manual harness. Use it when
this client repo is checked out as the root repo's `client/` submodule.

Build the manual harness and CLI binaries from the root repo:

```sh
make arktest
```

Start an embedded-wallet topology where `wavecli create` initializes the
wallet:

```sh
./arktest --datadir /tmp/arktest-restore start \
  --client-wallet=lwwallet \
  --manual-wallet \
  --client alice
```

In another terminal, load the generated aliases and create the source wallet:

```sh
eval "$(./arktest --datadir /tmp/arktest-restore aliases)"
export WAVED_WALLET_PASSWORD=itest-wallet-password

alice-cli create --print-mnemonic-json > alice-create.json
jq -r '.mnemonic[]' alice-create.json > alice.mnemonic

addr="$(alice-cli recv --onchain | jq -r '.onchain_address')"
./arktest --datadir /tmp/arktest-restore faucet "$addr" 100000
alice-cli balance
```

Do not restore `alice.mnemonic` into another live client in this same `arktest`
process. `arktest start` runs client daemons in-process and has no shell command
to stop only one client while keeping the chain, operator, and indexer alive.
`arktest stop` stops the whole topology, which is fine for cleanup but does not
leave the same regtest chain/indexer available for a manual restore.

Use the root integration test for the full local CLI restore path. It keeps one
operator/indexer topology alive, stops the source daemon in-process, restores the
same mnemonic into fresh daemon data directories, and asserts both a too-small
and a sufficient recovery window:

```sh
ARK_ITEST_CLIENT_WALLET=lwwallet \
go test -tags='itest wavewalletrpc swapruntime dev nolog' -v ./itest \
  -run '^TestBoardingIntegrationRecoveryCLIFromSeedRestoresBoardingFunds$' \
  -count=1 -timeout=60m
```

The test must be built with the same daemon-side `wavewalletrpc` tag that the
top-level `wavecli create`, `recv`, and `balance` commands require. Without
that tag, the CLI can connect to the daemon but the wallet service returns:

```sh
daemon was not built with -tags wavewalletrpc
```

## Troubleshooting

`--recover requires --mnemonic-file` means the CLI rejected a restore command
before it contacted the daemon. Supply a whitespace-separated 24-word mnemonic
file.

If recovery succeeds but funds are missing, first raise `--recovery-window`.
For example, a window of `1` only scans index `0`.

If restore hangs or mailbox/indexer responses look inconsistent, make sure no
other daemon is running with the same seed.

If `wavecli balance` shows zero after recovering unboarded boarding funds,
check whether the funding transaction has enough confirmations for the
operator's configured minimum confirmation count.
