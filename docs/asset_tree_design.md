# Asset-Aware Tree Design (darepo-client)

## Background

The current `lib/tree` package builds BTC-only virtual transaction trees from a
list of `LeafDescriptor`s. Each node is a single-input transaction with:

- `Input`: outpoint being spent.
- `Outputs`: taproot outputs for children plus a zero-fee anchor
  (`scripts.AnchorOutput`).
- `CoSigners`: MuSig2 participants for the input key spend.
- `Signature`: aggregated MuSig2 signature once signing finishes.

Construction is breadth-first via `NewTree` → `buildTreeBFS`, which groups
leaves by a configurable radix and instantiates leaf/branch nodes with
`NewLeafNode` / `NewBranchNode`. The tree is agnostic to assets: leaves only
carry a pkScript, amount, and a cosigner key; no asset proofs or funding data
flow through the structure.

The `assets` package already builds asset anchors (tapscript + asset commit) and
produces witness plans (`assets.AssetTxBuilder`). We need a way to attach the
asset-specific context (plan, proof bytes, BTC liquidity attribution) to the
tree without disturbing the existing signing and verification logic.

## Goals

1. Keep the existing tree construction/signing API intact for BTC-only flows.
2. Extend leaves to carry optional taproot-asset metadata, proofs, and funding
   bookkeeping.
3. Propagate lightweight per-node metadata so clients/operators can reason about
   asset branches (totals, proof blobs) without touching sighashes.
4. Minimize churn: a single leaf type, additive fields, no change to MuSig2
   semantics.
5. Provide a clear handoff between `assets.AssetTxBuilder` outputs and the tree
   builder inputs.

## Proposed Data Model (additive)

### LeafDescriptor extension

```go
// lib/tree/leaf.go
type LeafDescriptor struct {
    PkScript    []byte
    Amount      btcutil.Amount
    CoSignerKey *btcec.PublicKey

    // Asset holds optional asset anchor/proof context for this leaf.
    Asset *AssetMetadata
}
```

### Asset metadata payload

```go
// lib/tree/leaf.go (or new file under lib/tree)
type AssetMetadata struct {
    Plan           *assets.AnchorPlan // witness blueprint from AssetTxBuilder
    Proof          []byte             // serialized taproot-asset proof blob
    Funding        LeafFunding        // who supplied BTC dust and how much
    ChangePkScript []byte             // where to return BTC reimbursements
    ExitRebalance  btcutil.Amount     // +ve: server owes client; -ve: client owes
    Labels         map[string]string  // optional annotations (client id, asset id)
}

type LeafFunding struct {
    Mode   FundingMode    // OperatorProvided | ClientGas | Unknown
    Amount btcutil.Amount // sats attributed to the funder
}

type FundingMode uint8

const (
    FundingModeUnknown FundingMode = iota
    FundingModeOperatorProvided
    FundingModeClientGas
)
```

Notes:
- All fields are optional via `Asset == nil`. BTC-only callers keep using the
  existing fields; serialization code can omit asset metadata.
- `Plan`/`Proof` match the structures already emitted by `assets.AssetTxBuilder`
  for anchors.
- `ExitRebalance` captures reimbursable BTC at exit (operator fronted dust vs.
  client gas).

### Node metadata (optional)

We keep the core `Node` shape but allow attaching metadata for introspection:

```go
// lib/tree/node.go
type Node struct {
    Input     wire.OutPoint
    Outputs   []*wire.TxOut
    CoSigners []*btcec.PublicKey
    Children  map[uint32]*Node
    Signature *schnorr.Signature
    FinalKey  *btcec.PublicKey

    // Metadata is optional and ignored by signing/verification logic.
    Metadata *NodeMetadata
}

type NodeMetadata struct {
    // Aggregated across the subtree rooted at this node.
    TotalLeaves  int
    TotalFunding btcutil.Amount

    // Asset-specific payload for this branch/leaf.
    AssetProof []byte      // e.g., concatenated proofs or leaf proof
    Leaf       *LeafAssetMeta // set only at leaves
}

// LeafAssetMeta is a slim copy of the leaf-level asset info so callers don't
// have to reach back into input specs after the tree is built.
type LeafAssetMeta struct {
    Plan           *assets.AnchorPlan
    Proof          []byte
    Funding        LeafFunding
    ChangePkScript []byte
    ExitRebalance  btcutil.Amount
    Labels         map[string]string
}
```

Population rules:
- During leaf creation, if `LeafDescriptor.Asset != nil`, copy it into
  `Node.Metadata.Leaf` and set `TotalLeaves=1`, `TotalFunding=Funding.Amount`,
  `AssetProof=Asset.Proof`.
- During branch creation, aggregate `TotalLeaves` and `TotalFunding` from
  children. `AssetProof` stays nil unless a caller chooses to aggregate proofs
  in a follow-up pass.
- Signing (`ComputeFinalKey`, MuSig2) does not depend on `Metadata`.

### Output metadata (optional)

If needed for asset exits, we can optionally attach tapscript control data per
output:

```go
type OutputMetadata struct {
    ControlBlock []byte
    TapLeafHash  chainhash.Hash
}
```

Attach as a parallel slice to `Node.Outputs` if/when the implementation needs
it; otherwise keep it out of the hot path.

## Construction Flow (unchanged traversal, richer node init)

1. **Leaf assembly**: `assets.AssetTxBuilder` produces an anchor plan and proof.
   The caller wraps these into `LeafDescriptor{PkScript, Amount, CoSignerKey,
   Asset: &AssetMetadata{...}}`.
2. **Tree build**: Call `tree.NewTree` as today. `NewLeafNode` copies
   `LeafDescriptor.Asset` into `Node.Metadata`. `NewBranchNode` aggregates
   metadata (counts/funding).
3. **Signing**: `SignerSession` uses `Node.CoSigners` and `FinalKey` exactly as
   before. Metadata is ignored by sighashes.
4. **Distribution**: When returning paths to clients/operators, include the node
  metadata so wallets can assemble witnesses and track reimbursements without
  re-deriving asset state.

No changes are required to the BFS grouping, radix handling, or MuSig2 key
aggregation: asset data is purely side-channel for consumers.

## Integration Points

- **assets → tree**: Add a helper that converts an asset anchor build result
  into a `LeafDescriptor` with `AssetMetadata`.
- **Persistence/RPC**: When trees are stored or served, serialize `Asset` and
  `Node.Metadata` fields if present. BTC-only callers can ignore them.
- **Exit accounting**: Wallets/operators read `ExitRebalance` and `Funding` to
  decide who reimburses whom when a leaf exits the tree.

## Example Walkthrough

1) Client A (asset) funds dust themselves; Client B (asset) uses operator dust;
   Client C is BTC-only.
2) Build three `LeafDescriptor`s:
   - A: `Asset` filled, `Funding.Mode=ClientGas`, `ExitRebalance=0`.
   - B: `Asset` filled, `Funding.Mode=OperatorProvided`,
     `ExitRebalance=-dustAmount`.
   - C: `Asset=nil`.
3) `NewTree` groups leaves, creates nodes, attaches metadata at leaves, and
   aggregates counts/funding on branches.
4) MuSig2 signing runs unchanged. Metadata rides along for later proof/witness
   assembly and BTC reimbursement logic.

## Implementation Checklist

1. Extend `LeafDescriptor` with `Asset *AssetMetadata`.
2. Define `AssetMetadata`, `LeafFunding`, `FundingMode`.
3. Add optional `Metadata *NodeMetadata` to `Node` and populate it in
   `NewLeafNode` / `NewBranchNode`.
4. (Optional) Define `OutputMetadata` if/when tapleaf/control-block data needs
   to travel with the tree.
5. Add conversion helpers from `assets.AssetTxBuilder` outputs to
   `LeafDescriptor`.
6. Update serialization/RPC (if any) to carry the optional metadata.
7. Add docstring/tests to ensure BTC-only flows behave unchanged when
   `Asset=nil`.
8. Add a weighting hook (e.g., `WeightFunc` in builder config) so asset callers
   can balance the tree on asset amounts instead of BTC amounts; default to BTC
   amount for BTC-only flows.
