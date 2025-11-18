# Asset Transaction Representation

This note summarizes the pieces that make up the current implementation that
lives under `assets/`.  Everything described here is reflected in the code – the
document is only meant to help readers connect the moving parts.

## Two Layer Model

Every transfer is the combination of:

1. **Bitcoin anchor state** – who can spend the anchoring UTXO.  The spending
   conditions are provided by the closure helpers in `assets/op_true.go`,
   `assets/helpers.go`, and the onboarding helpers in
   `assets/onboarding.go`.  We rely on the Taproot tooling from btcd to build
   tap leaves, control blocks, and merkle roots.
2. **Taproot Asset virtual state** – which asset amounts move to which script
   keys.  We re-use the upstream `tappsbt` VPacket and proof primitives and
   only wrap them with a friendlier API.

The builder orchestrates both layers and spits out everything the harness tests
need: the virtual packet, the anchor PSBT template, witness plans, and the proof
exports.

## Important Types

| Location | Purpose |
|----------|---------|
| `assets/asset_tx.go` | Generic “asset transaction” container that mirrors the upstream Taproot Asset data model.  Used by onboarding helpers and tests to match transfers and derive merkle tweaks. |
| `assets/op_true.go`  | Utilities that build the OP_TRUE branch that we use for cooperative sweeps.  Returns the witness stack and control block that the builder records. |
| `assets/onboarding.go` | High level kit for script-only onboarding.  Produces the taproot sibling preimage and the MuSig2 aggregate keys used by the builder based flows. |
| `assets/asset_tx_builder.go` | The main builder.  Handles proof decoding, vpacket assembly, anchor PSBT creation, MuSig2/key spend helpers, tapscript helpers, and final publication. |

## Life of a Builder

1. **Construction** – `NewAssetTxBuilder` stores the target asset ID, network
   parameters, and allocates caches for script and anchor witnesses.
2. **Adding inputs/outputs** – `AddInput` and `AddOutput` validate the caller
   supplied configs and collect the closures.  Inputs store the raw proof blob
   so we can recover the taproot asset root during compilation.
3. **Compile** – `Compile()` decodes the input proofs, ensures they all point to
   the same asset ID, builds the `tappsbt` vpacket and records the witness
   plans.  Each script closure gets a `ScriptWitnessPlan` which carries the tap
   leaf, inclusion proof, script root, and combined taproot tweak that we later
   expose through `PrepareScriptSpend` and `GetTaprootRoots`.
4. **Commit** – `Commit()` sends the vpacket to tapd via
   `CommitVirtualPsbts`.  The response includes the updated vpacket(s) and the
   anchor PSBT template.  We store a copy so the tests can examine the anchor
   inputs.
5. **Witness population** – the builder caches any witness stack supplied via
   `SetScriptWitness`, `ApplyScriptSpend`, or `ApplyKeySpendSignature`.
   Script-path witnesses are recorded both in the virtual packet and in an
   `anchorWitnesses` map so we can re-attach them after the wallet returns the
   final PSBT.
6. **Finalize** – `FinalizeAnchor()` calls into the wallet to sign and finalize
   the anchor PSBT, then re-inserts any script-path witnesses we cached.  Tests
   and higher layers always see the fully populated PSBT.
7. **Publish / Export** – `Publish()` forwards everything to tapd’s
   `PublishAndLogTransfer` RPC.  `ExportProofs()` uses the tap manager to pull
   the updated proofs for each output script key.

## Signing Helpers

- `GetKeySpendSigHash` returns the BIP-341 digest for MuSig2 key spends.
- `PrepareScriptSpend` and `ApplyScriptSpend` wrap the tapscript branch flow:
  they hand back the digest, tapleaf, control block, and taproot/asset roots so
  the caller only needs to provide signatures.
- `GetTaprootRoots` exposes the script root, asset commitment root, and
  combined taproot tweak that `PrepareScriptSpend` stored in the witness plan.

These helpers are thin wrappers around the cached witness plans built during
`Compile()` – the tests in `test/assets/` use them to assert that the branch
that goes into the witness matches the data committed in the onboarding proof.

## Tests and Fixtures

The harness tests under `test/assets/` create onboarding fixtures using the
public helpers and then exercise both MuSig2 and script-path sweeps.  They also
verify that the control blocks we build locally reproduce the taproot root
exposed in the onboarding proof – this gives us coverage over the witness plan
caching and the taproot sibling ordering.

That’s the entire surface area we currently maintain.  Any new workflow should
be implemented in the `assets` package and documented here once it lands.
