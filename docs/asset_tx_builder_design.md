# Asset Transaction Builder

The `assets/asset_tx_builder.go` file implements the single-asset transaction
builder that every test in `test/assets/` and `test/tapflow/` relies on.  This
note documents the responsibilities of the builder so future edits can be
validated against the intended behaviour.

## Goals

The builder wraps the Taproot Assets RPC surface and reduces the amount of book
keeping the caller has to do:

1. Decode proof files and assemble a single `tappsbt.VPacket`.
2. Prepare anchor metadata (MuSig2 key spend or tapscript closures).
3. Drive the tapd RPC sequence: `CommitVirtualPsbts → FinalizePsbt →
   PublishAndLogTransfer`.
4. Cache every witness artifact (OP_TRUE siblings, tapscript control blocks,
   MuSig2 digests) so tests can assert on them without duplicating the taproot
   math.

Throughout the file we stick to the convention that *asset* always refers to
the virtual transaction and *anchor* to the Bitcoin layer.

## Data Model

### Inputs and Outputs

- `InputConfig` stores the raw proof file, the anchor key specification, and the
  optional tapscript closures for that input.
- `OutputConfig` declares the amount, anchor metadata, and the asset-level
  script to commit in the vpacket.
- `BtcInputSpec` and `BtcOutputSpec` capture BTC-only inputs and outputs
  that accompany the asset transfer (for example connectors used in ARK
  forfeits). These never touch the virtual transaction but are inserted into the
  anchor PSBT so downstream code can rebuild the connector tree.

Every closure can either wrap an `arklib/script` closure or supply a raw script
and a custom witness constructor.  This keeps the builder detached from higher
level policy while still letting tests build synthetic flows. When a script spec
needs to surface additional data (for example the OP_TRUE witness cache), it
does so through the typed `AssetScriptDetails` interface attached to the witness
plan.

### Plans

`Compile()` attaches a `ScriptWitnessPlan` to each closure.  The plan stores:

- the tap leaf derived from the closure,
- the control block inclusion proof returned by tapd,
- the taproot asset root (`AssetRoot`), the closure-only root (`ScriptRoot`),
  and the combined tweak committed in the anchor output (`TaprootRoot`),
- an optional OP_TRUE witness (for the simple cooperative branch).

Because the plan is cached, `PrepareScriptSpend` and `GetTaprootRoots` are
pure lookups – the code path no longer re-derives merkle proofs.

### Anchor Witness Cache

Script-path witnesses are stored twice:

1. In `scriptWitnesses` so the virtual packet is always up to date.
2. In `anchorWitnesses` so `FinalizeAnchor` can re-attach the witness after the
   wallet rewrites the PSBT.

This is necessary because the wallet only takes responsibility for MuSig2 key
spends.  Script-path witnesses remain the caller’s job.

## Execution Flow

1. **AddAssetInput / AddAssetOutput / AddBtcInput / AddBtcOutput** – validate
   configs and store a copy of the proof blob.  Asset inputs and outputs affect
   the virtual packet, while the BTC variants only touch the on-chain PSBT.
2. **Compile** – decode the proof file, build the `tappsbt.VPacket`, prime the
   witness plans, and persist the script witness map.
3. **Commit** – call `CommitVirtualPsbts`, capture the response, and keep a copy
   of any anchor inputs the wallet injects (UTXO leases, change outputs, etc.).
4. **Witness** – use `SetScriptWitness`, `PrepareScriptSpend`, or
   `ApplyScriptSpend` to populate the script-path witnesses.  `GetTaprootRoots`
   exposes the tweak triple for MuSig2 aggregators.
5. **Finalize** – after the wallet signs the anchor, replay our cached
   script-path witnesses into the PSBT.
6. **Publish / Export** – forward everything to tapd and retrieve the fresh
   proofs once the transfer is logged.

The harness tests exercise each step and assert on the taproot roots stored in
the plan.  Whenever a regression happens the failure mode is usually: “proof
merkle root does not match the asset root we reconstruct”, which means the
builder cached data correctly but the inputs handed to it were inconsistent.

## External APIs

The builder intentionally exposes only the knobs the tests need:

- `GetKeySpendSigHash` – BIP-341 digest for MuSig2 key spends.
- `PrepareScriptSpend` – tapscript digest + control block + tweak tuple.
- `GetTaprootRoots` – direct access to `(scriptRoot, assetRoot, taprootRoot)`.
- `ApplyScriptSpend` – witness assembly using the `WitnessFunc` supplied by the
  closure.

Everything else (`SetScriptWitness`, `ApplyKeySpendSignature`,
`FinalizeAnchor`, `Publish`, `ExportProofs`) mirrors the downstream RPCs.

Refer to `test/assets/onboarding_test.go` for the most complete walk through of
the builder – it is intentionally verbose and logs intermediate values when the
taproot tweak does not line up.
