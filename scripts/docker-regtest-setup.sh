#!/usr/bin/env bash
#
# docker-regtest-setup.sh
#
# Initializes the regtest environment after docker-compose up. Mines initial
# blocks for coinbase maturity, funds both lnd wallets (server and client),
# and verifies connectivity.
#
# Usage:
#   ./scripts/docker-regtest-setup.sh
#
# Prerequisites:
#   docker-compose up -d  (all services healthy)

set -euo pipefail

# Bitcoin Core RPC helper.
btc() {
    docker exec ark-bitcoind bitcoin-cli -regtest \
        -rpcuser=devuser -rpcpassword=devpass "$@"
}

# LND CLI helpers.
lnd_server() {
    docker exec ark-lnd-server lncli --network=regtest "$@"
}

lnd_client() {
    docker exec ark-lnd-client lncli --network=regtest \
        --rpcserver=localhost:10010 "$@"
}

echo "=== Darepo Regtest Setup ==="
echo ""

# Wait for services to be ready.
echo "Waiting for bitcoind..."
until btc getblockchaininfo > /dev/null 2>&1; do
    sleep 1
done
echo "  bitcoind ready."

echo "Waiting for lnd-server..."
until lnd_server getinfo > /dev/null 2>&1; do
    sleep 1
done
echo "  lnd-server ready."

echo "Waiting for lnd-client..."
until lnd_client getinfo > /dev/null 2>&1; do
    sleep 1
done
echo "  lnd-client ready."

# Ensure the default wallet exists (Bitcoin Core 30+ no longer
# auto-creates one).
echo ""
echo "Ensuring default wallet..."
btc createwallet "default" > /dev/null 2>&1 || \
    btc loadwallet "default" > /dev/null 2>&1 || true
echo "  Wallet ready."

# Mine initial blocks for coinbase maturity (100 + 6 buffer).
BLOCK_COUNT=$(btc getblockcount)
if [ "$BLOCK_COUNT" -lt 106 ]; then
    NEEDED=$((106 - BLOCK_COUNT))
    echo ""
    echo "Mining $NEEDED blocks for coinbase maturity..."
    btc -generate "$NEEDED" > /dev/null
    echo "  Mined. Block height: $(btc getblockcount)"
fi

# Fund lnd-server wallet.
echo ""
echo "Funding lnd-server..."
SERVER_ADDR=$(lnd_server newaddress p2tr | jq -r '.address')
echo "  Server address: $SERVER_ADDR"
btc sendtoaddress "$SERVER_ADDR" 10
btc -generate 6 > /dev/null
echo "  Sent 10 BTC, mined 6 blocks."

# Fund lnd-client wallet.
echo ""
echo "Funding lnd-client..."
CLIENT_ADDR=$(lnd_client newaddress p2tr | jq -r '.address')
echo "  Client address: $CLIENT_ADDR"
btc sendtoaddress "$CLIENT_ADDR" 10
btc -generate 6 > /dev/null
echo "  Sent 10 BTC, mined 6 blocks."

# Wait for wallets to sync.
echo ""
echo "Waiting for wallet sync..."
sleep 3

# Print summary.
echo ""
echo "=== Setup Complete ==="
echo ""
echo "Block height:    $(btc getblockcount)"
echo ""

SERVER_BAL=$(lnd_server walletbalance | jq -r '.confirmed_balance')
CLIENT_BAL=$(lnd_client walletbalance | jq -r '.confirmed_balance')
echo "lnd-server balance: $SERVER_BAL sat"
echo "lnd-client balance: $CLIENT_BAL sat"

echo ""
echo "Services:"
echo "  bitcoind RPC:     localhost:18443"
echo "  lnd-server gRPC:  localhost:10009"
echo "  lnd-client gRPC:  localhost:10010"
echo "  arkd client RPC:  localhost:7070"
echo "  arkd admin RPC:   localhost:8081"
echo "  darepod RPC:      localhost:10029"
echo ""
echo "CLI access:"
echo "  docker exec ark-server arkcli --rpcserver=localhost:8081 <command>"
echo "  docker exec ark-client darepocli --rpcserver=localhost:10029 <command>"
