# Reserve asset VTXOs while selecting ordinary Bitcoin carriers

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to
date as work proceeds. This document is maintained in accordance with
`PLANS.md` at the repository root.

## Purpose / Big Picture

After this change, a Taproot Asset out-of-round send names the locally managed
asset VTXO it must consume. Wavelength atomically reserves that VTXO and, when
the Bitcoin target requires more satoshis, selects only ordinary Bitcoin VTXOs
as additional carrier inputs. An unrelated asset VTXO can never be selected as
if it were ordinary Bitcoin balance. Before tapd is allowed to commit an asset
transition, Wavelength records a durable owner for every selected input so a
restart cannot mistakenly release inputs whose asset outcome is ambiguous.

The behavior is observable in focused tests. A manager fixture containing one
required asset VTXO, ordinary Bitcoin VTXOs, and another unrelated asset VTXO
must always return the required asset plus only enough ordinary Bitcoin value.
RPC tests must show the explicit asset outpoint reaches the wallet selection
request, all errors before OOR actor admission unlock known-safe reservations,
and an ambiguous tapd result leaves the preparation reservations quarantined.

This milestone intentionally retains the current one-asset-input,
one-asset-recipient transaction builder. It establishes selection and recovery
semantics independently. A later custom-transaction milestone will encode
mixed asset and Bitcoin checkpoints and asset change without changing these
wallet-selection rules.

## Progress

- [x] (2026-07-21 17:15Z) Audited the current wallet, VTXO manager, database,
  OOR RPC, tapassets preparer, and spending-reservation ownership paths.
- [x] (2026-07-21 17:15Z) Wrote and committed this living implementation plan
  on `feat/taproot-assets-carrier-selection` stacked above carrier onboarding.
- [x] (2026-07-21 17:41Z) Added required-outpoint selection through wallet and
  manager messages, including ordinary-Bitcoin filtering and min-change
  behavior.
- [x] (2026-07-21 17:41Z) Added an explicit asset input outpoint to the RPC
  intent and routed asset sends through managed wallet selection instead of
  the custom-input bypass.
- [x] (2026-07-21 17:41Z) Persisted pre-commit preparation ownership, preserved
  ambiguous and post-checkpoint outcomes, and wired one shared reservation
  store into the tap-sdk runtime.
- [x] (2026-07-21 17:41Z) Added manager, wallet, database, RPC, preparer, actor,
  and restart tests; regenerated sqlc/protobuf output; and passed formatting,
  focused tests, race tests, full unit tests, build, and changed-code lint.
- [x] (2026-07-21 17:48Z) Recorded final evidence and staged the implementation
  as a separate signed milestone.

## Surprises & Discoveries

- Observation: the refreshed integration already stores an optional
  `taproot_asset_root` and its production selection SQL excludes non-null
  roots, but the lightweight Go projection does not expose the root.
  Evidence: `db/sqlc/queries/vtxo.sql` filters
  `taproot_asset_root IS NULL`, while `actormsg.SelectedVTXO` carries only
  outpoint, amount, and pkScript. The manager therefore cannot defend itself
  against a faulty alternate store or test double that returns asset rows.
- Observation: the VTXO manager reserves spend inputs asynchronously, while
  the durable OOR reservation rows are currently written only after an OOR
  session checkpoints.
  Evidence: `vtxo.Manager.selectAndReserveVTXOs` uses detached child asks for
  spend selection, and `oor.sessionBehavior.recordReservations` writes owner
  kind zero during session admission. Tapd preparation occurs between these
  two points.
- Observation: the existing asset RPC requires one `custom_input`, bypassing
  the managed wallet selector even though asset VTXOs are persisted wallet
  VTXOs.
  Evidence: `waved/taprootAssetOORIntent` requires exactly one custom input and
  `RPCServer.SendOOR` enters `BuildCustomTransferInputs` whenever any custom
  input is supplied.
- Observation: safety changes at the first successful Taproot Asset commit,
  not only when tapd returns an explicitly unknown outcome. Once the checkpoint
  transition succeeds, a later Ark rejection, journal failure, or local result
  validation error cannot safely release the original asset VTXO.
  Evidence: the preparer now classifies every error with a checkpoint package
  or active commit marker as reconciliation-required; tests cover both the
  first unknown response and checkpoint-success/Ark-known-rejection sequence.
- Observation: Docker was unavailable in this worktree, so the canonical
  `make sqlc` and `make rpc` wrappers could not connect to the Docker socket.
  Evidence: both targets failed before generation. Generated output was instead
  produced with the same pinned sqlc 1.29.0, protobuf 3.21.12, Go protobuf
  1.36.11, gRPC 1.5.1, gateway 2.29.0, and repository mailbox plugin versions;
  generated headers and diffs match the canonical toolchain.

## Decision Log

- Decision: add `RequiredOutpoints []wire.OutPoint` to the wallet and manager
  spend-selection requests rather than overloading custom inputs.
  Rationale: a required outpoint is still a normal managed VTXO with the
  standard actor lifecycle. Custom inputs describe non-standard policies and
  deliberately bypass that lifecycle, which is the wrong trust model for an
  asset-bearing wallet coin.
  Date/Author: 2026-07-21 / Codex.
- Decision: preserve required-outpoint order, then append ordinary Bitcoin
  selections in largest-first order.
  Rationale: this makes the asset input stable at the graph boundary while
  retaining the deterministic selector used by ordinary sends.
  Date/Author: 2026-07-21 / Codex.
- Decision: enforce ordinary-Bitcoin eligibility in both SQL and the manager.
  Rationale: the SQL filter keeps the hot path small, while the manager-side
  `TaprootAssetRoot == nil` check protects alternate stores and tests from
  accidentally treating asset commitments as fungible satoshi balance.
  Date/Author: 2026-07-21 / Codex.
- Decision: use preparation owner kind one and the preparation request digest
  as its owner ID.
  Rationale: owner kind zero remains the admitted OOR session. The existing SQL
  upsert lets successful actor admission atomically rebind the same outpoints
  to the final session owner without another schema migration.
  Date/Author: 2026-07-21 / Codex.
- Decision: keep reservations on an unknown tapd commit outcome and unlock on
  every known pre-commit failure. Also quarantine every failure after the
  checkpoint transition has committed, even if a later Ark rejection is known.
  Rationale: releasing an input after tapd may have committed can enable a
  conflicting Bitcoin spend. Only an error before the first external commit is
  known to have no asset side effect and can restore wallet balance normally.
  Date/Author: 2026-07-21 / Codex.

## Outcomes & Retrospective

The managed carrier-input substrate is complete. An asset send now names one
wallet-managed VTXO, the manager reserves it first, and optional selection can
add only ordinary Bitcoin VTXOs. Selection validates duplicates, existence,
Live status, amounts, and in-memory ownership; it preserves deterministic
required-first ordering and minimum-change behavior. The public asset RPC no
longer accepts the custom-input bypass.

Before proof verification or the first tapd call, the tap-sdk preparer writes a
durable preparation owner derived from the request digest. The VTXO manager,
OOR registry, and compiled-in tap-sdk runtime use the same database store. A
known-safe pre-commit error drives the wallet unlock, the VTXO actor back to
Live, and atomic deletion of the preparation row. An unknown first commit or
any failure after a successful checkpoint transition returns `codes.Aborted`
with reconciliation required and leaves both the Spending state and row intact.

Validation completed with `make fmt-changed`, focused tests across `db`, `oor`,
`tapassets`, `vtxo`, `wallet`, `waverpc`, `waved`, and `cmd/waved`, the same
packages under `go test -race`, the full `make unit` suite (including the
`baselib` submodule), `make build`, and `make lint-changed-local`. All passed.

This branch deliberately does not yet make a mixed asset-plus-Bitcoin graph
buildable: the sealed asset preparation still requires exactly one input and
one recipient with equal BTC value. The selection API and reservation handoff
are ready for that next stacked transaction-builder branch. A live cross-daemon
Taproot Asset send remains the appropriate Lumos end-to-end acceptance test
once that builder consumes the selected carrier inputs.

## Context and Orientation

A virtual transaction output, or VTXO, is a spendable Bitcoin output represented
inside Wavelength. An asset-bearing VTXO commits Taproot Asset state in its
Taproot tree and also carries a satoshi value. Those satoshis are carrier value:
asset units and satoshis remain separate accounting quantities.

`wallet/messages.go` defines the wallet actor request used by
`waved/RPCServer.SendOOR`. `wallet/wallet.go` forwards that request to
`lib/actormsg/SelectAndReserveSpendRequest`. The shared message lives in
`lib/actormsg/vtxo_admission.go` to avoid a Go import cycle. The VTXO manager in
`vtxo/manager.go` is the only admission gate: it selects candidates and moves
their per-VTXO actors from Live to Spending.

`db/sqlc/queries/vtxo.sql` is the source of truth for the lightweight candidate
query. Generated files under `db/sqlc` must be produced with `make sqlc`, never
edited by hand. `vtxo.SelectedVTXO`, aliased from `actormsg.SelectedVTXO`, is
the lightweight projection consumed by the manager. Required outpoints are
loaded with `VTXOStore.GetVTXO` because their asset root and lifecycle status
must be checked explicitly.

`waverpc/daemon.proto` defines the public asset intent. The new input outpoint
is a `txid:vout` string. `waved/rpc_oor_taproot_asset.go` parses it before any
selection or tapd call. `waved/rpc_server.go` passes it in
`RequiredOutpoints`, builds all selected inputs from the local descriptor
store, and uses one cleanup owner for every error between selection and OOR
actor admission. Generated protobuf files come only from `make rpc`.

`tapassets/preparer.go` is the tap-sdk boundary. Its journal digest already
binds all Bitcoin inputs and the asset proof. Before its first tapd commit it
must call `oor.ReservationStore.UpsertReservation` for every input with owner
kind `oor.ReservationOwnerKindTaprootAssetPreparation` and the digest converted
to a `chainhash.Hash`. The database upsert later lets the admitted OOR actor
replace that preparation owner with owner kind
`oor.ReservationOwnerKindOOROutgoing`.

## Plan of Work

First, extend `wallet.SelectAndLockVTXOsRequest` and
`actormsg.SelectAndReserveSpendRequest` with required outpoints and forward an
owned copy through the wallet actor. Extend the lightweight selected VTXO
projection with an optional asset root. Add the root to the SQL projection
while retaining `taproot_asset_root IS NULL`, regenerate sqlc, and make test
stores preserve and filter the root.

In `vtxo.Manager.selectAndReserveVTXOs`, reject duplicate required outpoints.
Load each required descriptor directly and require Live status, an active
actor or recoverable stored actor, and no current in-memory reservation. Remove
required ordinary outpoints from the optional candidate set and discard every
optional candidate whose asset root is non-null. If required value is below
the target, run largest-first over ordinary candidates for the shortfall with
the original minimum-change rule. If required value already exceeds the target
but leaves sub-minimum change, select enough ordinary value to raise that
residual to the minimum. Reserve the combined required-first list through the
existing rollback-safe actor loop.

Next, add `input_vtxo_outpoint` to `TaprootAssetOORIntent` in
`waverpc/daemon.proto` and `InputVTXOOutpoint wire.OutPoint` to the SDK-neutral
intent in `oor/taproot_asset_preparer.go`. Asset requests must no longer supply
custom inputs. Parse the explicit outpoint once, route it through
`RequiredOutpoints`, and build all inputs with `BuildTransferInputs`. Preserve
custom input behavior for Bitcoin-only and non-standard-policy sends. Install a
deferred cleanup immediately after managed selection so every validation,
change-building, normalization, preparation, and actor-admission error releases
known-safe inputs. An ambiguous asset preparation error disables that cleanup
and leaves the inputs quarantined.

Then add `ReservationStore oor.ReservationStore` to
`tapassets.PreparerConfig` and `Preparer`. After validating and loading the
durable state, upsert every request input under preparation owner kind one and
the request digest before proof verification or either tapd commit. Define a
shared SDK-neutral unknown-outcome sentinel in `oor` so waved can distinguish a
safe rejection from a quarantined result without importing tap-sdk. Reuse one
`db.SpendingReservationPersistenceStore` owned by `waved.Server` for the VTXO
manager, OOR actor, and tapassets runtime; expose the narrow store through
`RPCServer` to the compiled-in `cmd/waved` registrar.

Finally, add focused tests. Manager tests cover required exact fit, required
plus ordinary shortfall, sub-minimum residual top-up, duplicate/missing/non-live
required inputs, an already-reserved required input, and exclusion of unrelated
asset VTXOs. Wallet tests prove field forwarding. Database tests prove the
projection excludes asset rows while preserving the nullable root contract.
RPC tests prove explicit outpoint parsing, custom-input rejection for assets,
required selection, and cleanup on each known pre-actor failure. Preparer tests
prove deterministic reservation ownership, idempotent restart, reservation
write failure before tapd, and ambiguous commit retention.

## Concrete Steps

Work from:

    cd /Users/dario/dev/lightninglabs/.worktrees/wavelength-carrier-funding

After SQL and protobuf source edits, regenerate rather than editing generated
files (or use the exact pinned local tools when Docker is unavailable):

    make sqlc
    make rpc

Format changed handwritten files and run focused tests throughout:

    make fmt-changed
    go test ./coinselect ./lib/actormsg ./vtxo ./wallet ./db
    go test ./oor ./tapassets ./waved ./cmd/waved ./waverpc

Before the implementation commit, run:

    make build
    make lint-changed-local
    make commitmsg-lint range="origin/main..HEAD"

The plan and implementation are separate signed commits. The plan commit is:

    git commit -S -m 'docs: plan asset carrier input selection'

The implementation commit uses a multi-package prefix because it spans the
wallet admission and tapassets boundary:

    git commit -S -m 'multi: reserve asset carrier inputs'

## Validation and Acceptance

The primary manager acceptance fixture has a required 600-satoshi asset VTXO,
ordinary 500- and 300-satoshi VTXOs, and an unrelated 2,000-satoshi asset VTXO.
For a 1,000-satoshi target with a valid minimum change, selection returns the
required asset and ordinary Bitcoin value; it never returns the unrelated
2,000-satoshi asset despite largest-first ordering. Every returned actor enters
Spending and a forced failure rolls every earlier actor back to Live.

An asset RPC request carries `input_vtxo_outpoint` and no custom inputs. The
wallet actor observes that outpoint in `RequiredOutpoints`. Reusing the same
idempotency key still returns before selection. A malformed outpoint fails
before selection. A known pre-commit preparer error unlocks all selected inputs,
returns their actors to Live, and atomically deletes the preparation rows. An
unknown commit outcome returns a reconciliation-required error while the VTXOs
remain Spending. Preparer restart tests prove the same request digest
idempotently refreshes the same durable owner without another committed graph;
manager recovery tests prove reservation rows retain Spending VTXOs at startup.

Run the commands in `Concrete Steps` and expect every package to report `ok`,
`make build` to complete, and changed-code lint to report no findings. The
future Lumos end-to-end test remains outside this branch because the current
sealed asset container still models one asset-bearing checkpoint and one asset
recipient.

## Idempotence and Recovery

Required selection is deterministic and does not mutate the caller's slice.
If any required point is invalid, no actor is reserved. If any actor reservation
fails, the existing rollback path releases the already accepted points. The RPC
cleanup uses a context detached from caller cancellation so a disconnected
client does not pin known-safe inputs.

Preparation reservation upserts are idempotent on outpoint. Repeating the same
request refreshes the same owner kind and digest. Successful OOR actor admission
rebinds the rows to its session ID. Known failures invoke the wallet release;
the VTXO status transition deletes the reservation row in the same database
transaction. Unknown tapd outcomes deliberately retain both Spending status and
the preparation row. Without an upstream status-by-lock/request API, the safe
recovery action is manual reconciliation rather than a competing retry.

SQL and protobuf generation are deterministic and safe to rerun. If generation
fails, leave source files intact, fix the tool environment, and rerun the same
target; never patch generated descriptors manually.

## Artifacts and Notes

The starting branch is:

    b2b3f10f tapassets: wallet fund onboarding carriers

The existing database already provides the ownership handoff primitive:

    ON CONFLICT (outpoint_hash, outpoint_index) DO UPDATE SET
        owner_kind = EXCLUDED.owner_kind,
        owner_id = EXCLUDED.owner_id,
        created_at = EXCLUDED.created_at;

This means the feature needs no new migration. Only the selection projection
and generated query types change.

## Interfaces and Dependencies

At completion, the wallet and manager requests expose:

    RequiredOutpoints []wire.OutPoint

The public asset intent exposes:

    string input_vtxo_outpoint

The SDK-neutral internal intent exposes:

    InputVTXOOutpoint wire.OutPoint

The reservation owner kinds are:

    const ReservationOwnerKindOOROutgoing = 0
    const ReservationOwnerKindTaprootAssetPreparation = 1

`tapassets.PreparerConfig` requires:

    ReservationStore oor.ReservationStore

No new third-party dependency is required. The only confirmed upstream gap is
a tapd or tap-sdk query that resolves an ambiguous custom-anchor commit by its
deterministic request or lock identity. This milestone remains safe without it
by quarantining the selected inputs.

Revision note (2026-07-21): created the plan after auditing the carrier
onboarding stack and current mainline reservation behavior. The scope stops at
managed selection and pre-commit recovery so mixed asset change remains a
separate reviewable milestone.

Revision note (2026-07-21): completed the selection, RPC, reservation handoff,
error quarantine, runtime wiring, generated output, and layered validation.
Expanded the safety rule from explicitly unknown tapd responses to every error
after the first successful asset checkpoint transition.
