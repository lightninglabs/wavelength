# Public Test Network Endpoints

`waved` has built-in Ark and swap service endpoints for testnet3, testnet4,
and signet. Leave `server.host` and `swap.serveraddress` empty to select the
endpoint for the configured Bitcoin network and outbound transport.

| Network config | Ark gRPC | Ark REST | Swap gRPC | Swap REST |
|----------------|----------|----------|-----------|-----------|
| `testnet` | `test.wavelength.lightning.finance:443` | `https://test.wavelength-rest.lightning.finance` | `test.swap.wavelength.lightning.finance:443` | `https://test.swapd-rest.lightning.finance` |
| `testnet4` | `lumosd-testnet4.testnet.lightningcluster.com:443` | `https://test4.wavelength-rest.lightning.finance` | `swapd-testnet4.testnet.lightningcluster.com:443` | `https://test4.swapd-rest.lightning.finance` |
| `signet` | `signet.wavelength.lightning.finance:443` | `https://signet.wavelength-rest.lightning.finance` | `signet.swap.wavelength.lightning.finance:443` | `https://signet.swapd-rest.lightning.finance` |

The daemon defaults both outbound transports to gRPC. Set the selectors to
`rest` when the host cannot use native gRPC:

```bash
waved \
  --network=signet \
  --server.transport=rest \
  --swap.servertransport=rest \
  --wallet.type=lwwallet
```

All public endpoints use publicly trusted TLS certificates. Leave
`server.insecure` and `swap.serverinsecure` disabled, and leave the custom TLS
certificate paths empty so the clients use the system certificate pool. The
swap endpoint is consumed only by builds that include `swapruntime`.

The testnet4 REST gateways are live behind their friendly-domain CNAMEs. The
testnet4 gRPC endpoint's public NLB remains disabled, so it is still reached
through its raw cluster hostname; a friendly-domain CNAME will follow once
the certificate work in
[lightning-infra#3517](https://github.com/lightninglabs/lightning-infra/pull/3517)
lands.

## Wallet Chain Data and Fee Estimation

`wallet.esploraurl` (lwwallet) and `wallet.feeurl` (btcwallet) also resolve
network defaults when left empty:

| Network config | lwwallet Esplora URL | btcwallet fee URL |
|----------------|-----------------------|--------------------|
| `mainnet` | `https://mempool.space/api` | `https://nodes.lightning.computer/fees/v1/btc-fee-estimates.json` |
| `testnet` | `https://mempool.space/testnet/api` | `https://nodes.lightning.computer/fees/v1/btctestnet-fee-estimates.json` |
| `testnet4` | `https://mempool.space/testnet4/api` | `https://nodes.lightning.computer/fees/v1/btctestnet-fee-estimates.json` |
| `signet` | `https://mempool.space/signet/api` | `https://nodes.lightning.computer/fees/v1/btctestnet-fee-estimates.json` |

`regtest` and `simnet` have no public default for either field; a local dev
stack must set `wallet.esploraurl` or `wallet.feeurl` explicitly, as in the
local test-network example below. An explicit value always overrides the
network default on every network, including mainnet.

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
