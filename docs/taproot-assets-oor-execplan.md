# Taproot Assets in Wavelength OOR Transfers

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to
date as implementation proceeds.

This document follows `PLANS.md` at the repository root.

## Purpose / Big Picture

Wavelength should be able to move a Taproot Asset through an out-of-round
(OOR) Ark transfer without treating the Bitcoin graph and the asset graph as
independent. A caller should be able to prepare an asset-bearing OOR package,
persist the exact Taproot Assets recovery packages, submit it through the
existing durable OOR actor, and resume without changing any committed txid.
The first vertical slice is deliberately proof-selected and caller-funded. It
uses tap-sdk's custom-anchor builder and does not attempt wallet coin selection,
Lightning asset invoices, or a complete asset-aware swap product.

The observable result is an optional asset extension on the existing OOR
package. Bitcoin-only sends remain byte-for-byte compatible. Asset sends first
commit every VTXO-to-checkpoint transition, rebuild the Ark transaction from
the committed checkpoint outpoints, commit the checkpoint-to-recipient
transition, persist both layers, and only then request Ark signatures.

## Progress

- [x] (2026-07-15 10:30Z) Audited the merged tap-sdk custom-anchor API, the
  historical `darepo-client#12` builder, and Wavelength's durable OOR FSM.
- [x] (2026-07-15 10:30Z) Created an isolated feature worktree based on the
  Wavelength rename branch.
- [x] (2026-07-14 23:00Z) Added the shared, versioned OOR asset package,
  recipient/input root binding, and durable input/start-message root codecs.
- [x] (2026-07-14 23:20Z) Added additive submit and offline-receive protobuf
  fields for input/output roots and the opaque asset transfer, plus bounded
  request codecs and transport round-trip tests.
- [ ] Add tap-sdk-backed two-transition preparation with exact PSBT/root
  binding and deterministic graph verification.
- [x] (2026-07-14 23:45Z) Added a prepared-package entry point to the durable
  outgoing OOR FSM; committed PSBTs, canonical recipients, roots, and sealed
  packages survive start-message and snapshot restore before signing.
- [x] (2026-07-15 01:45Z) Threaded recipient roots and sealed transfer
  packages through offline receive, durable event/snapshot codecs, incoming
  VTXO materialization, and the OOR artifact store without changing the
  Bitcoin-only path.
- [x] (2026-07-15 01:35Z) Persisted the optional asset root on VTXO
  descriptors, excluded asset rows from generic Bitcoin coin selection, and
  made OOR signing, forfeit signing, and unilateral timeout exits derive the
  composed control block.
- [ ] Expose the smallest daemon/SDK surface needed by swapd to submit the
  prepared transfer.
- [ ] Add unit, codec, FSM, transport, tamper, and restart tests; run changed
  lint, package tests, and the repository build.

## Surprises & Discoveries

- Observation: Wavelength uses btcd's `/v2` PSBT and wire packages while
  tap-sdk intentionally exposes serialized PSBT bytes and SDK-owned DTOs.
  Evidence: Wavelength imports `github.com/btcsuite/btcd/psbt/v2`; tap-sdk's
  `CustomAnchorRequest.AnchorPSBT` and transfer package use byte slices. The
  integration boundary must therefore be serialization, not shared PSBT Go
  types.
- Observation: `lib/arkscript.ComposeWithSiblingRoot` already implements the
  host-policy plus Taproot Asset root composition and control-block extension
  needed by this work, but no production OOR path currently uses it.
- Observation: tap-sdk is currently based on taproot-assets v0.8.0 while this
  rename branch pins a v0.7.1 development commit. Adding tap-sdk may upgrade
  the module graph; the build and targeted regression suite must prove that
  this is safe or the dependency alignment must be handled upstream.
- Observation: current tapd can commit an unconfirmed compact path but cannot
  publish/log that path through chain porter. The OOR slice must persist the
  path and leave confirmation materialization/publish to a later boundary.
- Observation: the merged tap-sdk cannot currently be imported by Wavelength.
  Wavelength selects btcd v0.26 and its `/v2` modules, while tap-sdk still
  imports classic root-module `wire`, `txscript`, and `chaincfg` packages that
  are absent at v0.26. Evidence: adding tap-sdk commit `932b4aa` and compiling
  the adapter failed during package loading; the exact reproduction is filed
  as `lightninglabs/tap-sdk#163`.
- Observation: the first SDK-neutral sender slice derived composed spend paths
  on `TransferInput`, but the production checkpoint signer still rebuilt the
  historical Bitcoin-only control block from `Descriptor.TapScript`.
  Evidence: the new asset-signing regression failed until checkpoint signing,
  forfeit signing, and timeout exit were routed through root-aware spend-info
  derivation.
- Observation: retaining the sealed transfer only in the receive-session
  snapshot is insufficient because the successful session advances to ack and
  completion. The package must live beside the finalized OOR artifacts, while
  the 32-byte root lives beside each owned VTXO.

## Decision Log

- Decision: implement assets as an optional OOR extension rather than a second
  OOR protocol. Rationale: locking, co-signing, finalize, idempotency, and
  recovery remain Bitcoin graph concerns and should reuse the hardened FSM.
  Date/Author: 2026-07-15 / Codex.
- Decision: use one sealed tap-sdk transfer package per checkpoint and one for
  the Ark transaction. Rationale: each Bitcoin graph edge is a distinct V1
  asset transition and tap-sdk packages are the recovery and tamper-detection
  boundary. Date/Author: 2026-07-15 / Codex.
- Decision: reject signing until both transition layers are committed and
  durably representable. Rationale: committing a checkpoint changes its txid;
  committing the Ark changes recipient output keys. Any earlier Bitcoin
  signature would bind stale topology. Date/Author: 2026-07-15 / Codex.
- Decision: start with exact caller-funded, isolated single-asset anchors while
  keeping the wire/container shape capable of multiple checkpoint packages.
  Rationale: this is the smallest path supported by the merged SDK without
  depending on unresolved wallet funding or passive-inventory APIs.
  Date/Author: 2026-07-15 / Codex.
- Decision: do not downgrade Wavelength's btcd dependency graph or hide the
  mismatch with local replaces. Continue the protocol, persistence, and
  operator validation work against opaque sealed packages; add the concrete
  tap-sdk adapter once `tap-sdk#163` provides a compatible module graph.
  Rationale: Wavelength, lnd, btcwallet, and taproot-assets already consume the
  v0.26 generation, so a downgrade would turn a visible integration blocker
  into broad dependency risk. Date/Author: 2026-07-15 / Codex.
- Decision: persist the 32-byte asset root directly on each VTXO and exclude
  such VTXOs from generic Bitcoin coin selection. Rationale: the root is
  required to reconstruct every future control block, while ordinary rounds
  do not yet carry an asset state transition and must not accidentally consume
  an asset-bearing output. Date/Author: 2026-07-15 / Codex.

## Outcomes & Retrospective

Implementation is in progress. The SDK-neutral package/root, typed transport,
and prepared-FSM milestones pass `go test ./lib/tx/oor ./rpc/oorpb ./oor` and
changed-code lint. Prepared sessions prove that Ark signing is the first FSM
effect and that submit retries restore the same sealed packages and canonical
recipients. VTXO persistence now retains the asset root, generic selection
skips asset-bearing rows, and regression tests cover composed checkpoint,
forfeit, and timeout control blocks. The receive path now preserves roots and
sealed packages across transport, actor restart, materialization, artifact
lookup, and idempotent retry. The client branch passes `make build`,
`go test ./oor ./vtxo ./unroll ./waved ./db`, and changed-code lint. The first
confirmed upstream gap is
`lightninglabs/tap-sdk#163`; this section will record the remaining test
evidence and any further tapd/tap-sdk gaps.

## Context and Orientation

The deterministic outgoing protocol lives in `oor/`. `StartTransferEvent` is
processed by `Idle.ProcessEvent` in `oor/transitions.go`; today it builds plain
checkpoint PSBTs and an Ark PSBT, then immediately emits
`RequestArkSignatures`. State is exported through `OutgoingSnapshot` and a TLV
codec. Submit transport is defined in `rpc/oorpb/oorwire.proto` and converted
by `oor/outbox_messages.go`.

Bitcoin graph primitives live in `lib/tx/oor`. A checkpoint spends one VTXO,
creates its policy output at index zero, and appends P2A. The Ark transaction
spends the checkpoint outputs, pays canonically sorted recipients, and appends
P2A. `lib/arkscript` owns semantic policy compilation and
`ComposeWithSiblingRoot`.

tap-sdk's `CustomAnchorTxBuilder` consumes proof-selected inputs plus a caller
anchor PSBT. `Build` prepares V1 virtual packets; `Commit` asks tapd to insert
the asset commitments and returns a sealed `CustomAnchorTransferPackage` with
the committed anchor bytes, proof suffixes, output roots, signing plans, and
digests. Wavelength must persist that package rather than copying internal
taproot-assets structures.

## Plan of Work

First add a versioned shared asset extension under `lib/tx/oor`. It will carry
the ordered checkpoint package bytes and Ark package bytes and will validate
basic cardinality and size bounds. Add the Taproot Asset root to recipient
metadata so clients and the operator can reconstruct a composed Ark policy
without decompiling a P2TR output.

Next add a tap-sdk adapter package. It will accept exact proof-selected asset
inputs and logical recipient allocations, serialize Wavelength PSBT/v2
templates into the tap-sdk byte boundary, and run the graph in this order:

1. Build the ordinary checkpoint templates.
2. Commit the asset transition for each checkpoint and parse the returned
   committed checkpoint PSBT.
3. Rebuild the Ark transaction from those committed outpoints.
4. Attach owner-leaf control blocks composed with each checkpoint asset root.
5. Extend each confirmed/compact proof path with the checkpoint proof suffix.
6. Commit the final Ark transition and parse its committed PSBT.
7. Bind final output roots to canonical recipient metadata and return one
   immutable prepared OOR package.

Then teach the outgoing actor to accept the prepared package as an explicit
entry point. The FSM must validate it, derive the session ID from the committed
Ark txid, snapshot the asset package before emitting `RequestArkSignatures`,
and thread it through submit retries. The ordinary constructor continues to
build Bitcoin-only packages exactly as before.

Protobuf transport is extended additively with generated submit and
offline-receive fields. Next thread incoming notification data through the
durable receiver so it can materialize an asset-aware VTXO, and expose a
narrow daemon/SDK method for swapd. Publication remains out of scope until the
path-aware tapd follow-up lands.

## Concrete Steps

Work from `/Users/dario/dev/lightninglabs/.worktrees/wavelength-client-assets`.

After each implementation milestone run:

    make fmt-changed
    make lint-changed-local

Run focused tests while editing, for example:

    go test ./lib/tx/oor ./tapassets ./oor ./rpc/oorpb

After protobuf edits run:

    make rpc

Before handoff run the repository build and the broadest practical unit suite:

    make build
    make unit

Commit each milestone with a signed, package-prefixed commit message.

## Validation and Acceptance

Acceptance requires a test that starts with at least one confirmed Taproot
Asset proof and ordinary Wavelength VTXO/checkpoint/recipient policies and
proves all of the following without patching serialized transactions after
commit:

- each checkpoint package validates and its committed txid is the outpoint
  consumed by the Ark package;
- each policy root composed with `TaprootAssetRoot` equals the package's
  `TaprootMerkleRoot` and the actual P2TR output key;
- both asset packages survive encode/decode and outgoing FSM snapshot restore;
- Ark signing is the first emitted side effect after asset preparation;
- submit retries are byte-identical and carry the same asset packages;
- Bitcoin-only OOR tests remain unchanged; and
- mutations to package digests, roots, outpoints, PSBT bytes, ordering, or
  cardinality are rejected before submission.

The cross-repository showcase is accepted when swapd can opt into the asset
path, the operator rejects malformed packages before input locking, and a
valid request reaches the existing OOR signing/finalize path.

## Idempotence and Recovery

Planning and validation are deterministic, but tapd commit is an external
side effect with an outcome-unknown timeout. Never blindly re-run a timed-out
commit. Persist the request identity and any sealed response before advancing;
if the backend outcome is unknown, stop and surface reconciliation rather than
producing a competing package. Once both sealed packages exist, all OOR actor
retries use their exact bytes and are safe under the existing idempotent submit
rules.

Generated protobuf files are recreated with `make rpc`; rerunning generation
is safe. Tests and formatting commands are safe to repeat. Do not reset the
original checkout or its user-owned submodule state.

## Artifacts and Notes

- tap-sdk design: `tap-sdk/docs/design/advanced-custom-anchor-transactions.md`
- tap-sdk epic: `lightninglabs/tap-sdk#139`
- tap-sdk btcd compatibility blocker: `lightninglabs/tap-sdk#163`
- historical reference: `lightninglabs/darepo-client#12`
- Wavelength OOR overview: `docs/oor_subsystem.md`

The feature branch is `feat/wavelength-taproot-assets-oor`, based on
`origin/claude/wavelength-project-rename-f5f721`.

## Interfaces and Dependencies

The shared OOR layer will gain a stable, versioned asset-extension DTO whose
payloads are sealed tap-sdk `CustomAnchorTransferPackage` binary encodings.
Recipient outputs will gain an optional 32-byte Taproot Asset root. The
tap-sdk adapter will depend on `github.com/lightninglabs/tap-sdk` and consume
only SDK-owned public DTOs plus serialized PSBT bytes.

The prepared-session API will require committed Ark/checkpoint PSBTs, transfer
inputs, canonical recipients, and the asset extension. It will not accept a
mutable tap-sdk builder or tapd client inside the deterministic FSM. That keeps
network I/O at the orchestration boundary and makes durable actor replay
independent of tapd availability.

Revision note (2026-07-15): updated after the first SDK-neutral root/container
milestone, the tap-sdk dependency compatibility reproduction, and the durable
asset-bearing VTXO/spend-path milestone.
