# Signet Endpoints

`darepod` connects to the public staging Ark and swap services when the
configured Bitcoin network is `signet`. The default outbound transport is
gRPC, so a signet config with no server overrides resolves to:

| Service | Transport | Endpoint |
|---------|-----------|----------|
| Ark operator | gRPC | `arkd-signet.staging.lightningcluster.com:443` |
| Swap server | gRPC | `swapd-signet.staging.lightningcluster.com:443` |

Both endpoints use publicly trusted TLS certificates. Leave
`server.insecure` and `swap.serverinsecure` disabled, and leave the custom TLS
certificate paths empty so the clients use the system certificate pool.

The wallet backend still needs its own signet chain source. As an example, an
`lwwallet` deployment can start with an operator-provided signet Esplora URL:

```bash
darepod \
  --network=signet \
  --wallet.type=lwwallet \
  --wallet.esploraurl=https://your-signet-esplora.example/api
```

The swap endpoint is consumed only by builds that include `swapruntime`.

## REST Transport

Set both transport selectors to `rest` when the host cannot use native gRPC:

```bash
darepod \
  --network=signet \
  --server.transport=rest \
  --swap.servertransport=rest \
  --wallet.type=lwwallet \
  --wallet.esploraurl=https://your-signet-esplora.example/api
```

The resolved HTTPS gateways are:

| Service | Endpoint |
|---------|----------|
| Ark operator | `https://arkd-signet-rest.staging.lightningcluster.com` |
| Swap server | `https://swapd-signet-rest.staging.lightningcluster.com` |

The same Ark gateway carries both `ArkService` and `MailboxService`. The swap
gateway carries the swap RPC surface used by the daemon-owned swap runtime.

## Custom Deployments

Custom endpoint values take precedence over the public signet defaults:

```bash
darepod \
  --network=signet \
  --server.host=ark.example.com:443 \
  --swap.serveraddress=swap.example.com:443 \
  --wallet.type=lwwallet \
  --wallet.esploraurl=https://your-signet-esplora.example/api
```

For a local signet development stack, keep the local endpoint defaults and
enable the two insecure flags:

```bash
darepod \
  --network=signet \
  --server.insecure \
  --swap.serverinsecure \
  --wallet.type=lwwallet \
  --wallet.esploraurl=http://localhost:3000
```

The insecure flags are an explicit local-development signal, so the daemon
keeps `localhost:10010` and `localhost:10030` instead of replacing them with
the public staging endpoints.

## walletdk

The embedded Go SDK uses the same network-aware defaults. Set the network and
wallet chain source, but leave the Ark and swap address fields untouched:

```go
cfg := walletdk.DefaultConfig()
cfg.Network = "signet"
cfg.WalletType = "lwwallet"
cfg.WalletEsploraURL = "https://your-signet-esplora.example/api"

client, err := walletdk.Start(ctx, cfg)
```

`walletdk.Start` resolves the gRPC staging endpoints while it validates the
embedded daemon config. Set `ServerTransport` and `SwapServerTransport` to
`walletdk.TransportREST` to select the two HTTPS gateways instead.
