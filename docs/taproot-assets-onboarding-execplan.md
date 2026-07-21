# Onboard a Taproot Asset into a Wavelength VTXO

This ExecPlan is a living document. The sections `Progress`, `Surprises & Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to date as work proceeds. This plan is maintained in accordance with `PLANS.md` at the repository root.

## Purpose / Big Picture

After this change, a user running `waved` with the opt-in Taproot Assets integration can move one isolated, confirmed Taproot Asset anchor into a Bitcoin output controlled by a normal Wavelength VTXO policy. Once the transaction confirms, `waved` registers that exact output with the operator and persists the same descriptor locally. The output can then be selected by the existing `taproot-asset-oor` prototype, proving that tap-sdk can bootstrap asset state into Wavelength without giving tapd credentials to the operator.

The observable demonstration is a single idempotent daemon RPC and CLI command. The first call builds, commits, signs, and publishes the custom-anchor transaction. Calls made before confirmation report a pending state without committing another transition. A call after confirmation registers and persists the VTXO and reports it ready for OOR spending.

## Progress

- [x] (2026-07-15 17:27Z) Audited round boarding, OOR preparation, tap-sdk custom anchors, wallet signing, and VTXO lineage constraints.
- [x] (2026-07-15 17:27Z) Selected a direct confirmed asset-deposit VTXO as the PoC protocol.
- [x] (2026-07-15 18:31Z) Added the SDK-neutral onboarding request, result, journal, tap-sdk-backed implementation, protocol RPCs, and restart/idempotency tests.
- [x] (2026-07-15 19:58Z) Added LND WalletKit anchor signing, operator registration, ready-only local persistence, actor activation, and restart-safe RPC tests in `waved`.
- [x] (2026-07-15 18:23Z) Added the daemon protobuf RPC and the advanced `wavecli taproot-assets onboard` command with optional confirmation polling.
- [x] (2026-07-15 18:50Z) Added unit, persistence, RPC, retry, and command tests; ran repository build, full unit, formatting, changed-code lint, and race checks.

## Surprises & Discoveries

- Observation: ordinary Wavelength boarding cannot preserve a Taproot Asset transition.
  Evidence: `wallet.TriggerBoardMsg`, `types.VTXORequest`, and the round wire contain Bitcoin amounts and policies but no sealed asset transition or output root.

- Observation: tap-sdk deliberately leaves Bitcoin anchor signing to the host application.
  Evidence: `CustomAnchorTransferPackage.SigningRequests` identifies the exact key-path signer and tap-sdk's integration test signs with LND WalletKit before calling `PublishCustomAnchorTransfer`.

- Observation: a confirmed Wavelength policy output is directly spendable by the existing OOR checkpoint builder once both client and operator have registered the same descriptor.
  Evidence: OOR input construction consumes a `vtxo.Descriptor` and validates the policy plus `TaprootAssetRoot`; it does not require the input to have been created by a round.

- Observation: `TaprootMerkleRoot` is not equal to `TaprootAssetRoot` when a Wavelength policy sibling is supplied.
  Evidence: tap-sdk exports the former as the final BIP-341 root and the latter as the asset-only commitment. The onboarding validator independently recomputes `TapBranch(policy_root, asset_root)` and checks the actual P2TR output.

## Decision Log

- Decision: implement a dedicated on-chain asset deposit, not asset-aware round boarding.
  Rationale: the round would need a new multi-party phase after the final transaction is known and before any MuSig2 signature is produced. The deposit path is isolated, idempotent, and composes with the existing asset OOR implementation.
  Date/Author: 2026-07-15 / Codex

- Decision: the first PoC accepts exactly one complete asset proof from an isolated anchor and moves the full asset amount.
  Rationale: partial allocations and passive co-anchored assets require additional change outputs and proof delivery policy. Rejecting them is explicit and cannot lose assets.
  Date/Author: 2026-07-15 / Codex

- Decision: constrain automatic Bitcoin anchor signing to Wavelength's LND backend and require tapd to use the same LND wallet.
  Rationale: tap-sdk returns an exact external signing plan. LND WalletKit can sign the tapd-managed anchor key without exposing private keys, while other Wavelength wallet backends do not necessarily own tapd's anchor keys.
  Date/Author: 2026-07-15 / Codex

- Decision: treat registration as the boundary at which the output becomes a selectable VTXO.
  Rationale: the operator must independently observe the confirmed UTXO and validate the sealed package before it can safely lock or co-sign a later OOR spend.
  Date/Author: 2026-07-15 / Codex

- Decision: do not add a separate owner-registration signature to the PoC registration RPC.
  Rationale: the sealed package, final PSBT, standard policy template, and asset root bind the exact output and its owner key. Registering that public output cannot grant spending authority, while the subsequent OOR spend still requires the owner signature. This keeps the operator boundary credential-free and avoids inventing a second authentication protocol.
  Date/Author: 2026-07-15 / Codex

- Decision: require an explicit CLI idempotency key instead of generating one.
  Rationale: a generated key that is not durably recorded outside waved makes a manual retry after interruption unsafe. Requiring the caller to choose and preserve the key makes the command's retry contract visible and testable.
  Date/Author: 2026-07-15 / Codex

## Outcomes & Retrospective

The client now moves one complete isolated asset anchor into a standard
Wavelength policy through tap-sdk, delegates Bitcoin signing to LND WalletKit,
publishes and logs the transition through tapd, waits for operator-confirmed
registration, and only then materializes the output as a selectable local
VTXO. The durable journal binds the caller's idempotency key to the request,
owner key, sealed package, and final PSBT; post-commit retries are
byte-identical and ambiguous commit outcomes fail for reconciliation.

The supported PoC shape is deliberately narrow: tapd and `waved` share one LND
wallet, the selected proof owns the anchor's only asset, the full asset amount
moves to one wallet-owned asset script key, and `max_fee_sat` is paid exactly.
The advanced `wavecli taproot-assets onboard` command requires a stable
caller-owned request ID and can poll pending confirmation with `--wait`.

Validation completed with `make rpc`, `make build`, `make unit`,
`make lint-changed-local`, focused package tests, and `go test -race
./tapassets ./waved`. Coverage includes request conflicts, passive/partial
rejection, policy/root composition, ambiguous commit handling, restart
restoration, WalletKit signing, pending-to-ready registration, duplicate local
persistence, proof-file CLI mapping, and byte-identical wait retries. This
branch has not run the full cross-repository regtest against live tapd,
bitcoind, operator, sender, and receiver processes.

No additional tap-sdk or taproot-assets API issue was required. Remaining
prototype limitations are non-LND anchor signing, partial/passive asset
transitions, fee estimation/change, proof-courier product UX, and a demonstrated
unilateral-exit path for the direct root. The supported showcase continues by
spending the registered output through the existing Taproot Asset OOR path.

## Context and Orientation

`tapassets/preparer.go` currently adapts tap-sdk's custom-anchor builder to the two-transition OOR graph. `tapassets/store.go` provides an atomic file journal used to make commit attempts restart-safe. `waved/server.go` owns tapd and LND connections, while `waved/rpc_server.go` serves local daemon RPCs. `waverpc/daemon.proto` defines that local API, and `cmd/wavecli/waveclicommands` contains its CLI commands. `vtxo.Descriptor` is the client-side durable description of a spendable Wavelength output.

A custom-anchor package is a tap-sdk-owned sealed description of a Taproot Asset transition. Its output summary exposes the exact Bitcoin anchor outpoint, Taproot Asset root, complete Taproot merkle root, script key, amount, and proof update. The operator can validate this package without a tapd connection, while only the client can commit and publish it.

The onboarding output uses the standard two-leaf Wavelength VTXO policy: owner plus operator can spend collaboratively, or the owner can spend alone after a relative delay. tap-sdk composes the asset root beside this policy root and returns the resulting output script.

## Plan of Work

Add `tapassets/onboarding.go` with SDK-neutral request and result types, an injected key deriver, anchor signer, operator registrar, and a production implementation backed by `tapsdk.Wallet`. Local VTXO materialization remains in `waved`, after successful registration. Reuse the existing atomic `tapassets.Store`, but use a distinct key prefix and a versioned journal that records the request digest, derived owner key, sealed package, final PSBT, publication result, and registration result. Persist an intent marker before every tapd commit. An ambiguous commit outcome must return `ErrReconciliationRequired`; it must never blindly retry.

The builder will ask tapd to verify the proof and list the complete anchor inventory. It will require one selected asset, no passive assets, an exact amount match, and a positive Bitcoin anchor value. It will create a one-input/one-output PSBT whose output value is the input anchor value minus a caller-capped fee, whose placeholder script is the Wavelength policy script, and whose tap-sdk output uses a wallet-owned asset script key plus the complete Wavelength policy leaves. The signing plan identifies the input anchor's key-path internal key.

Add an LND WalletKit signer in `waved` that calls `SignPsbt` followed by `FinalizePsbt`. It is enabled only for the LND wallet backend. Wire the onboarding service after the wallet, tap-sdk adapter, operator terms, VTXO store, and VTXO manager are ready. The service sends a direct `arkrpc.RegisterTaprootAssetVTXO` call containing the sealed package, final PSBT, policy, and asset root; output coordinates and the owner key are derived and cross-checked from those artifacts. On success it saves the local descriptor and tells the VTXO manager to materialize it. All programmatic dependencies remain excluded from mapstructure.

Extend `waverpc/daemon.proto` with `OnboardTaprootAsset`. The request carries an idempotency key, asset ref, full amount, proof bytes, and maximum fee. The response reports pending-confirmation or ready plus the output and root. Add a CLI command that reads the proof file, requires an explicit request ID, retries pending confirmation when `--wait` is set, and prints stable JSON.

## Concrete Steps

Work from `/Users/dario/dev/lightninglabs/.worktrees/wavelength-client-assets-onboarding`.

After protobuf edits, run:

    make rpc

After each implementation milestone, run focused tests such as:

    go test ./tapassets ./waved ./cmd/wavecli/waveclicommands

Before each commit, run:

    make fmt-changed
    make lint-changed-local

Before handoff, run:

    make build
    make unit

Run race tests for the new journal and onboarding packages with:

    go test -race ./tapassets ./waved

## Validation and Acceptance

Unit tests must prove request validation, exact policy/root composition, rejection of passive assets and partial amounts, request-ID conflict detection, ambiguous-commit failure, byte-identical restoration, WalletKit signing, operator-pending retry, and idempotent local persistence.

The RPC test must call onboarding twice with the same request. The tap-sdk commit fake must observe one commit, the first response must be pending confirmation, and the second must become ready after the operator fake returns a confirmation height. A changed request with the same ID must fail before an external call.

The CLI must expose `wavecli taproot-assets onboard --idempotency-key ... --asset-ref ... --asset-amount ... --proof-file ... --max-fee-sat ...`. In a regtest deployment with tapd and Wavelength sharing LND, the command should publish one transaction, wait for a block when requested, return a ready VTXO outpoint, and allow that outpoint to be passed to the existing Taproot Asset OOR send command.

## Idempotence and Recovery

The idempotency key is bound to a canonical request digest. Reusing it with different inputs fails. Derived keys, sealed packages, and final PSBTs are journaled and reused. A process crash after a tapd commit but before the response is durably stored produces an explicit reconciliation-required error. Publication and operator registration are safe to retry with the byte-identical final package. Local VTXO persistence is idempotent only when every immutable descriptor field matches.

## Artifacts and Notes

The prototype intentionally does not change the normal round protocol. It also does not claim support for co-anchored passive assets, partial asset amounts, non-LND anchor signing, or a production proof-courier flow.

## Interfaces and Dependencies

In `tapassets/onboarding.go`, define an `OnboardingRequest`, `OnboardingResult`, and an `Onboarder.Onboard(context.Context, *OnboardingRequest)` method. Its injected dependencies must include a `tapsdk.Wallet`, `tapassets.Store`, owner-key derivation, Bitcoin anchor signing, and operator registration. The daemon RPC owns local materialization after the onboarder reports `ready`.

In `arkrpc/ark.proto`, define `RegisterTaprootAssetVTXO` as the credential-free operator registration boundary. In `waverpc/daemon.proto`, define `OnboardTaprootAsset` as the user-facing orchestration boundary.

Revision note (2026-07-15): initial plan created after the feasibility audit selected direct confirmed deposit registration over round-protocol expansion.
