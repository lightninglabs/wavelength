# lib/types

## Purpose

Shared domain types for Ark protocol messages exchanged between client and
server during round participation. These types are used across `round`, `vtxo`,
`wallet`, and `db` packages.

## Key Types

- `JoinRoundRequest` — Client's round registration request: boarding inputs, VTXO requests, forfeit requests, leave requests.
- `JoinRoundAuth` — Authentication data for round join (Schnorr signature proof-of-control).
- `VTXORequest` — Describes a new VTXO to create in a round (amount,
  owner key, cosigner keys). `IsChange bool` (TLV record 4) marks the
  output that absorbs the server-computed fee residual under the #270
  seal-time handshake; serialized into `JoinRoundAuth`. `FixedAmount bool`
  (TLV record 5) marks a request whose amount must not be adjusted by
  server-side fee distribution.
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
- `OperatorTerms` — Server-published round parameters (fee rates, expiry config, connector dust amount). `MaxOORLineageVBytes uint32` carries the operator-published cap on the cumulative on-chain vbytes a recipient must publish to claim a VTXO produced by an OOR submit unilaterally. Zero means no cap enforced server-side (clients fall back to a conservative local default).
- `Ancestry` — One rooted commitment-tree fragment contributing ancestry to a VTXO (defined in `lib/types/ancestry.go`). Fields: `TreePath *tree.Tree` (extracted root-to-leaf path), `CommitmentTxID chainhash.Hash`, `InputIndices []uint32` (Ark tx input indices this fragment serves; empty for round-direct VTXOs), `TreeDepth uint32`. Round-direct VTXOs carry a single-element slice; cross-round multi-input OOR VTXOs carry one entry per distinct commitment tx.
- `MaxAncestryTreeDepth([]Ancestry) int` — Returns the largest `TreeDepth` across a slice; drives worst-case unilateral-exit timing calculations.
- `ClientBatchInfo` — Client's view of batch output info after tree construction.
- `BatchOutputInfo` — Batch output metadata (outpoint, value, tree root).
- `ConnectorLeafInfo` — Assigned connector leaf (outpoint + output) plus the connector-tree ancestry params (`RootOutputIndex`, `NumLeaves`, `Radix`, `LeafIndex`) the client uses to reconstruct the tree and prove the leaf descends from the commitment tx before signing the forfeit (darepo-client#681).
- `BoardingInputSignature` — Signed boarding input for round commitment.
- `ForfeitTxSig` — Forfeit transaction signature. `ParticipantVTXOSigs
  []*ForfeitParticipantSig` carries each non-operator participant's tapscript
  signature share.
- `ForfeitParticipantSig` — One non-operator participant's tapscript signature
  contribution to a forfeit transaction.
- `SerializeTxProof` / `DeserializeTxProof` — TLV codec for `proof.TxProof`
  (defined in `lib/types/proof_codec.go`); the canonical wire encoding shared
  by `wallet` and `db` for `BoardingRequest.TxProof`.
- `OORPackageDirection` / `OORPackageLinkKind` — Enums for OOR package direction and link types.
- `VTXORequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` / `EffectivePkScript` — Policy helpers that decode the serialized `PolicyTemplate` field into an `arkscript.PolicyTemplate` and derive the output pkScript.
- `BoardingRequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` — Equivalent policy helpers for boarding inputs.
- `VTXORequest.HasLocalOwner` — Reports whether the VTXO request has a locally-owned key (non-zero `KeyLocator`).
- `VTXOOrigin` — Local-only classification (`Unknown`, `RoundBoarding`, `RoundRefresh`, `RoundTransfer`) stamped on `VTXORequest.Origin` at wallet intent-composition time. Not serialized onto the join-round wire. Consumed downstream by the round actor's `emitVTXOsReceived` dispatch so each owned round VTXO gets a correctly classified `ledger.VTXOReceivedMsg.Source` (boarding credits `wallet_balance`, refresh credits `transfers_out`, transfer credits `transfers_in`). See [docs/fee_ledger.md](../../docs/fee_ledger.md) for the full routing table.

## Relationships

- **Depends on**: `lib/arkscript` (policy template decoding, `StandardVTXOParams`), `lib/tree` (tree types, used by `Ancestry.TreePath`).
- **Depended on by**: `round` (round protocol messages), `wallet` (boarding types), `db` (persistence), `rpc/roundpb` (proto conversion), `vtxo` (FSM environment and outbox messages), `oor` (package persistence), `darepod` (wallet/forfeit wiring), `lib/actormsg` (admission message fields), `swapclientserver`.

## Invariants

- `VTXOOwnerKeyFamily` (44) is the HD key family used for deriving VTXO owner signing keys.
- `VTXOSigningKeyFamily` (45) is the HD key family used for per-round VTXO MuSig2 signing keys.
- `JoinRoundAuthMessage` produces a deterministic byte encoding for Schnorr
  signature verification; `DecodeJoinRoundAuthMessage` enforces the matching
  decode-time size limits (`joinRoundAuthMaxRequestCount`,
  `joinRoundAuthMaxBlobEntrySize`, `joinRoundAuthMaxScriptSize`) so a
  malicious peer cannot force unbounded allocation during decode.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
