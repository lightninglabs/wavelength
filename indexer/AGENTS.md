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

- `RegisterReceiveScriptTaproot` / `UnregisterReceiveScript` /
  `ListMyReceiveScripts` — Register, unregister, and enumerate the caller's
  receive scripts on the server.
- `TaprootScriptScope.PolicyTemplate` selects an exact-policy proof for
  `ListVTXOsByScripts`. `newPolicyScope` signs the script and canonical policy
  under the explicit `policy_script_scope` type. It creates no registration.
  Legacy `script_scope` proofs keep their original wire format.
- `BuildListVTXOsByScriptsTaprootRequest(ctx, scopes, afterCursor []byte, limit, statusFilter)` / `ListVTXOsByScriptsTaproot(ctx, scopes, afterCursor []byte, limit, statusFilter)` — Build and execute taproot-scope-proofed `ListVTXOsByScripts` queries. `afterCursor` is an opaque `[]byte` keyset cursor passed through unchanged. The proof covers each pkScript using owner-key signatures gated on script scope.
- `BuildGetOORSessionByTxidTaprootRequest` / `GetOORSessionByTxidTaproot` — Build and execute a taproot-proofed OOR session lookup by Ark txid.
- `BuildListOORRecipientEventsByScriptTaprootRequest` / `ListOORRecipientEventsByScriptTaproot` — Build and execute a taproot-proofed listing of OOR receive events for a given pkScript.
- `GetSubtreeByScriptsTaproot` — Taproot-proofed lookup of a VTXO subtree
  (optionally including internal nodes) for the given scopes.

## Relationships

- **Depends on**: `arkrpc` (generated IndexerService stubs), `mailbox/rpc`
  (transport-agnostic `RPCClient`/`RPCOptions` contracts), `internal/indexerlimits`
  (pagination bounds).
- **Depended on by**: `waved` (wiring, receive script registration, metadata
  queries), `proofkeys`, `walletcore`.

## Invariants

- All proof-of-control signatures are Schnorr (BIP-340) over a tagged hash of the request scope.
- Taproot-scoped proofs (`buildTaprootScopes`) attach per-script owner signatures so the server can verify that the caller controls the scripts being queried — preventing unauthorized balance enumeration.
- `WithSigner` returns a shallow copy; both the original and the copy share the same underlying RPC transport.
- The receive-script ownership proof is a TLV stream signed as a single tagged
  digest, so every record it carries is covered by the signature. The optional
  policy template record (`proofTLVTypePolicyTemplate`, TLV type 12) is emitted
  only when non-empty, which keeps standard registrations byte-identical to the
  pre-policy encoding while making a policy substitution on an exact-policy
  query invalidate the signature under the same signer key.

## Deep Docs

- [docs/custom-policy-queries.md](../docs/custom-policy-queries.md)
  — What the exact-policy query proof commits to and why the operator
  must reconstruct the output itself.
