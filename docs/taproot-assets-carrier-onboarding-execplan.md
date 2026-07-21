# Fund Taproot Asset onboarding carriers from the shared Bitcoin wallet

This ExecPlan is a living document. The sections `Progress`, `Surprises &
Discoveries`, `Decision Log`, and `Outcomes & Retrospective` must be kept up to
date as work proceeds. This document is maintained in accordance with
`PLANS.md` at the repository root.

## Purpose / Big Picture

After this change, a Wavelength user can move a complete, confirmed Taproot
Asset balance into Wavelength without relying on the asset's existing on-chain
anchor to contain enough bitcoin for both the new virtual transaction output
(VTXO) and the miner fee. The user chooses, or accepts the operator minimum for,
the number of satoshis carried by the asset VTXO. The Taproot Asset daemon
(`tapd`) asks the same LND wallet used by Wavelength to add ordinary bitcoin
inputs, pay the miner fee, and return any bitcoin change. The CLI and RPC expose
the carrier amount, fee policy, fee ceiling, and actual fee instead of hiding
them.

The behavior is observable with focused tests that start from a 1,000-satoshi
asset anchor, request a 1,000-satoshi carrier, and still produce a valid
wallet-funded transfer with a positive fee and optional bitcoin change. A later
Lumos plan will prove the same behavior with real LND and tapd daemons; this
plan owns the Wavelength workflow and its deterministic tests only.

## Progress

- [x] (2026-07-21 16:54Z) Replayed and signed the existing Taproot Asset
  onboarding/OOR substrate onto current `origin/main`; focused Wavelength
  package tests pass.
- [x] (2026-07-21 16:54Z) Audited the pinned tap-sdk custom-anchor funding API
  and confirmed wallet-funded inputs, added bitcoin change, fee selection,
  fee ceiling, and deterministic lock IDs are available without an upstream
  change.
- [ ] Change the durable onboarding request and tap-sdk plan from exact
  caller-funded subtraction to an exact carrier output plus wallet funding.
- [ ] Expose carrier and fee policy in the daemon RPC and `wavecli`, including
  the effective carrier and actual fee in the response.
- [ ] Add workflow, RPC, CLI, restart, and malformed-request tests, then run
  formatting, focused tests, build, and changed-code lint.
- [ ] Update this plan with final evidence and commit the carrier-onboarding
  milestone as a signed commit stacked on the integration-refresh branch.

## Surprises & Discoveries

- Observation: the original prototype subtracts an exact fee from the current
  asset anchor, so a normal 1,000-satoshi tapd anchor cannot satisfy an
  operator minimum of 1,000 satoshis once any miner fee is paid.
  Evidence: `tapassets/onboarding.go` computes `outputValue := anchor.AmtSat -
  int64(request.AnchorFeeSat)` and rejects values below the dust floor.
- Observation: tap-sdk wallet funding preserves all caller-declared asset
  output values and indices, may append ordinary bitcoin inputs, and may append
  at most one ordinary bitcoin change output.
  Evidence: the pinned tap-sdk validates `CustomAnchorFundingWalletFunded` with
  `AnchorChangeOutputAdd`, `AnchorFee`, `MaxFeeSat`, and a 32-byte
  `CustomLockID`; its committed-shape validator rejects mutation of asset
  outputs.
- Observation: an added bitcoin change output does not need a Wavelength key or
  policy. It is an ordinary LND on-chain wallet output and is not registered as
  a VTXO by the operator.
  Evidence: tap-sdk delegates `AnchorChangeOutputAdd` to WalletKit and records
  the backend-selected change index separately from asset outputs.

## Decision Log

- Decision: wallet-fund onboarding through tap-sdk rather than manually
  selecting LND UTXOs in Wavelength.
  Rationale: tap-sdk already seals the exact funding policy, locked outpoints,
  actual fee, and backend-managed input indices into the transfer package. It
  also verifies that the wallet did not mutate the declared asset output.
  Date/Author: 2026-07-21 / Codex.
- Decision: zero `carrier_value_sat` means the current operator-advertised
  minimum VTXO amount; a positive value must be at least that minimum.
  Rationale: this gives a useful default while keeping carrier satoshis visible
  in both request documentation and response data.
  Date/Author: 2026-07-21 / Codex.
- Decision: require exactly one of `fee_rate_sat_per_vbyte` and `target_conf`,
  plus a non-zero `max_fee_sat` hard ceiling.
  Rationale: the fee strategy and the maximum possible spend are distinct. An
  estimator can choose a fee while the user retains an absolute loss bound.
  Date/Author: 2026-07-21 / Codex.
- Decision: derive tap-sdk's 32-byte custom lock ID by hashing a domain label
  and the durable onboarding request digest.
  Rationale: retries of the same request reuse the same WalletKit lease
  identity, while a different request cannot collide accidentally or reuse a
  caller-controlled raw lock ID.
  Date/Author: 2026-07-21 / Codex.
- Decision: retain the current isolated-asset and complete-amount onboarding
  restrictions.
  Rationale: carrier funding solves the bitcoin shortfall without expanding
  passive-asset or partial-asset semantics. Partial sends are a separate
  off-chain OOR milestone.
  Date/Author: 2026-07-21 / Codex.

## Outcomes & Retrospective

The implementation is in progress. The refreshed substrate is available as
the signed `feat/taproot-assets-integration-refresh` branch, and this plan is
being implemented on `feat/taproot-assets-carrier-onboarding`. Final behavior,
test evidence, and any remaining live-daemon gap will be recorded here.

## Context and Orientation

Wavelength is the wallet-facing daemon. A Taproot Asset is committed inside a
Bitcoin Taproot output called an anchor. An Ark VTXO is an off-chain spendable
output whose value is denominated in satoshis. When an asset is held by a VTXO,
those satoshis are its carrier: they make the Bitcoin output valid and later
fund any additional asset-change outputs. Asset units and carrier satoshis are
accounted independently.

`tapassets/onboarding.go` implements an idempotent state machine. It verifies a
confirmed proof with tapd, derives the Wavelength owner policy, builds a
tap-sdk `CustomAnchorRequest`, commits it, asks Wavelength's WalletKit signer to
finalize the Bitcoin PSBT, publishes through tap-sdk, waits for confirmation,
and registers the asset-bearing output with the operator. The sealed package
and final PSBT are stored so retrying the same idempotency key does not rebuild
or repay anything.

`tapassets/driver.go` projects a sealed tap-sdk package into Wavelength's narrow
internal representation. It must carry the package's actual miner fee so the
result can report it after restart without recalculation or another external
call.

`waved/rpc_taproot_asset_onboarding.go` converts the public daemon request into
the internal request. It already has the negotiated `OperatorTerms`; the
effective floor is `terms.MinVTXOAmountFloor()`. `waverpc/daemon.proto` defines
the public request and response, while `cmd/wavecli/waveclicommands/
cmd_taproot_assets.go` provides the user command. Generated protobuf files must
be regenerated from the proto source, never edited by hand.

Tap-sdk and Wavelength must use the same LND wallet. Tapd's WalletKit client
adds and locks ordinary bitcoin inputs during the commit. The later Wavelength
WalletKit signing pass signs both the original tapd-managed asset anchor and
the backend-added ordinary inputs. Tap-sdk records which inputs are backend
managed, preventing an input from being left unclassified.

## Plan of Work

First, replace `OnboardingRequest.AnchorFeeSat` with explicit
`CarrierValueSat`, `FeeRateSatPerVByte`, `TargetConf`, and `MaxFeeSat` fields.
Validation must accept exactly one fee selector. The request digest must bind
all four values. A helper should convert the two public fee selector fields to
tap-sdk's `AnchorFee`, returning a clear error before any journal write.

In `Onboarder.commit`, stop subtracting the fee from `ManagedUtxo.AmtSat`.
Build the initial PSBT with the asset input and one asset output whose value is
exactly `CarrierValueSat`. Select `CustomAnchorFundingWalletFunded`,
`AnchorChangeOutputAdd`, the converted fee policy, `MaxFeeSat`, and a
deterministic 32-byte custom lock ID. Keep input zero's anchor signing plan;
tap-sdk classifies any appended wallet inputs as backend managed. Do not treat
the appended ordinary bitcoin change output as an asset output.

Extend `commitResult` and `OnboardingResult` with `ActualFeeSat`, sourced from
`CustomAnchorTransferPackage.Funding.ActualFeeSat`. Update package restoration
and fake drivers so a pending-confirmation restart returns the same effective
carrier and actual fee. Validate that the sealed package is wallet-funded, its
actual fee does not exceed the requested maximum, and its asset output retains
the requested value even when the anchor PSBT contains a second bitcoin-only
change output.

Then revise `OnboardTaprootAssetRequest` in `waverpc/daemon.proto`. Preserve
existing field numbers and reinterpret field 5 only as the hard `max_fee_sat`
ceiling; add `carrier_value_sat`, `fee_rate_sat_per_vbyte`, and `target_conf` at
new field numbers. Add `actual_fee_sat` to the response; existing `value_sat`
remains the effective carrier value for compatibility. The RPC resolves zero
carrier to the operator floor and rejects a positive carrier below that floor.
Regenerate all `waverpc` outputs and the low-level CLI development registry.

Finally, update `wavecli taproot-assets onboard`. Add `--carrier-value-sat`
with a zero/default meaning operator minimum, `--sat-per-vbyte`, and
`--target-conf`. Require exactly one fee selector and keep `--max-fee-sat` as
the hard cap. Update command help so users understand that carrier satoshis are
Bitcoin value belonging to the asset VTXO and may require ordinary wallet
funds. Extend CLI and RPC tests for defaults, mutual exclusion, floors,
idempotency rewrites, actual-fee persistence, and wallet-funded request shape.

## Concrete Steps

Work from `/Users/dario/dev/lightninglabs/.worktrees/
wavelength-carrier-funding` on branch
`feat/taproot-assets-carrier-onboarding`.

After each implementation slice, format only changed Go files:

    make fmt-changed

Regenerate RPC files using the repository target when Docker is available:

    make rpc

If the local Docker daemon is unavailable, use the exact protobuf compiler and
Go plugin versions pinned by `scripts/rpc.Dockerfile` and `go.mod`, generate the
`waverpc` package from `waverpc/daemon.proto`, then verify a second generation
is clean. Record that environment limitation in this plan.

Run the focused tests during development:

    go test ./tapassets ./waved ./cmd/wavecli/waveclicommands

Before the milestone commit, run:

    go test ./tapassets ./waved ./cmd/waved \
      ./cmd/wavecli/waveclicommands ./waverpc
    make build
    make lint-changed-local

Commit the completed milestone with an SSH signature and the repository's
package-prefix convention:

    git commit -S -m 'tapassets: wallet fund onboarding carriers'

## Validation and Acceptance

`TestOnboarderResumesPendingConfirmation` must prove that the request passed to
tap-sdk is wallet funded, has `AnchorChangeOutputAdd`, carries the selected fee
strategy and maximum, uses a deterministic 32-byte lock ID, and preserves the
exact requested carrier value. It must also prove a restart returns the same
actual fee without a second commit, signature, or publication.

A dedicated test must set the managed asset anchor to 1,000 satoshis, request a
1,000-satoshi carrier, and use a positive fee ceiling. It passes because wallet
funding is authorized; the pre-change implementation fails because it can only
produce an output smaller than 1,000 satoshis. Negative tests must reject no
fee selector, two fee selectors, a zero maximum fee, a zero internal carrier,
and a sealed package whose actual fee exceeds the maximum.

RPC tests must show that carrier zero becomes the operator floor, a carrier
below the floor returns `InvalidArgument`, and the response contains both the
effective carrier and actual fee. CLI tests must show that exactly one of
`--sat-per-vbyte` and `--target-conf` is required, while omitting
`--carrier-value-sat` leaves zero for daemon-side defaulting.

The final diff must not add tapd credentials to Lumos or swapd. Bitcoin-only
Wavelength behavior and the existing exact caller-funded OOR builder must
remain unchanged. The full live acceptance test is intentionally owned by the
stacked Lumos integration branch because that repository starts the operator,
two Wavelength clients, paired LND/tapd daemons, and bitcoind.

## Idempotence and Recovery

The request digest binds the carrier and fee policy, so reusing an idempotency
key with different economics fails before another tapd call. The custom lock ID
is derived from that digest, so retries use the same backend lock identity.
Once the sealed package is stored, retries decode it and reuse the exact PSBT.
If tapd reports a known failure before committing, clear the attempt marker and
allow a safe retry. If the outcome may have committed, preserve the marker and
return `ErrReconciliationRequired`; do not build a competing transition.

The implementation must never unlock or publish a different PSBT merely
because an RPC retry timed out. A future upstream status-by-custom-lock-ID API
can reconcile an ambiguous tapd outcome; until then the safe PoC behavior is a
quarantine with an explicit error.

## Artifacts and Notes

The refreshed substrate's focused validation completed successfully before
this plan was created:

    ok github.com/lightninglabs/wavelength/oor
    ok github.com/lightninglabs/wavelength/vtxo
    ok github.com/lightninglabs/wavelength/unroll
    ok github.com/lightninglabs/wavelength/tapassets
    ok github.com/lightninglabs/wavelength/waved
    ok github.com/lightninglabs/wavelength/cmd/waved
    ok github.com/lightninglabs/wavelength/cmd/wavecli/waveclicommands
    ok github.com/lightninglabs/wavelength/db
    ok github.com/lightninglabs/wavelength/lib/tx/oor

## Interfaces and Dependencies

At the end of this plan, `tapassets.OnboardingRequest` must expose:

    CarrierValueSat       uint64
    FeeRateSatPerVByte    uint64
    TargetConf            uint32
    MaxFeeSat             uint64

`tapassets.OnboardingResult` and the daemon response must expose
`ActualFeeSat uint64`. `tapassets.commitResult` must preserve the same value
from `tapsdk.CustomAnchorTransferPackage.Funding.ActualFeeSat`.

The generated tap-sdk request must use
`tapsdk.CustomAnchorFundingWalletFunded`,
`tapsdk.AnchorChangeOutputAdd`, either `tapsdk.AnchorFeeSatPerVByte` or
`tapsdk.AnchorFeeTargetConf`, the caller's maximum fee, and a deterministic
32-byte custom lock ID. No new tap-sdk or taproot-assets dependency is expected
for this milestone.

Revision note (2026-07-21): created this plan after rebasing the existing
integration and auditing tap-sdk's wallet-funded custom-anchor contract. The
scope deliberately ends at onboarding; mixed off-chain asset sends and the
live Lumos topology remain separate reviewable milestones.
