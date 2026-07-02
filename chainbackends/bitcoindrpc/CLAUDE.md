# bitcoindrpc

## Purpose

Direct-to-bitcoind JSON-RPC implementation of
`chainbackends.PackageSubmitter`. Used by the production daemon when v3 /
TRUC CPFP package relay must go straight to a bitcoind node instead of
through LND's `WalletKit`. Sibling to `lnd.go` in `chainbackends/`: one
concrete backend per submitter source.

## Key Types

- `PackageSubmitter` — HTTP client that authenticates via basic auth and
  POSTs a `submitpackage` JSON-RPC call to bitcoind. Uses a dedicated
  `*http.Client` with a 30s backstop timeout so a wedged node can't stall
  the caller for the full parent context.
- `New(host, user, password)` — Constructs a `PackageSubmitter` using the
  legacy no-error form. Bare `host:port` input defaults to `http://`; full
  `http://`/`https://` URLs are accepted as-is. Prefer `NewWithOptions` when
  malformed URLs or TLS config errors need to surface to the caller.
- `NewWithOptions(host, user, password, opts...)` — Constructs a
  `PackageSubmitter`, returning parse/TLS errors instead of swallowing them.
  Bare `host:port` defaults to `https://` when `WithTLSCertPath` is set,
  `http://` otherwise.
- `Option` / `WithTLSCertPath(path)` — Configures the HTTPS submitter to
  trust a custom CA certificate at `path` (augments, not replaces, the
  system trust store), for operators fronting bitcoind's RPC server with a
  local TLS reverse proxy.

## Relationships

- **Depends on**: `btcd/btcjson` (SubmitPackageResult), `btcd/wire` (MsgTx),
  standard library `net/http`, `crypto/tls`, `crypto/x509` (custom CA
  support).
- **Depended on by**: `cmd/darepod` (constructs the submitter via
  `bitcoindrpc.NewWithOptions` from the `bitcoind.*` CLI flags and assigns
  it to `darepod.Config.PackageSubmitter`). No other package or itest
  harness currently references this package.

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
