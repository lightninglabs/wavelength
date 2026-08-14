# internal/wasmhost

## Purpose

Answers one question for the `js && wasm` build: is this binary hosted by a
browser page or by a Node process? The `wavewalletdk` wasm blob targets both,
and nothing about the daemon differs between them *except* storage — a browser
has no filesystem and persists everything in OPFS, while Node has a real one
reachable through Go's `os` package and through the `go-wasmsqlite` `nodefs`
VFS. Several packages need that answer, so it is derived in exactly one place.

## Key Types

- `UnderNode() bool` — reports whether the host is Node rather than a browser.
  Checks **two** signals together: a `process.versions.node` string *and* the
  absence of a `Worker` constructor. Neither is sufficient alone — a bundler
  may define a `process` shim in a browser, and a non-Node non-browser host
  would also lack `Worker`.
- `SQLiteVFS() string` — the `go-wasmsqlite` VFS name giving this host durable
  storage: `"nodefs"` under Node, `"opfs"` otherwise. Both names are durable,
  so an open that asks for one and cannot have it fails rather than quietly
  falling back to memory.

## Relationships

- **Depends on**: `syscall/js` only. No repo packages — deliberately leaf, so
  every storage-selecting package can import it without a cycle.
- **Depended on by**: `db` (`sqlite_open_wasm.go` — VFS name plus whether to
  mangle the database filename for OPFS), `lwwallet` (`walletdb_wasm.go` —
  same choice for btcwallet's SQL `walletdb`), `cmd/wavewalletdk-wasm`
  (`main.go` — refuses the browser `data_dir` default under Node).
- **Sends** / **Receives**: nothing. Pure host introspection, no actors.

## Invariants

- The whole file is behind `//go:build js && wasm`. There is no native
  fallback and none is wanted: a caller reachable from a non-wasm build has no
  business asking this question.
- `UnderNode` must keep requiring **both** signals. Loosening it so a browser
  faking one is read as a filesystem host sends the daemon looking for storage
  that is not there.
- Host detection selects storage, never behaviour. If a code path needs to
  differ between hosts for any other reason, that is a design smell, not a new
  caller for this package.
- Both `SQLiteVFS` values are durable. Never add a memory VFS to the
  selection: the daemon's databases are the only copy of VTXO, swap, and round
  state, and there is no server to re-fetch them from.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — sibling internal packages.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
