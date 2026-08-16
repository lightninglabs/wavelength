# internal/wasmhost

## Purpose

Answers one question for the `js && wasm` build: is this binary hosted by a
browser page or by a Node process? The wavewalletdk wasm blob targets both,
and they differ in exactly one respect that matters — storage. A browser has
no filesystem and persists everything in OPFS; Node has a real filesystem,
reachable both through Go's `os` package (once the host assigns
`globalThis.fs` from `node:fs`) and through the go-wasmsqlite `nodefs` VFS.
Several packages need that answer, so it is derived in exactly one place.

## Key Types

- `UnderNode() bool` — Reports whether the host is Node rather than a browser
  page. Checks two signals together: a `process.versions.node` string **and**
  the absence of a `Worker` constructor.
- `SQLiteVFS() string` — Names the go-wasmsqlite VFS that gives this host
  durable storage: `"nodefs"` under Node, `"opfs"` otherwise.

## Relationships

- **Depends on**: `syscall/js` only; no in-repo imports.
- **Depended on by**: `db` (`sqlite_open_wasm.go` — VFS selection and whether
  the configured path is used verbatim or hashed to an OPFS name), `lwwallet`
  (`walletdb_wasm.go` — the same choice for btcwallet's SQL walletdb),
  `cmd/wavewalletdk-wasm` (`main.go` — refuses the browser `data_dir` default
  under Node).
- **Sends** / **Receives**: none; this is a pure predicate package with no
  actor surface.

## Invariants

- **The whole package is `js && wasm`-only.** Every file carries the
  `//go:build js && wasm` constraint, so nothing here compiles into a native
  daemon build and no native caller can depend on it.
- **Both signals are required to conclude "Node".** Neither is sufficient
  alone: a bundler may define a `process` shim inside a browser, so a node
  version string is not proof on its own, and a non-browser host other than
  Node would also lack a `Worker` constructor. Requiring both keeps a browser
  that fakes one of them from being mistaken for a filesystem host — which
  would send the daemon looking for storage that is not there.
- **Both VFS names are durable.** An open that asks for one and cannot have it
  must fail rather than quietly fall back to memory; callers pair
  `SQLiteVFS()` with `require_persistent=true` in the DSN for exactly that
  reason. The daemon's databases are the only copy of wallet, VTXO, swap, and
  round state.
- Host detection is a read of the JS global object, not a cached value.
  Callers that need it on a hot path should hoist it themselves.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
