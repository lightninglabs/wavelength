# lib/swaprecovery

## Purpose

Defines the public versioned metadata and domain-separated derivation for
seed-restorable receive preimages. It serializes no private keys or secrets.

## Contract

- `OwnerProofTag` is `wavelength-swap-recovery-owner-v1`, the sole recovery
  owner signing tag accepted by the daemon's public identity-signing RPC.
- `NewParams` encodes version 1, a non-hardened key index, and a fresh
  32-byte nonce. `ParseParams` rejects unsupported encodings and indices.
- `Derive` uses HKDF-SHA256 under `wavelength-swap-preimage-v1`, binding the
  network genesis hash and parameters to a wallet self-ECDH secret from
  key family 46. Repeated derivation with the same wallet is deterministic.
- Never use a public scalar or public key as the ECDH secret. Keep that secret
  private and reveal an invoice's derived preimage only for settlement.
- There is no compatibility path for the unreleased previous domains.

## Trust Boundary

The daemon's recovery-only signing policy is not enforced by lnd's raw signer.
Anyone with lnd `SignMessage` or `DeriveSharedKey` access can bypass that
policy and sign owner proofs or reproduce preimages using public parameters.
Treat those credentials and any remote signer as trusted wallet-secret access.
See [seed restore](../../docs/seed_restore_recovery.md).

## Relationships

- Used by `sdk/swaps` for registration and proof signing, and by `waved`
  for constrained identity signing and wallet-backed preimage derivation.
- Requires the same encoding and domains on the swap server; frozen vectors
  in both repositories cover the owner-proof transcript.
