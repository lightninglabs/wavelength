# Persist asset state and mixed OOR package bindings

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to
date as work proceeds. This document is maintained in accordance with
`PLANS.md` at the repository root.

## Purpose / Big Picture

After this change, Wavelength can remember which Taproot Asset and how many
asset units a virtual transaction output (VTXO) carries without changing the
meaning of its Bitcoin amount. The existing `amount_sat` remains the explicit
carrier-satoshi value. A separate nested asset object returned by `ListVTXOs`
contains the opaque SDK-level asset reference, the full unsigned 64-bit asset
amount, and the 32-byte commitment root.

The durable out-of-round (OOR) package format will also represent a graph that
contains one asset-bearing checkpoint and zero or more ordinary Bitcoin
checkpoints. Its checkpoint-package slice remains positional: every slot maps
to the checkpoint at the same index, a non-empty slot is a sealed Taproot Asset
transition, and an empty slot is a Bitcoin-only edge. Historical v0 containers
whose slots are all non-empty remain byte-for-byte readable.

Finally, a caller preparing the next asset spend can ask the database for the
package that created an exact VTXO and can ask the `tapassets` adapter to derive
a restart-stable compact proof path and OP_TRUE asset witness from that sealed
package. This milestone does not yet construct a partial asset send. It builds
the SDK-neutral persistence, wire, and proof-source substrate that the next
stacked transaction-builder branch will consume.

The behavior is observable in focused tests: a descriptor containing
`math.MaxUint64` asset units survives SQLite storage and `ListVTXOs`; mixed
asset/Bitcoin package slots survive encoding and reject ambiguous shapes; a
prepared graph accepts an asset input beside zero, one, or several Bitcoin
inputs; an outpoint with both created and consumed bindings resolves only its
created package; and decoding the same stored package before and after a
simulated restart returns identical proof-path and witness bytes.

## Progress

- [x] (2026-07-21 17:54Z) Audited the existing descriptor, migrations,
  generated SQL, protobuf surfaces, OOR snapshots and durable TLVs, incoming
  materialization, package bindings, and tap-sdk package projection.
- [x] (2026-07-21 17:54Z) Created
  `feat/taproot-assets-asset-state` above
  `feat/taproot-assets-carrier-selection` and wrote this living plan.
- [x] (2026-07-21 18:24Z) Added SDK-neutral asset identity and amount to
  descriptor persistence and
  the public VTXO projection, including a full-`uint64` database encoding.
- [x] (2026-07-21 18:24Z) Propagated asset metadata through onboarding,
  operator signing descriptors, recipient wire messages,
  OOR snapshots and actor messages, and incoming VTXO materialization.
- [x] (2026-07-21 18:24Z) Generalized the v0 sealed asset container and
  prepared-submit validation
  for positional Bitcoin-only checkpoint slots and Bitcoin-only recipients.
- [x] (2026-07-21 18:24Z) Added exact created-output package lookup and the
  tapassets proof-source
  resolver/projection.
- [x] (2026-07-21 18:56Z) Regenerated SQL/protobuf output, added
  compatibility and restart tests, and completed formatting, focused and full
  unit tests, focused race tests, build, and changed-file lint validation.

## Surprises & Discoveries

- Observation: the current v0 asset container already has the right positional
  shape, but its package reader and writer reject zero-length checkpoint slots.
  Evidence: `lib/tx/oor/asset_transfer.go` writes a length prefix per
  checkpoint and `readTaprootAssetPackage` rejects length zero, so mixed graphs
  need no new field or version, only explicit empty-slot semantics.
- Observation: an OOR outpoint can legitimately have both a
  `created_output` binding and, after it is spent, a `consumed_input` binding.
  Evidence: `db/sqlc/queries/oor_artifacts.sql` already has a kind-filtered
  package query, while `OORArtifactPersistenceStore.GetPackageForOutpoint`
  uses the unfiltered query and therefore cannot express proof-source intent.
- Observation: tap-sdk's sealed package already retains every item needed to
  derive a later spend source, but Wavelength's current `commitResult`
  projection discards most mappings and the input `ProofSource`.
  Evidence: `tapassets/driver.go` currently projects only input outpoint,
  asset reference, and amount, and projects output proof bytes by an
  outpoint/script-key search. The upstream package additionally exposes stable
  logical IDs, packet and virtual indices, anchor indices, proof-source kind
  and bytes, script mode, and the exact OP_TRUE witness.
- Observation: the Docker daemon was unavailable, so the repository wrappers
  could not generate SQL or protobuf output. The same pinned tools were run
  locally: sqlc 1.29.0, protoc 3.21.12, protoc-gen-go 1.36.11,
  protoc-gen-go-grpc 1.5.1, and grpc-gateway 2.29.0. The merged SQL schema and
  all protobuf packages were regenerated successfully.
- Observation: generic seed recovery still queries the indexer with the
  ordinary receive pkScript. An asset-bearing VTXO instead commits to the
  composed Ark-policy and asset root, so metadata hydration alone cannot make
  seed recovery discover those scripts. Recovery needs an indexer query by
  owner identity or another asset-aware inventory primitive and remains an
  explicit follow-up.
- Observation: an incoming recipient event can locally bind its root to the
  announced P2TR script and can enforce complete, bounded ref/amount metadata,
  but the SDK-neutral OOR package layer cannot prove that the opaque ref and
  amount correspond to that root. For this PoC, the operator is the
  authoritative validator of the sealed Ark package before publishing these
  fields. A later trust-minimized receiver path can inject the tapassets
  projection at the wallet boundary.

## Decision Log

- Decision: store `taproot_asset_amount` as an optional exactly eight-byte
  big-endian BLOB, not SQL `BIGINT`.
  Rationale: Taproot Asset amounts are `uint64`; both SQLite integer values and
  Go's generated SQL integer fields are signed 64-bit. A fixed-width byte
  encoding preserves `math.MaxUint64`, has one canonical representation, and
  distinguishes historical metadata absence (`NULL`/empty) from a value.
  Date/Author: 2026-07-21 / Codex.
- Decision: keep mixed checkpoint slots in TaprootAssetTransfer v0 rather than
  introduce a sparse v1 map.
  Rationale: the existing count and order already bind each slot to one
  checkpoint. Accepting empty slots is backward compatible for every old
  all-nonempty v0 payload, minimizes cross-repository churn, and keeps lookup
  constant-time. Older binaries will reject newly mixed payloads, so
  Wavelength and Lumos must deploy this feature together.
  Date/Author: 2026-07-21 / Codex.
- Decision: allow historical asset-root-only descriptors to load, but require
  new metadata producers to write asset reference and positive amount
  together.
  Rationale: migration 16 and already-persisted PoC outputs know only the
  commitment root. Rejecting them would break existing databases. New
  onboarding and OOR materialization paths have the identity and amount, so
  silently producing another incomplete descriptor would be a bug.
  Date/Author: 2026-07-21 / Codex.
- Decision: expose a dedicated created-output lookup rather than add a caller
  parameter to the historical unfiltered lookup.
  Rationale: existing unroll callers may intentionally resolve either link;
  proof-source reconstruction specifically means “the package that created
  this state” and should be impossible to call ambiguously.
  Date/Author: 2026-07-21 / Codex.
- Decision: keep tap-sdk package decoding and compact proof-path construction
  inside `tapassets` and return a narrow Wavelength projection.
  Rationale: database, OOR, RPC, and operator surfaces must remain independent
  of tapd and tap-sdk implementation types. The adapter can validate upstream
  mappings once and return opaque proof bytes, plain strings and integers,
  `wire.OutPoint`, and cloned witness stacks.
  Date/Author: 2026-07-21 / Codex.
- Decision: carry canonical asset reference and amount on both recipient
  outputs and operator signing descriptors.
  Rationale: Lumos must bind the sealed package to the exact consumed and
  created states; root-only signing metadata would leave identity and quantity
  as unauthenticated hints. The client therefore sources these fields from the
  validated package-backed descriptor and preserves them through every durable
  retry boundary.
  Date/Author: 2026-07-21 / Codex.

## Outcomes & Retrospective

The implementation now persists full-width asset quantities separately from
carrier sats, carries canonical SDK-neutral metadata through onboarding,
operator, recipient, actor, snapshot, and incoming-materialization boundaries,
accepts mixed Bitcoin/asset checkpoint graphs in the v0 positional package,
and reconstructs a bounded compact proof path plus OP_TRUE witness from the
exact created-output package. Historical root-only rows/messages and
all-nonempty v0 packages remain readable. Formatting, focused and full unit
tests, focused race tests, build, and changed-file lint all pass. The live
Lumos cross-repository test and asset-aware seed recovery remain follow-ups.

## Context and Orientation

A VTXO is Wavelength's spendable virtual Bitcoin output. Its
`vtxo.Descriptor.Amount` and the public `waverpc.VTXO.amount_sat` describe the
Bitcoin satoshis carried by the output. Taproot Asset units are a separate
quantity and must never be added to or substituted for those satoshis.

`vtxo/interfaces.go` defines the canonical descriptor. `db/vtxo_store.go`
maps it to the `vtxos` table. SQL migration sources live under
`db/sqlc/migrations`, and query sources live under `db/sqlc/queries`; generated
files under `db/sqlc` must only be changed by `make sqlc` or the repository's
exact pinned local generator. Migration 16 added `taproot_asset_root`, and this
branch appends migration 18 for the asset reference and amount. The nested
public asset projection belongs in `waverpc/daemon.proto` and is populated by
`waved.descriptorToProto`.

An OOR transfer has one checkpoint transaction per selected input and one Ark
transaction that spends every checkpoint into recipient outputs. The shared
container `lib/tx/oor.TaprootAssetTransfer` stores sealed tap-sdk packages while
keeping them opaque outside the `tapassets` package. `oor.PreparedSubmitPackage`
binds that container to concrete transfer inputs, checkpoints, and recipients.
For a mixed graph, root presence on input `i` must exactly equal package-slot
presence at index `i`; Bitcoin-only inputs have neither.

Recipient metadata crosses several durable boundaries. The canonical domain
type is `lib/tx/oor.RecipientOutput`; `rpc/oorpb/oorwire.proto` carries it to
the operator; `arkrpc/indexer.proto` carries it back in recipient and VTXO
events; `oor/actor_durable_message.go` and
`oor/outgoing_snapshot_codec.go` retain it across actor restarts; and
`oor/local_persistence_handler.go` plus `oor/incoming_vtxo.go` materialize the
descriptor. New TLV record numbers must be append-only so historical actor
messages and snapshots continue to decode with zero-valued optional fields.

`db.OORArtifactPersistenceStore` stores sealed packages and outpoint
bindings. A created-output binding links a local VTXO to the package whose Ark
transaction created it; a consumed-input binding links the same outpoint to a
later package that spent it. `GetOORPackageByOutpointAndKind` already exists in
generated SQL and is the correct primitive for an unambiguous
`GetCreatedPackageForOutpoint` method.

The only package allowed to import tap-sdk is `tapassets`. A sealed
`tapsdk.CustomAnchorTransferPackage` contains input proof sources, output
logical and virtual mappings, transition proof updates, script modes, and
OP_TRUE witness data. The new resolver validates that package, finds the exact
output by anchor outpoint plus asset identity and amount, requires a unique
matching proof update, reconstructs an `AssetProofPath` from either a confirmed
proof file or an existing compact path, appends the output transition, and
returns cloned opaque bytes and witness elements.

## Plan of Work

First, append migration 18 with nullable `taproot_asset_ref TEXT` and
`taproot_asset_amount BLOB` columns. Extend the VTXO insert/upsert query and all
generated projections. Add documented encode/decode helpers in
`db/vtxo_store.go` that only emit eight-byte big-endian amounts and reject any
other non-empty length. Extend `vtxo.Descriptor` and the cache-safe row mapping.
Add a nested `TaprootAssetVTXO` message to `waverpc/daemon.proto`, leaving
`amount_sat` unchanged, and populate it only when a descriptor has a root.

Next, carry reference, amount, and root through every asset recipient path.
Extend `RecipientOutput`, `ArkRecipientOutput`, OOR protobuf recipient fields,
indexer recipient/VTXO event fields, recipient TLV payloads, outgoing
snapshots, incoming events, cloning helpers, and descriptor construction.
Append TLV record types without renumbering existing fields. Extend
`TransferInputSnapshot` so a restart retains the selected descriptor's asset
metadata. Add asset identity and amount to `tapassets.OnboardingResult` and
materialize them in the direct-on-chain descriptor. All new metadata must use
plain strings, byte arrays, and integers outside `tapassets`.

Then change `TaprootAssetTransfer.Validate`, marshal, and unmarshal so empty
checkpoint slots are legal only as Bitcoin-only placeholders. The slice must
remain non-empty, must equal the expected checkpoint count, must contain at
least one non-empty slot, and must have a non-empty bounded Ark package. Keep
the checksum and v0 binary envelope unchanged. Update
`PreparedSubmitPackage.Validate` so each input root is present exactly when its
slot is non-empty. Validate asset commitments only for recipients with roots;
allow ordinary Bitcoin recipients and require at least one asset-bearing
recipient.

Add `GetCreatedPackageForOutpoint` to `db.OORArtifactPersistenceStore`. Reuse
the existing kind-filtered query inside one read transaction and materialize
the matching binding exactly as the historical method does. Leave
`GetPackageForOutpoint` unchanged for compatibility. In `tapassets/driver.go`,
retain all upstream logical IDs, packet roles and indices, anchor indices,
input proof source fields, output script mode, and exact proof-update mapping.
Add a narrow resolver that converts the sealed Ark package and exact created
outpoint into a validated, restart-stable proof source and OP_TRUE witness.

Finally, add tests at each boundary. Cover mixed and historical all-nonempty v0
container round trips, all-empty/count/size corruption, prepared asset plus
zero/one/many Bitcoin inputs and recipients, database max-`uint64` and invalid
BLOB lengths, onboarding and incoming metadata propagation, OOR proto and TLV
round trips with old-field omission, `ListVTXOs`, created-versus-consumed
binding ambiguity, resolver mismatch/duplicate/missing update failures, and
restart byte equality. Regenerate code, format changed files, and run focused
tests and their race variants before the full unit/build/lint gates.

## Concrete Steps

Work from:

    cd /Users/dario/dev/lightninglabs/.worktrees/wavelength-carrier-funding

After SQL and protobuf source edits, regenerate rather than editing generated
files:

    make sqlc
    make rpc

If those Docker wrappers cannot connect to the Docker daemon, inspect the
Makefile/tool manifests and run the exact pinned local sqlc and protobuf tools.
Record the versions and reason in `Surprises & Discoveries`.

Format and run focused packages throughout:

    make fmt-changed
    go test ./lib/tx/oor ./db ./vtxo ./rpc/oorpb ./oor ./tapassets ./waved
    go test -race ./lib/tx/oor ./db ./vtxo ./rpc/oorpb ./oor ./tapassets ./waved

Before the implementation commit, run:

    make unit
    make build
    make lint-changed-local
    make commitmsg-lint range="origin/main..HEAD"

## Validation and Acceptance

Acceptance requires all focused and repository tests to pass and the following
behaviors to be pinned by tests. A descriptor with an asset reference, root,
and `math.MaxUint64` amount must save and load exactly and must appear through
`descriptorToProto` with the satoshi carrier value unchanged. An old row with
only an asset root must still load and list with a present asset object whose
reference and amount are absent/zero.

A v0 transfer with slots `[asset-package, empty, empty]` must marshal and
unmarshal exactly, while `[empty, empty]`, a checkpoint-count mismatch, an
empty Ark package, an oversized package, a bad checksum, and trailing bytes
must fail. Historical `[asset-package, asset-package]` bytes must continue to
decode. Prepared-submit tests must accept every supported mixed input/output
shape and reject every disagreement between input roots and package slots.

When one outpoint has a created binding to package A and a consumed binding to
package B, `GetCreatedPackageForOutpoint` must always return A and report the
created binding. The proof resolver must derive one compact path with the
package output's transition proof, return the exact OP_TRUE witness, reject a
wrong outpoint/ref/amount, ambiguous or missing proof updates, and a non-OP_TRUE
output, and return byte-identical results after reloading the same stored
package.

## Idempotence and Recovery

Migration 18 is additive and safe to retry through the repository migrator.
All historical rows decode because both columns are nullable. SQL regeneration
is deterministic. Protobuf and TLV fields are append-only, so omitted fields
decode to empty values and old messages remain accepted.

Package and proof-source methods do not mutate stored or caller-owned byte
slices. Every returned blob and witness stack is cloned. Re-running a focused
test or decoding the same sealed package after restart therefore produces the
same bytes without external tapd calls.

## Artifacts and Notes

This branch is stacked on commit `dda6a523`, the carrier-selection milestone.
The next branch will use this substrate to build a partial asset transfer such
as 1,000 units to 800 receiver plus 200 change, funded by explicit carrier
satoshis. The cross-repository live test belongs in Lumos once its validator
understands the same positional empty-slot semantics.

## Interfaces and Dependencies

At completion, `vtxo.Descriptor` has `TaprootAssetRef string`,
`TaprootAssetAmount uint64`, and the existing
`TaprootAssetRoot *chainhash.Hash`. `waverpc.VTXO` has an optional nested
asset message; its existing `amount_sat` contract is unchanged.

`lib/tx/oor.TaprootAssetTransfer.CheckpointPackages` remains `[][]byte`, but
an empty element has the defined meaning “the checkpoint at this index is
Bitcoin-only.” `oor.PreparedSubmitPackage.Validate` enforces package-slot and
input-root equivalence and mixed recipients.

`db.OORArtifactPersistenceStore.GetCreatedPackageForOutpoint` returns the
existing SDK-neutral `OORPackageBundle`. The new `tapassets` resolver accepts
sealed package bytes plus SDK-neutral output identity and returns a projection
containing compact proof-path bytes, asset reference and amount, anchor output
index/outpoint, stable logical and packet mappings, and a cloned OP_TRUE
witness. No non-`tapassets` package imports tap-sdk or taproot-assets.
