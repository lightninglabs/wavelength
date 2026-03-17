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

## Relationships

- **Depends on**: `arkrpc` (IndexerService stubs), `serverconn` (mailbox transport).
- **Depended on by**: `darepod` (wiring, receive script registration, metadata queries).
