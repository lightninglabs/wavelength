# lib/types

## Purpose

Shared domain types for Ark protocol messages exchanged between client and
server during round participation. These types are used across `round`, `vtxo`,
`wallet`, and `db` packages.

## Key Types

- `JoinRoundRequest` — Client's round registration request: boarding inputs, VTXO requests, forfeit requests, leave requests.
- `JoinRoundAuth` — Authentication data for round join (Schnorr signature proof-of-control).
- `VTXORequest` — Describes a new VTXO to create in a round (amount, owner key, cosigner keys, policy template). `IsChange bool` marks the designated change output under the #270 seal-time fee handshake; the server overrides `Output.Value` with the residual after deducting operator fee when `IsChange=true`. At most one `VTXORequest` or `LeaveRequest` per intent may set `IsChange=true`.
- `ForfeitRequest` — Describes a VTXO being forfeited (outpoint, connector leaf info, forfeit tx signature).
- `LeaveRequest` — Describes a cooperative exit (VTXO outpoint, destination address). `IsChange bool` has the same seal-time fee semantics as on `VTXORequest`; when set, the server stamps the residual onto this leave output.
- `BoardingRequest` — Describes a boarding input (outpoint, amount, script).
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
- **Depended on by**: `round` (round protocol messages), `wallet` (boarding types), `db` (persistence), `rpc` (proto conversion).

## Invariants

- `VTXOOwnerKeyFamily` (44) is the HD key family used for deriving VTXO owner signing keys.
- `JoinRoundAuthMessage` produces a deterministic byte encoding for Schnorr signature verification.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
