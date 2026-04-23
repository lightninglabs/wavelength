# bitcoindrpc

## Purpose

Direct-to-bitcoind JSON-RPC implementation of
`chainbackends.PackageSubmitter`. Used by the production daemon (and by
integration tests) when v3 / TRUC CPFP package relay must go straight to a
bitcoind node instead of through LND's `WalletKit`. Sibling to `lnd.go` in
`chainbackends/`: one concrete backend per submitter source.

## Key Types

- `PackageSubmitter` — HTTP client that authenticates via basic auth and
  POSTs a `submitpackage` JSON-RPC call to bitcoind. Uses a dedicated
  `*http.Client` with a 30s backstop timeout so a wedged node can't stall
  the caller for the full parent context.
- `New(host, user, password)` — Constructs a `PackageSubmitter`. `host` is
  the `host:port` form; the submitter prefixes `http://` because bitcoind's
  JSON-RPC server speaks plain HTTP by default. TLS termination (when
  present) is expected to be handled by an external reverse proxy.

## Relationships

- **Depends on**: `btcd/btcjson` (SubmitPackageResult), `btcd/wire` (MsgTx),
  standard library `net/http`.
- **Depended on by**: `cmd/darepod` (wires via `bitcoind.{host,user,pass}`
  flags), `harness` (itest injection into `darepod.Config.PackageSubmitter`).

## Invariants

- `SubmitPackage` serializes parents first, child last, and calls
  `submitpackage` with `maxfeerate=0` so the CPFP child's intentionally
  high feerate is not rejected by bitcoind's default 0.10 BTC/kvB cap.
- bitcoind returns HTTP 500 with a JSON-RPC error envelope for method-level
  failures; the code first tries to decode the body and only falls back to
  an HTTP-level error when the body is unparseable (e.g. HTML error pages
  from proxies or 401/403). This keeps protocol-level error codes visible
  to the caller rather than collapsing to HTTP status.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
