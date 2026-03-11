# indexer

## Purpose

Server indexing client for receive script registration, wrapping
`arkrpc.IndexerServiceMailboxClient` with BIP-340 Schnorr signature proofs.

## Relationships

- **Depends on**: `arkrpc` (IndexerService stubs), `serverconn` (mailbox transport).
- **Depended on by**: `darepod` (wiring).
