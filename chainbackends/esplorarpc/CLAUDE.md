# esplorarpc

## Purpose

Esplora-backed implementation of `chainbackends.PackageSubmitter`. POSTs a
v3/TRUC parent+child package to an Esplora/electrs `/txs/package` endpoint so
chain backends that cannot relay packages themselves — lnd's `WalletKit` and
the neutrino SPV backend — can still broadcast the zero-fee ephemeral-anchor
parents that unilateral exit / fraud response produce (darepo-client#590).

Sibling to `chainbackends/bitcoindrpc`: one concrete `PackageSubmitter` per
relay source. The lwwallet backend does **not** use this — it relays packages
through its own Esplora chain backend (`lwwallet.EsploraClient.SubmitPackage`).

## Key Types

- `PackageSubmitter` — HTTP client that POSTs a JSON array of raw tx hex
  (parents-first, child-last) to `<baseURL>/txs/package`. Dedicated
  `*http.Client` with a 30s backstop timeout. Constructed via
  `New(baseURL, ...Option)`.
- `New(baseURL, opts...)` — normalizes `baseURL` (bare host → `https://`,
  trailing slash trimmed, scheme validated). `Option`s: `WithHTTPClient`
  (tests / custom TLS / proxy), `WithLog` (debug-logs the raw endpoint
  response — the only place an Esplora-side reject reason is visible).

## Relationships

- **Depends on**: `btcd/btcjson` (`SubmitPackageResult`), `btcd/wire`
  (`MsgTx`), `btclog/v2` + `lnd/fn/v2` (optional logger), standard library
  `net/http`. Deliberately does **not** import `chainbackends` (mirrors
  `bitcoindrpc`): the interface is satisfied structurally, asserted only in
  the test.
- **Depended on by**: `cmd/darepod` (wires via `package.esploraurl`, falling
  back to `wallet.esploraurl`, when `bitcoind.host` is unset).

## Invariants

- Returns the parsed `*btcjson.SubmitPackageResult` even when it reports
  per-tx rejections; the caller's backend classifies `PackageMsg` /
  `TxResults` (same contract as `bitcoindrpc`). A Go error is returned only
  for transport failures or an unparseable non-2xx body (e.g. an HTML proxy
  error page).
- Esplora returns bitcoind's **bare** submitpackage result object (no
  JSON-RPC `{result, error}` envelope), unmarshaled directly into
  `btcjson.SubmitPackageResult` (whose `UnmarshalJSON` reads `package_msg` /
  `tx-results`).
- The `maxFeeRate` argument is accepted for interface compatibility but
  **ignored**: `/txs/package` exposes no per-call maxfeerate override, so the
  server default governs (matches the lwwallet path). bitcoind-direct callers
  pass `maxfeerate=0` to avoid the default 0.10 BTC/kvB cap rejecting a CPFP
  child's high standalone feerate; if a server enforces that cap this endpoint
  would need a query-param escape hatch.
- Trust: routing a package through a third-party Esplora reveals it to that
  server — a privacy regression for an otherwise-trustless SPV wallet, hence
  opt-in.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
