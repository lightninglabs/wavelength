#!/bin/sh
#
# docker-bitcoind-init.sh
#
# Creates a default wallet in bitcoind and mines a single block so that
# lnd nodes can complete their initial chain sync. Bitcoin Core 30+
# no longer auto-creates a wallet on startup.

set -eu

BTC="bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass -rpcconnect=bitcoind"

# Create or load the default wallet.
$BTC createwallet "default" 2>/dev/null || $BTC loadwallet "default" 2>/dev/null || true

# Mine a single block to kick off lnd chain sync.
$BTC -generate 1
