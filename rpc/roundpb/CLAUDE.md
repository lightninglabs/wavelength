# rpc/roundpb

## Purpose

Generated protobuf/gRPC stubs for the round protocol, plus hand-written
support code: `service.go` (mailbox method name constants), `convert.go`
(proto <-> Go domain-type conversions, including the security-sensitive
`TreeFromProto` VTXO-tree deserializer), and `version.go` (the round flow
version guard).

## Key Types

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`. The hand-written files define:

- `ServiceName` — Fully-qualified protobuf service name
  (`"round.v1.RoundService"`) used for mailbox event routing.
- Server→client push method names: `MethodJoinAck`, `MethodBatchInfo`,
  `MethodAwaitingInputSigs`, `MethodAggNonces`, `MethodAggSigs`,
  `MethodRoundFailed`, `MethodError`, `MethodJoinRoundQuote`.
- Client→server method names: `MethodJoinRound`, `MethodAcceptQuote`,
  `MethodRejectQuote`, `MethodSubmitNonces`, `MethodSubmitPartialSigs`,
  `MethodSubmitForfeitSigs` (boarding input sigs),
  `MethodSubmitVTXOForfeitSigs` (VTXO forfeit sigs).
- `TreeFromProto` / `TreeToProto` — Convert between `*VTXOTree` proto and
  `lib/tree.Tree`; `TreeFromProto` takes `WithMaxTreeNodes` to bound the
  deserialized node count (`DefaultMaxTreeNodes` = 50,000). Both carry the
  tree's optional asset sidecar: `TreeToProto` writes `VTXOTree.AssetRef`
  plus per-node `SigningTweak`, `AssetAmount`, and `AssetCommitmentRoot`;
  `TreeFromProto` rebuilds a `tree.AssetTreeContext` from them
  (`assetContextFromProto`).
- `OutpointFromProto`/`ToProto`, `TxOutFromProto`/`ToProto`,
  `PSBTFromBytes`/`ToBytes`, `MsgTxFromBytes`/`ToBytes`,
  `SchnorrSigFromBytes`/`ToBytes` — wire/proto ⇄ Go conversions for the
  round protocol's payload types.
- `FlowVersion` / `FlowVersionV1` / `ValidateFlowVersion` — the per-round
  choreography version stamped by the operator and validated by the
  client; fails closed on any version this build does not understand.

`MethodSubmitForfeitSigs` and `MethodSubmitVTXOForfeitSigs` are distinct
wire methods for two different payload types; see `round/CLAUDE.md` for
the `SubmitForfeitSigRequest` vs `SubmitVTXOForfeitSigsToServer`
distinction.

## Relationships

- **Depends on**: `lib/tree`, `lib/types` (conversion targets in
  `convert.go`), and `tap-sdk` (`ParseAssetRef`, for canonical asset-reference
  validation); otherwise generated proto types only.
- **Depended on by**: `round` (outbox routing, proto conversions, flow
  version), `db` (persisting round/VTXO proto blobs), `waved` (proto
  conversion, flow version).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Method name constants in `service.go` must match the proto service
  definition; mismatches silently drop events at the mailbox router.
- `TreeFromProto` enforces a pre-order invariant (child index > parent
  index), a **single-parent** invariant (no node may be named as a child
  twice), and bounds on child/output indices; this is what prevents a
  malicious server from encoding cycles, shared children, or
  out-of-range references in a `VTXOTree` and DoS-ing tree traversal. Do
  not relax these checks. The pre-order check alone does **not** imply
  single-parent: two parents at indices 0 and 1 can both name child 5
  and both satisfy `childIdx > i`, which decodes a DAG whose shared
  subtree every recursive walk re-visits once per path reaching it.
- `TreeFromProto` additionally requires every node other than the root
  to be claimed exactly once, so the decoded shape is a single connected
  tree rather than a forest. Without it a sender can pad a message up to
  the node cap with nodes no walk from `Root` reaches, each of which is
  still deserialized and run through `tree.ComputeFinalKey`.
- The same three structural invariants plus a node cap
  (`DefaultMaxTreePathNodes`) are enforced by `arkrpc.TreePathToTree`,
  the sibling decoder on the untrusted indexer receive path. Keep the
  two in step: a shape rejected by one and accepted by the other is a
  gap, not a difference in trust level.
- **The asset sidecar is validated before any key derivation.** Per-node
  asset fields without a `VTXOTree.AssetRef` are rejected outright; an
  `AssetRef` must parse *and* round-trip to its canonical encoding
  (`assetRef.String() == pt.AssetRef`), so a non-canonical spelling of the
  same asset cannot slip through; and `AssetTreeContext.Validate(root)` must
  pass before `TreeFromProto` derives final keys from the per-node signing
  tweaks. An asset tree is absent, not invalid, when `AssetRef` is empty and
  no node carries asset fields — that decodes to a nil `AssetContext`, which
  is the normal non-asset case.
- **The signing tweak is what binds a node to its asset commitment.** With an
  asset context present, each node's taproot tweak comes from
  `assetCtx.SigningTweak(node.Input)` rather than the sweep root alone.
  Accepting per-node tweaks without first validating the context would let a
  sender steer final-key derivation.
- `ValidateFlowVersion` must reject any `FlowVersion` other than the
  versions this build implements (currently only `FlowVersionV1`); never
  make it permissive by default.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
