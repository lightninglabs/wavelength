# Build partial Taproot Asset OOR transfers with explicit carriers

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to
date as work proceeds. This document is maintained in accordance with
`PLANS.md` at the repository root.

## Purpose / Big Picture

After this change, a Wavelength wallet that owns a VTXO carrying 1,000 units of
a Taproot Asset can send 800 units to another Wavelength wallet and retain 200
units as asset change. The caller supplies the receiver's Bitcoin carrier
satoshis and, when asset change exists, an explicit carrier amount for that
change. If the asset input does not contain enough satoshis, ordinary
Bitcoin-only VTXOs fund the shortfall. Any remaining satoshis return as a
normal Bitcoin-only change VTXO. Asset units and satoshis are never converted
or silently exchanged.

The observable result is a mixed out-of-round (OOR) package with one
asset-bearing checkpoint, zero or more Bitcoin-only checkpoints, an
asset-bearing receiver, optional asset change, optional Bitcoin change, and a
zero-valued pay-to-anchor output. The exact package and all change scripts
survive a daemon restart. A receiver can spend the asset again using the
sealed package that created its VTXO, without requiring the sender's original
anchor to appear in the receiver's tapd wallet inventory.

This is a prototype. It does not offer an asset-only user experience, pay
Bitcoin fees in an asset, automatically swap assets for satoshis, publish
unconfirmed compact proofs through chain porter, or make swapd hold tapd
credentials.

## Progress

- [x] (2026-07-21 21:30Z) Audited the current exact-send builder, durable OOR
  state, mixed checkpoint container, carrier selection, output ordering, proof
  reconstruction, runtime registration, and tap-sdk output-preview PR.
- [x] (2026-07-21 21:30Z) Restacked the Wavelength asset-state branch on the
  exported runtime-registration branch so exact-package lookup can be injected
  without exposing database or tapd types to swapd.
- [x] (2026-07-21 20:03Z) Extended the public intent additively with receiver
  asset units and an
  explicit asset-change carrier; retain a legacy full-send default.
- [x] (2026-07-21 20:03Z) Derived asset and Bitcoin change before the first
  external commit, and
  persist the resulting scripts and deterministic ordering state.
- [x] (2026-07-21 20:03Z) Built one checkpoint per input with exactly one asset
  package and empty
  positional slots for Bitcoin-only checkpoints.
- [x] (2026-07-21 20:03Z) Stabilized canonical output indices using tap-sdk
  preview, composed Ark
  policies with the previewed roots, and reject preview/commit divergence.
- [x] (2026-07-21 20:03Z) Resolved chained proof sources from the exact locally
  created package and
  distinguish initial managed-inventory verification from chained lineage
  verification.
- [x] (2026-07-21 20:03Z) Added amount, carrier, mixed-input, output-ordering,
  restart,
  reconciliation, proof-lineage, and response-filtering tests.
- [x] (2026-07-21 20:03Z) Added synchronous durable carrier selection, an
  atomic reservation-set acquisition and ownership handoff, retained
  post-commit quarantine, and restart adoption of only still-Spending inputs.
- [x] (2026-07-28 17:10Z) Regenerated RPC and SQLC artifacts during
  implementation; ran formatting, changed-source lint with zero findings,
  focused tests, focused race tests, the full unit suite including `baselib`,
  and the debug build. All completed successfully.
- [x] (2026-07-28 17:15Z) Created the signed implementation commit. Publishing
  remains a separate review-stack action because the root prototype PR is
  currently conflicted with `main`.

## Surprises & Discoveries

- Observation: a receiver's tapd normally does not list the sender's confirmed
  base anchor as a locally managed UTXO.
  Evidence: the current managed-inventory verifier requires every passive
  anchor to appear in `ListUtxos`, so reusing it for a sealed OP_TRUE chain
  would reject a valid two-wallet transfer. Chained spends need the exact
  operator-accepted package lineage as their passive-isolation trust root while
  still calling `VerifyProof` for the confirmed base.
- Observation: Taproot Asset output roots commit to their Bitcoin anchor output
  indices, while Wavelength canonically sorts recipient outputs by the final
  composed P2TR script.
  Evidence: assigning an index changes the Taproot Asset root; that root changes
  the P2TR script; the changed script can change the canonical index. The
  builder therefore needs a bounded fixed-point loop and cycle handling rather
  than one sorting pass.
- Observation: deriving a fresh change policy advances wallet keys and cannot
  happen again after an outcome-unknown tapd commit.
  Evidence: reconstructing change after restart can change the request digest
  and txid. All change recipients and the ordering nonce must be durable before
  the first external commit.
- Observation: tap-sdk PR #166 adds the missing read-only output preview.
  Evidence: commit `d263d8c2` exposes
  `CustomAnchorPlan.PreviewOutputCommitments`, allowing Wavelength to stabilize
  output indices without mutating tapd state.
- Observation: the two virtual transitions need more proof capacity than the
  previous exact-send estimate.
  Evidence: the builder reserves two transition leaves plus serialization
  headroom before either tapd commit, and rejects an undersized source proof
  during preflight.
- Observation: optimistic VTXO selection is too weak for an external tapd
  mutation boundary.
  Evidence: an asset preparation could otherwise reserve rows before each
  child VTXO actor had durably entered Spending. Asset sends now await those
  actor writes; ordinary Bitcoin OOR sends retain detached admission.
- Observation: per-input reservation writes leave a crash and exit race.
  Evidence: preparation now acquires the complete set atomically and OOR actor
  admission atomically hands that exact set to the session owner while every
  VTXO remains Spending. Absent, partial, mixed, stale, and foreign sets fail
  closed.

## Decision Log

- Decision: keep `asset_amount` as the expected total units in the selected
  input and add `recipient_asset_amount` plus
  `asset_change_carrier_value_sat` to the public intent.
  Rationale: old callers can retain full-send behavior, while partial sends
  state both asset allocation and carrier allocation explicitly. Receiver
  carrier sats remain the existing recipient `amount_sat`.
  Date/Author: 2026-07-21 / Codex.
- Decision: require explicit carrier sats for every asset-bearing output and
  use ordinary Bitcoin VTXOs to cover a shortfall.
  Rationale: carrier satoshis are Bitcoin transaction value, not asset units or
  a hidden fee. This keeps the prototype honest and makes coin selection and
  insufficient-funds errors understandable.
  Date/Author: 2026-07-21 / Codex.
- Decision: derive asset change inside the preparer and expose only the
  caller's receiver outpoint in `SendOORResponse`.
  Rationale: wallet change is local implementation state, not another payment
  recipient. Returning it as a sent outpoint would be both confusing and
  unsafe for swap orchestration.
  Date/Author: 2026-07-21 / Codex.
- Decision: use one checkpoint per selected VTXO and keep the existing sparse
  positional asset-package container.
  Rationale: operator validation and restart recovery already bind each
  package slot to the same checkpoint index; an empty slot unambiguously means
  a Bitcoin-only edge.
  Date/Author: 2026-07-21 / Codex.
- Decision: both asset receiver and asset change use the Taproot Asset OP_TRUE
  virtual script mode while the Ark Bitcoin policy controls spend ownership.
  Rationale: the asset proof follows the Ark policy-composed anchor, and the
  caller already signs the Bitcoin path. A separate recipient asset script key
  would duplicate ownership and complicate chained proof witnesses.
  Date/Author: 2026-07-21 / Codex.
- Decision: use deterministic bounded preview retries with cycle detection and
  an ordering nonce.
  Rationale: output-index/root/script feedback may not converge for one set of
  derived keys. Retrying a deterministic alternate OP_TRUE key set is safe only
  when the chosen nonce and recipients are persisted before commit.
  Date/Author: 2026-07-21 / Codex.
- Decision: initial onboarding spends and chained package spends have distinct
  proof-verification modes.
  Rationale: an initial spend can require complete local tapd inventory and
  backend signing. A chained spend instead verifies the confirmed base proof,
  validates the exact persisted package lineage, and uses the caller-known
  OP_TRUE witness; requiring foreign inventory would break receiver custody.
  Date/Author: 2026-07-21 / Codex.
- Decision: derive the temporary preparation reservation owner from a
  domain-separated hash of the non-empty public idempotency key, then hand the
  complete set to the outgoing session in its actor commit transaction.
  Rationale: both the preparer and durable actor can reconstruct that identity
  without widening the public wire. Safety comes from the journal's exact
  intent/input digest and the store's exact-set, exact-owner, and Spending
  preconditions; the hash is an identity, not a secret capability.
  Date/Author: 2026-07-21 / Codex.
- Decision: retain all selected inputs after atomic preparation ownership has
  been acquired if a later preparation step fails.
  Rationale: after tapd may have been mutated, best-effort per-input release
  can create an unrecoverable partial set. The prototype instead returns an
  explicit reconciliation error and preserves the complete quarantine.
  Date/Author: 2026-07-21 / Codex.

## Outcomes & Retrospective

The Wavelength implementation is complete and locally validated. It provides
explicit receiver/change carriers, partial and full sends, mixed asset/Bitcoin
inputs, deterministic output stabilization, exact sealed-package lineage,
durable restart state, and atomic input quarantine/adoption. It does not hide
carrier sats or exchange asset units for Bitcoin.

Two limits remain deliberate for the prototype. Chained verification trusts
the exact locally persisted, operator-accepted package as the lineage root
after tapd verifies its confirmed base proof; it cannot independently prove
that a foreign confirmed base had no hidden passive assets. Existing v0/v1
preparation journals have no migration to v2 and should be reset in a PoC
environment. Cross-repository operator validation and the live two-wallet
regtest remain separate stacked Lumos milestones.

## Context and Orientation

A VTXO is Wavelength's spendable virtual Bitcoin output. Its
`vtxo.Descriptor.Amount` is satoshis. An asset-bearing descriptor additionally
stores a Taproot Asset reference, an unsigned 64-bit asset amount, and a
32-byte commitment root. These quantities must never be added together.

The public request lives in `waverpc/daemon.proto` as
`TaprootAssetOORIntent`. `waved/rpc_oor_taproot_asset.go` validates and maps
that request. `waved/rpc_server.go` selects the mandatory asset input plus any
ordinary Bitcoin inputs and calls `oor.TaprootAssetOORPreparer`.

`oor/taproot_asset_preparer.go` is the SDK-neutral boundary. It receives
selected descriptors and one caller receiver, derives durable change through
an injected wallet-facing builder, and returns an immutable prepared package
to the ordinary durable OOR actor. The actor must never contact tapd inside its
database transaction.

`tapassets/preparer.go` is the only transaction builder allowed to use
tap-sdk. A checkpoint transaction spends one selected VTXO and creates one
collaborative output plus a zero-valued pay-to-anchor output. The Ark
transaction spends every checkpoint output into canonically ordered receiver
and change outputs and appends its pay-to-anchor output last.

The sealed container `lib/tx/oor.TaprootAssetTransfer` has one checkpoint slot
per selected input. Exactly one slot is non-empty in this prototype. The Ark
package is always present. `db.OORArtifactPersistenceStore` can retrieve the
exact package that created a given output, and
`tapassets.ResolveCreatedAssetProofSource` reconstructs its compact proof path
and OP_TRUE witness after restart.

`waved/taproot_assets_runtime.go` is the composition root: it has access to
wallet change derivation and the local artifact store, but can inject narrow
interfaces into `tapassets` without giving swapd database handles, tapd
credentials, signing keys, or SDK types.

## Plan of Work

First edit `waverpc/daemon.proto` additively. Add
`recipient_asset_amount = 9` and
`asset_change_carrier_value_sat = 10` to `TaprootAssetOORIntent`, regenerate
all protobuf outputs with `make rpc`, and update the CLI/domain mapping tests.
A zero receiver asset amount temporarily means the legacy full-send default.
Validation requires a positive amount no greater than `asset_amount`; a
partial send requires asset-change carrier at or above the operator's VTXO
floor, and a full send requires it to be zero. All satoshi sums use overflow
safe arithmetic and stay within `btcutil.MaxSatoshi`.

Next extend `oor.TaprootAssetOORPrepareRequest` with the operator output floor
and change-building seam. The request initially contains exactly the caller's
receiver. The preparer computes remaining asset units, derives an OP_TRUE asset
change policy when needed, and derives ordinary Bitcoin change for residual
satoshis. Persist the complete pre-root recipients, policy templates, scripts,
and ordering nonce before the first tapd commit. A restored request reuses
those exact bytes and never advances wallet keys again.

Then generalize `tapassets/preparer.go` from exact one-input funding to a mixed
graph. Build one checkpoint per input, identify exactly one authoritative asset
descriptor, commit only that checkpoint through tap-sdk, and leave empty
package slots at Bitcoin checkpoint positions. Rebuild the Ark transaction
from all committed checkpoint outpoints. Map signing plans by each checkpoint
outpoint's actual index in the canonically sorted Ark inputs, not by transfer
input position, and require one plan per Ark input.

Pin tap-sdk commit `d263d8c2` and add a preview method to Wavelength's private
`customAnchorDriver`. Assign stable logical IDs to receiver and asset change,
preview output commitments, compose their Ark policies with the previewed
roots, recompute canonical indices, and repeat until stable. Detect cycles.
For a cycle, derive the next deterministic OP_TRUE key set from a bounded
ordering nonce and retry. Re-preview the stable request immediately before
commit, then require the committed PSBT, output roots, indices, values, and
scripts to match that preview exactly. Never target the pay-to-anchor output.

Finally inject an exact created-package loader from
`waved/taproot_assets_runtime.go`. For an onboarded input, retain full managed
inventory verification and backend signing. For a locally created input,
load only the package with a created-output binding, reconstruct its compact
path and OP_TRUE witness, verify the confirmed base proof with tapd, validate
every persisted package step, and append the new checkpoint proof. Persist
commit-attempt markers before every external mutation and restore a fully
committed preparation without further tapd calls.

## Concrete Steps

Work from:

    cd /Users/dario/dev/lightninglabs/.worktrees/wavelength-partial-assets

After editing `waverpc/daemon.proto`, regenerate code:

    make rpc

Run focused tests while implementing:

    go test ./lib/tx/oor ./tapassets ./oor ./waved ./waverpc
    go test -race ./tapassets ./oor

Before the implementation commit:

    make fmt-changed
    make fmt-changed-check
    make unit
    make build
    make lint-changed-local
    make tidy-module-check
    make commitmsg-lint range="origin/main..HEAD"

## Validation and Acceptance

The primary test starts with one VTXO containing 1,000 asset units and explicit
carrier satoshis, requests 800 units for the receiver, and requests a positive
carrier for the 200-unit asset change. It supplies a Bitcoin-only VTXO when
the asset carrier is insufficient. The committed graph must contain one asset
checkpoint package in the exact selected-input slot, empty package slots for
Bitcoin checkpoints, an 800-unit receiver, a 200-unit change output, optional
Bitcoin change, and pay-to-anchor last. The response contains only the
receiver outpoint.

Additional tests cover a full asset send with no asset change; exact, excess,
and insufficient carrier funding; receiver and change carriers at and below
the operator floor; asset and satoshi overflow; the asset checkpoint at a
non-zero input position; Ark input reordering; OP_TRUE asset outputs and an
unrooted Bitcoin change output; preview index flips; stable convergence;
cycle nonce retry and exhaustion; preview/commit mismatch; exact created
package selection where an outpoint has both created and consumed bindings;
restart without fresh key derivation; ambiguous commit reconciliation; a
fully committed restart with no tapd calls; and a receiver-side chained spend
whose tapd does not list the sender's base anchor.

Bitcoin-only OOR behavior must remain unchanged. Request, proof, source,
capacity, and recipient-policy preflight failures occur before tapd mutation.
After atomic preparation ownership, every failure retains the complete input
set for deterministic retry or reconciliation rather than releasing a
possibly asset-committed input.

## Idempotence and Recovery

All protobuf additions are optional and additive. A legacy caller that omits
the receiver asset amount performs the historical full send. Prepared change
scripts and the ordering nonce are durable; retry and restart reuse them
byte-for-byte. A tapd commit attempt is journaled before the RPC. Success
replaces the marker with the sealed package. An outcome-unknown error keeps the
input reservation and requires reconciliation rather than a blind competing
commit. Once every sealed package exists, restoration performs no external
commit.

Selection waits until every carrier VTXO has durably entered Spending before
the preparation store can atomically acquire the exact set. The reservation
owner is derived from the idempotency key. Durable actor admission changes the
complete set to the deterministic OOR session owner in the same transaction
that checkpoints the actor; it never inserts missing rows or steals partial or
foreign sets. Restart adoption requires every journaled descriptor to remain
Spending and accepts either the complete preparation owner or the complete
already-handed-off session owner. State-file replacement also syncs the parent
directory so a successful rename survives a crash boundary.

The preparation journal format is version 2 and intentionally has no migration
from earlier unpublished prototype versions. PoC deployments carrying an older
journal must clear that state before upgrade.

The tap-sdk dependency is pinned to the pseudo-version containing merged PR
#166. Move to the next tagged tap-sdk release through a separate dependency-only
commit when available; do not substitute a local `replace` directive.

## Artifacts and Notes

No Wavelength product issue is opened for this PoC. Product-policy findings
remain in local design notes until the prototype is evaluated. A tap-sdk or
taproot-assets issue may be opened only for a demonstrated upstream gap. The
live two-tapd test determines whether `VerifyProof` accepts a confirmed base
proof that the receiving tapd has not imported.

Revision note (2026-07-21): initial plan written after auditing carrier
allocation, output-index feedback, durable change derivation, sparse package
slots, and receiver-side proof lineage. The asset-state branch was restacked
on runtime registration so the implementation can inject exact package lookup
without widening subsystem ownership.

## Interfaces and Dependencies

`waverpc.TaprootAssetOORIntent` additively exposes
`RecipientAssetAmount uint64` and `AssetChangeCarrierValueSat uint64`.
`oor.TaprootAssetOORPrepareRequest` exposes `OutputFloor btcutil.Amount` and
passes one caller receiver. `tapassets.PreparerConfig` receives a narrow
change-recipient builder and exact created-package loader. The internal
`customAnchorDriver` exposes a read-only preview operation backed by
`CustomAnchorPlan.PreviewOutputCommitments`.

`oor.ReservationSetStore` atomically acquires, inspects, and hands off complete
carrier sets. Asset wallet selection sets `WaitForDurable`; Bitcoin-only OOR
selection preserves its existing detached path. The preparation resumer
returns exact journaled outpoints for restart adoption without selecting a new
carrier set.

The only new upstream dependency is tap-sdk commit
`d263d8c2c4005a1037277b9202cb2bd28a14fb0c` from PR #166. Lumos, swapd, and
the public OOR wire remain SDK-neutral and receive no tapd credentials.
