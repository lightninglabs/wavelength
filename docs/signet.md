# Public Test Network Endpoints

`waved` has built-in Ark and swap service endpoints for testnet3, testnet4,
and signet. Leave `server.host` and `swap.serveraddress` empty to select the
endpoint for the configured Bitcoin network and outbound transport.

| Network config | Ark gRPC | Ark REST | Swap gRPC | Swap REST |
|----------------|----------|----------|-----------|-----------|
| `testnet` | `arkd.testnet.lightningcluster.com:443` | `https://arkd-rest.testnet.lightningcluster.com` | `swapd.testnet.lightningcluster.com:443` | `https://swapd-rest.testnet.lightningcluster.com` |
| `testnet4` | `arkd-testnet4.testnet.lightningcluster.com:443` | `https://arkd-testnet4-rest.testnet.lightningcluster.com` | `swapd-testnet4.testnet.lightningcluster.com:443` | `https://swapd-testnet4-rest.testnet.lightningcluster.com` |
| `signet` | `arkd-signet.staging.lightningcluster.com:443` | `https://arkd-signet-rest.staging.lightningcluster.com` | `swapd-signet.staging.lightningcluster.com:443` | `https://swapd-signet-rest.staging.lightningcluster.com` |

The daemon defaults both outbound transports to gRPC. Set the selectors to
`rest` when the host cannot use native gRPC:

```bash
waved \
  --network=signet \
  --server.transport=rest \
  --swap.servertransport=rest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=https://your-signet-esplora.example/api
```

All public endpoints use publicly trusted TLS certificates. Leave
`server.insecure` and `swap.serverinsecure` disabled, and leave the custom TLS
certificate paths empty so the clients use the system certificate pool. The
swap endpoint is consumed only by builds that include `swapruntime`.

The testnet4 REST gateways are live. The testnet4 gRPC hostnames are already
the deployment names, but their public NLBs remain disabled until the
certificate work in
[lightning-infra#3517](https://github.com/lightninglabs/lightning-infra/pull/3517)
lands.

## Config Resolution

`waved.DefaultConfig()` leaves the two explicit address fields empty. The
config accessors resolve the effective values without mutating those fields:

- `Config.ArkServerAddress()` selects from `network`, `server.transport`, and
  `server.host`.
- `Config.SwapServerAddress()` selects from `network`,
  `swap.servertransport`, and `swap.serveraddress`.

An explicit address always wins. This includes local test-network stacks:

```bash
waved \
  --network=signet \
  --server.host=localhost:10010 \
  --server.insecure \
  --swap.serveraddress=localhost:10030 \
  --swap.serverinsecure \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000
```

Mainnet, regtest, and simnet have no public service deployment in the default
table, so empty address fields resolve to `localhost:10010` and
`localhost:10030`.

## wavewalletdk

The embedded Go SDK uses the same config-level lookup. Set the network and
wallet chain source, but leave the Ark and swap address fields empty:

```go
cfg := wavewalletdk.DefaultConfig()
cfg.Network = "signet"
cfg.WalletType = "lwwallet"
cfg.WalletEsploraURL = "https://your-signet-esplora.example/api"

client, err := wavewalletdk.Start(ctx, cfg)
```

Set `ServerTransport` and `SwapServerTransport` to
`wavewalletdk.TransportREST` to select the two HTTPS gateways instead.
