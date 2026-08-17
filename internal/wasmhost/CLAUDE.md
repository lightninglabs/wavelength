# internal/wasmhost

## Purpose

Answers one question for the `js && wasm` build: is this binary hosted by a
browser page or by a Node process? The wavewalletdk wasm blob targets both,
and nothing about the daemon differs between them except storage — a browser
has no filesystem and persists everything in OPFS, while Node has a real one
reached through Go's `os` package and through the go-wasmsqlite `nodefs` VFS.
Several packages need that answer, so it is derived in exactly one place.

The whole package is behind `//go:build js && wasm`; it contributes nothing to
native builds.

## Key Types

- `UnderNode() bool` — reports whether the host is Node rather than a browser
  page.
- `SQLiteVFS() string` — names the go-wasmsqlite VFS that gives this host
  durable storage: `"nodefs"` under Node, `"opfs"` otherwise.

## Relationships

- **Depends on**: `syscall/js` only. Deliberately dependency-free so any
  layer can import it without creating a cycle.
- **Depended on by**: `db` (`sqlite_open_wasm.go` — VFS name and whether to
  mangle the database filename), `lwwallet` (`walletdb_wasm.go` — same, for
  btcwallet's SQL walletdb), `cmd/wavewalletdk-wasm` (`main.go` — refuses the
  browser `data_dir` default under Node).
- **Sends** / **Receives**: nothing; this is a pure predicate package with no
  actors or messages.

## Invariants

- **Both signals are required.** `UnderNode` checks for a `process.versions.node`
  string *and* the absence of a `Worker` constructor. Neither is sufficient
  alone: a bundler may define a `process` shim in a browser, and a non-browser
  host other than Node would also lack `Worker`. Weakening this to one signal
  lets a browser be mistaken for a filesystem host, which sends the daemon
  looking for storage that is not there.
- **Both VFS names are durable.** `SQLiteVFS` never returns a memory VFS, so
  an open that asks for one and cannot have it fails rather than quietly
  running against memory. Callers pair it with `require_persistent=true` on
  the DSN for exactly that reason — the daemon's databases are the only copy
  of wallet, VTXO, swap, and round state.
- The two hosts diverge on filename handling, not just VFS: a browser hashes
  the configured path into an origin-local OPFS name, while Node keeps the
  path as given. Callers own that branch; this package only reports which host
  they are on.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
