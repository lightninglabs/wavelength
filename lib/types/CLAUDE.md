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
  (TLV record 5) requires the operator quote to preserve `Amount` exactly —
  used for contract outputs where shrinking the replacement output would
  invalidate the higher-level protocol; a fixed single output is not
  eligible for the implicit-change exception.
- `ForfeitRequest` — Describes a VTXO being forfeited (outpoint,
  connector leaf info, forfeit tx signature). `AuthSpend *arkscript.SpendPath`
  (proof-of-control path for join-auth) and `ForfeitSpend *arkscript.SpendPath`
  (spend path for building the round forfeit tx) are nil for standard wallet
  VTXOs, where the operator loads the canonical path from the VTXO registry.
  Custom-script VTXOs populate both fields, which are serialized onto the
  join-round wire (TLV records 3/4 on the forfeit entry) so the operator can
  validate the caller-provided path and build the exact connector-bound
  forfeit request.
- `LeaveRequest` — Describes a cooperative exit (VTXO outpoint, destination
  address). `IsChange bool` (TLV record 3) marks the leave output that
  absorbs the server fee residual; serialized into `JoinRoundAuth`.
- `BoardingRequest` — Describes a boarding input (outpoint, amount, script).
  `TxProof fn.Option[proof.TxProof]` carries an optional SPV merkle
  inclusion proof for server-side verification of boarding UTXOs without
  requiring the server's own chain source.
- `OperatorTerms` — Server-published round parameters (fee rates, expiry
  config, connector dust amount, `DustLimit`, `MinVTXOAmount`,
  `MaxVTXOAmount`, `MaxUserBalance`). `MaxVTXOAmount` caps the amount
  accepted per VTXO across boarding requests, round outputs, and OOR
  recipient outputs; `MaxUserBalance` caps a single user's total system
  balance (zero means no cap, enforced client-side before funds enter the
  system). There is no `ForfeitScript`/`SweepKey`/`SweepDelay` field — sweep
  parameters live on the per-round `tree.Tree` (see `BatchOutputInfo.Tree`),
  not on the operator terms.
  `MaxOORLineageVBytes uint32` carries the operator-published cap on the
  cumulative on-chain vbytes a recipient must publish to claim a VTXO
  produced by an OOR submit unilaterally. Zero means no cap enforced
  server-side (clients fall back to a conservative local default).
  `OperatorTerms.MinVTXOAmountFloor()` returns `MinVTXOAmount` floored at
  `DustLimit`, so a zero, below-dust, or negative advertised minimum (older
  or misconfigured operator snapshots) never lets a VTXO amount go below
  dust.
- `Ancestry` — One rooted commitment-tree fragment contributing ancestry to a VTXO (defined in `lib/types/ancestry.go`). Fields: `TreePath *tree.Tree` (extracted root-to-leaf path), `CommitmentTxID chainhash.Hash`, `InputIndices []uint32` (Ark tx input indices this fragment serves; empty for round-direct VTXOs), `TreeDepth uint32`. Round-direct VTXOs carry a single-element slice; cross-round multi-input OOR VTXOs carry one entry per distinct commitment tx.
- `MaxAncestryTreeDepth([]Ancestry) int` — Returns the largest `TreeDepth` across a slice; drives worst-case unilateral-exit timing calculations.
- `ClientBatchInfo` — Client's view of batch output info after tree construction.
- `BatchOutputInfo` — Batch output metadata (outpoint, value, tree root).
- `ConnectorLeafInfo` — Assigned connector leaf (outpoint + output) plus the connector-tree ancestry params (`RootOutputIndex`, `NumLeaves`, `Radix`, `LeafIndex`) the client uses to reconstruct the tree and prove the leaf descends from the commitment tx before signing the forfeit (darepo-client#681).
- `BoardingInputSignature` — Signed boarding input for round commitment.
- `ForfeitTxSig` — Forfeit transaction signature. `ClientVTXOSig` covers the
  standard single-signer case. `ParticipantVTXOSigs []*ForfeitParticipantSig`
  carries additional tapscript signatures from every other non-operator
  participant that must authorize the selected spend path — needed for
  custom policies (e.g. vHTLC refund-style paths) that require more than one
  client-side signature for a single forfeited VTXO. `SpendPath` is the
  canonical arkscript spend path for the forfeited input, making the
  custom-or-standard tapscript leaf explicit in round messaging.
- `ForfeitParticipantSig` — One non-operator participant's tapscript
  signature for a forfeited VTXO input: `PubKey *btcec.PublicKey` (the
  x-only key that produced it) and `Signature *schnorr.Signature`.
- `OORPackageDirection` / `OORPackageLinkKind` — Enums for OOR package direction and link types.
- `VTXORequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` / `EffectivePkScript` — Policy helpers that decode the serialized `PolicyTemplate` field into an `arkscript.PolicyTemplate` and derive the output pkScript.
- `BoardingRequest.EffectivePolicyTemplate` / `DecodePolicyTemplate` / `DecodeStandardPolicyTemplate` — Equivalent policy helpers for boarding inputs.
- `VTXORequest.HasLocalOwner` — Reports whether the VTXO request has a locally-owned key (non-zero `KeyLocator`).
- `VTXOOrigin` — Local-only classification (`Unknown`, `RoundBoarding`, `RoundRefresh`, `RoundTransfer`) stamped on `VTXORequest.Origin` at wallet intent-composition time. Not serialized onto the join-round wire. Consumed downstream by the round actor's `emitVTXOsReceived` dispatch so each owned round VTXO gets a correctly classified `ledger.VTXOReceivedMsg.Source` (boarding credits `wallet_balance`, refresh credits `transfers_out`, transfer credits `transfers_in`). See [docs/fee_ledger.md](../../docs/fee_ledger.md) for the full routing table.

## Relationships

- **Depends on**: `lib/arkscript` (policy template decoding, `StandardVTXOParams`), `lib/tree` (tree types, used by `Ancestry.TreePath`).
- **Depended on by**: `round` (round protocol messages), `wallet` (boarding types), `db` (persistence), `rpc` (proto conversion).

## Invariants

- `VTXOOwnerKeyFamily` (44) is the HD key family used for deriving VTXO owner signing keys.
- `VTXOSigningKeyFamily` (45) is the HD key family used for per-round VTXO MuSig2 signing keys.
- `JoinRoundAuthMessage` produces a deterministic byte encoding for Schnorr signature verification. `joinRoundAuthMessageVersion` is currently 3 (bumped for `VTXORequest.FixedAmount` and the forfeit `AuthSpend`/`ForfeitSpend` TLV records); the auth message binds `ForfeitRequest.AuthSpend`/`ForfeitSpend` when present, so changing either invalidates any existing signature over the request.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
