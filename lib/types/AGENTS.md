# lib/types

## Purpose

Shared domain types for Ark protocol messages exchanged between client and
server during round participation. These types are used across `round`, `vtxo`,
`wallet`, and `db` packages.

## Key Types

- `Ancestry` — One rooted commitment-tree fragment that contributes ancestry to
  a VTXO. Round-direct VTXOs have exactly one entry; OOR VTXOs whose ancestry
  spans multiple commitment txs (cross-round multi-input Ark spend) have one
  entry per distinct commitment tx. Fields: `TreePath *tree.Tree`,
  `CommitmentTxID chainhash.Hash`, `InputIndices []uint32` (Ark tx input indices
  served by this fragment; empty for round-direct VTXOs), `TreeDepth uint32`
  (worst-case unilateral-exit depth for this fragment). Lives in `lib/types` so
  both `round.ClientVTXO` and `vtxo.Descriptor` can carry the same multi-fragment
  ancestry without an import cycle.
- `JoinRoundRequest` — Client's round registration request: boarding inputs, VTXO requests, forfeit requests, leave requests.
- `JoinRoundAuth` — Authentication data for round join (Schnorr signature proof-of-control).
- `VTXORequest` — Describes a new VTXO to create in a round (amount,
  owner key, cosigner keys). `IsChange bool` (TLV record 4) marks the
  output that absorbs the server-computed fee residual under the #270
  seal-time handshake; serialized into `JoinRoundAuth`.
- `ForfeitRequest` — Describes a VTXO being forfeited (outpoint,
  connector leaf info, forfeit tx signature). Local-only fields:
  `AuthSpend *arkscript.SpendPath` (proof-of-control path for custom-script
  join-auth construction) and `ForfeitSpend *arkscript.SpendPath` (spend
  path for forfeit tx building when the VTXO uses a non-standard policy).
- `LeaveRequest` — Describes a cooperative exit (VTXO outpoint, destination
  address). `IsChange bool` (TLV record 3) marks the leave output that
  absorbs the server fee residual; serialized into `JoinRoundAuth`.
- `BoardingRequest` — Describes a boarding input (outpoint, amount, script).
  `TxProof fn.Option[proof.TxProof]` carries an optional SPV merkle
  inclusion proof for server-side verification of boarding UTXOs without
  requiring the server's own chain source.
- `OperatorTerms` — Server-published round parameters (fee rates, expiry config, connector dust amount).
- `ClientBatchInfo` — Client's view of batch output info after tree construction.
- `BatchOutputInfo` — Batch output metadata (outpoint, value, tree root).
- `ConnectorLeafInfo` — Connector output index and leaf info for forfeit construction.
- `BoardingInputSignature` — Signed boarding input for round commitment.
- `ForfeitTxSig` — Forfeit transaction signature.
- `OORPackageDirection` / `OORPackageLinkKind` — Enums for OOR package direction and link types.
- `VTXORequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` / `EffectivePkScript` — Policy helpers that decode the serialized `PolicyTemplate` field into an `arkscript.PolicyTemplate` and derive the output pkScript.
- `BoardingRequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` — Equivalent policy helpers for boarding inputs.
- `VTXORequest.HasLocalOwner` — Reports whether the VTXO request has a locally-owned key (non-zero `KeyLocator`).
- `VTXOOrigin` — Local-only classification (`Unknown`, `RoundBoarding`, `RoundRefresh`, `RoundTransfer`) stamped on `VTXORequest.Origin` at wallet intent-composition time. Not serialized onto the join-round wire. Consumed downstream by the round actor's `emitVTXOsReceived` dispatch so each owned round VTXO gets a correctly classified `ledger.VTXOReceivedMsg.Source` (boarding credits `wallet_balance`, refresh credits `transfers_out`, transfer credits `transfers_in`). See [docs/fee_ledger.md](../../docs/fee_ledger.md) for the full routing table.

## Relationships

- **Depends on**: `lib/arkscript` (policy template decoding, `StandardVTXOParams`), `lib/tree` (tree types).
- **Depended on by**: `round` (round protocol messages, `ClientVTXO.Ancestry`), `vtxo` (re-exported as `vtxo.Ancestry` type alias), `wallet` (boarding types), `db` (persistence), `rpc` (proto conversion).

## Invariants

- `VTXOOwnerKeyFamily` (44) is the HD key family used for deriving VTXO owner signing keys.
- `JoinRoundAuthMessage` produces a deterministic byte encoding for Schnorr signature verification.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
