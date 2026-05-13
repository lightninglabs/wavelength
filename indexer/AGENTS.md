# indexer

## Purpose

Server indexing client for receive script registration and VTXO queries,
wrapping `arkrpc.IndexerServiceMailboxClient` with BIP-340 Schnorr signature
proofs for proof-of-control.

## Key Types

- `Client` — Wraps the mailbox RPC client with automatic proof-of-control
  signing. `WithSigner` returns a shallow copy using a different signer.
- `SchnorrSigner` — Interface for signing proof-of-control messages.
  Implementations: `PrivKeySchnorrSigner` (raw key), `LNDSchnorrSigner`
  (LND wallet), `KeyRingSchnorrSigner` (btcwallet keyring).
- `ProofPubKey` — Returns the owner pubkey used in proof construction.
  Added to `SchnorrSigner` implementations for receive script ownership.
- `SyncClient` / `SyncCursorStore` / `SyncBackend` — Cursor-based sync infrastructure for VTXO/OOR event polling.
- `MemorySyncCursorStore` — In-memory cursor store (test-only compilation unit).

## Key Methods (on `*Client`)

- `BuildListVTXOsByScriptsTaprootRequest(ctx, scopes, afterCursor []byte, limit, statusFilter)` / `ListVTXOsByScriptsTaproot(ctx, scopes, afterCursor []byte, limit, statusFilter)` — Build and execute taproot-scope-proofed `ListVTXOsByScripts` queries. The `afterCursor` is opaque `[]byte` (keyset cursor) passed through unchanged, replacing the former `uint64` offset cursor. The proof covers each pkScript in the request using owner-key signatures gated on script scope.
- `BuildGetOORSessionByTxidTaprootRequest` / `GetOORSessionByTxidTaproot` — Build and execute a taproot-proofed OOR session lookup by Ark txid.
- `BuildListOORRecipientEventsByScriptTaprootRequest` / `ListOORRecipientEventsByScriptTaproot` — Build and execute a taproot-proofed listing of OOR receive events for a given pkScript.

## Relationships

- **Depends on**: `arkrpc` (IndexerService stubs), `serverconn` (mailbox transport).
- **Depended on by**: `darepod` (wiring, receive script registration, metadata queries).

## Invariants

- All proof-of-control signatures are Schnorr (BIP-340) over a tagged hash of the request scope.
- Taproot-scoped proofs (`buildTaprootScopes`) attach per-script owner signatures so the server can verify that the caller controls the scripts being queried — preventing unauthorized balance enumeration.
- `WithSigner` returns a shallow copy; both the original and the copy share the same underlying RPC transport.
