# Taproot Assets in the Wavelength Client

A Wavelength wallet can hold a Taproot Asset inside an Ark VTXO, send it to
another wallet out of round without owning a single satoshi in Ark, and recover
it on chain if the operator disappears. This document describes the client half
of that integration: how an asset-bearing output is built, how units get into
Ark and back out, who pays for the Bitcoin that carries them, what evidence
proves an asset moved, and where the design is still incomplete.

The operator half lives in a different repository. Everything below is what
`waved` does, plus the two things it needs from the operator: a boarding
admission that understands composed outputs, and a carrier float it can lease.

Three vocabulary notes, because the rest of the document leans on them. *Units*
are asset quantity. *Carrier satoshis* are the Bitcoin value of the output the
units ride on. Nothing in the client converts between the two, ever. A *sealed
package* is the tap-sdk transition container that created an output, and it is
the only lineage a later spend has.

## Composed outputs

Every asset-bearing Ark output pays to

```
taproot(internal_key, tapBranch(sorted(policy_root, taproot_asset_root)))
```

The asset commitment root sits beside the ordinary Ark policy root as a sibling
in the tap tree. The semantic policy is therefore unchanged: the collaborative
leaf, the exit leaf, and the CSV delay are exactly what a Bitcoin-only VTXO
would carry. What changes is that every script spend of the output must present
the asset root as one extra top-level control-block sibling
(`arkscript.ComposeWithSiblingRoot` and `ComposeSpendInfoWithSibling` do that).

Composition by hash is what makes the disclosure trustworthy. Both the owner
deriving an address and the operator validating a boarding input recompute the
composed script from the policy template and the disclosed leaf hash, then
compare it byte for byte against the on-chain script. Finding a second preimage
set for the same output key would mean breaking taproot, so byte equality
authenticates the asset root without either side learning the leaf's preimage.
Because both sides must agree, they share one derivation:
`arkscript.ComposedBoardingScript`, which `tapassets.ComposedBoardingScript`
delegates to, and `arkscript.ComposedBoardingAddress`, which adds the address
plus the tapscript whose collaborative inclusion proof gained the commitment
leaf hash as its sibling.

Composition also creates a small addressing problem. A receiver has to be
findable before anyone knows the asset root that will end up in its script, so
routing cannot use the composed script. A VTXO descriptor stores the composed
`PkScript`, and `oortx.RecipientOutput.RoutingPkScript()` derives the
*uncomposed* policy script that owned-script routing, registration, and
addressing use. Migration `000021_owned_receive_asset_alias` adds the
`asset_alias` source to `owned_receive_scripts` so a composed script can be
resolved back to the uncomposed script the wallet actually owns.

## Onboarding: from a tapd anchor to a boardable output

Onboarding moves confirmed asset units that tapd already holds into a composed
output shaped like an Ark boarding input. It is one call,
`OnboardTaprootAsset`, and it is idempotent on the caller's idempotency key.
The workflow lives in `tapassets/onboarding.go` with the RPC edge in
`waved/rpc_taproot_asset_onboarding.go`.

**Selecting inputs.** `tapassets.ResolveOwnedAssetProofs` picks the wallet's
own unleased single-asset UTXOs: an exact single match first, then the smallest
sufficient UTXO, then accumulation smallest-first. Units therefore do not have
to sit in one exactly-sized UTXO, which matters because a wallet that has been
receiving assets normally has them scattered.

**Journaling.** The request is hashed into a digest that keys a durable journal
entry, and `onboardingRequestDigest` sorts the proofs by content before hashing
them. Sorting makes the digest order-independent, so a retry that enumerates
the same UTXOs in a different order resolves the same journal instead of being
rejected as a reused key with a different request.

**Asset change on pinned keys.** Whatever units exceed the amount being
onboarded return to the wallet as asset change. `pinChange` derives the change
script key and internal key once (tapd's `DeriveScriptKey` and
`DeriveInternalKey`), stores both in the journal, and declares the change
output in tap-sdk's *External* script mode. Wallet mode would derive fresh keys
on every `Build`, and the split commitment binds the change script key, so a
replay would produce a different transition and a different transaction.
External mode with persisted keys is what makes the replay byte-identical.

**Conservation, fail-closed.** The committed package is accepted only if
`sum(inputs) == boarded + change` exactly, every input carries the same asset,
and the boarded output's script recomputes from the policy and the committed
asset root. `checkOnboardingChange` re-derives the change anchor script from
the pinned internal key and compares it too. A nil change passes; anything that
looks nearly right fails.

**Who pays.** Carrier satoshis, the asset-change output's satoshis, and the
anchor's miner fee all come from the client's own lnd wallet, through a
wallet-funded tap-sdk plan bounded by `MaxFeeSat` and a deterministic
`CustomLockID`. That is why onboarding hard-requires the lnd wallet backend:
`signTaprootAssetOnboardingAnchor` signs and finalizes the anchor PSBT through
lnd's `WalletKit`, and the same on-chain wallet has to be the one tapd is
backed by.

**Exit delay.** The boarded output carries `terms.BoardingExitDelay`, not the
shorter VTXO delay. The output is spent as a round boarding input, and round
admission rejects the VTXO delay because it leaves the operator too little
margin on an input it must be able to forfeit. The delay is re-read from live
operator terms on every replay rather than persisted, so a long-lived journal
entry cannot pin a stale delay.

**Ordering.** The journal marker is written *before* the tapd commit runs, not
after. On failure, `commitOutcomeKnown(err)` decides what happens: a known
outcome clears the marker and returns the real error, while an unknown outcome
leaves the marker in place and returns `ErrReconciliationRequired`. Clearing a
marker over a commit that actually landed would let a retry double-spend the
asset, so the ambiguous case deliberately stops and asks. `FileStore` fsyncs
the containing directory after the rename for the same reason: without that
fsync the marker can be lost while the tapd mutation survives, which is the
exact state the marker exists to prevent.

## Boarding: getting the output into a round

`BoardTaprootAsset` takes the idempotency key of a completed onboarding and
gets the resulting output admitted into a round
(`waved/taproot_asset_board.go`). It replays rather than trusts: the stored
request is fed back through `Onboarder.Onboard`, which is idempotent, and the
composed script is re-derived and compared before anything else happens.

The steps, in order:

1. Load the stored onboarding request and its height hint.
2. Replay the onboarding to recover the disclosure: outpoint, owner key
   descriptor, operator key, boarding exit delay, asset ref and amount, the
   commitment leaf hash, and the OP_TRUE witness a round's commitment
   transition will spend the output with.
3. If the boarding intent store already knows this outpoint, report
   `already_boarded`, trigger the round join, and return.
4. Rebuild the on-chain script from the policy template and the leaf hash.
5. Gather the confirmation through the chainsource actor. This needs both a
   height hint and the output script, because the lnd backend refuses a zero
   hint and requires a script beside the txid. The wait is bounded, and a
   timeout surfaces as `ErrAssetBoardingUnconfirmed`, which the RPC maps to
   `FailedPrecondition` with "retry after the next block".
6. Export the boarded proof (`tapassets.ExportBoardedProof`). The boarded output
   is deliberately absent from tapd's wallet inventory, so the proof is only
   findable by scanning `ListTransfers` for the transfer that created the
   outpoint.
7. Fund the round fee (see below). This runs *before* registration, so a wallet
   with no Bitcoin in Ark fails with something actionable instead of assembling
   a round the operator rejects at seal.
8. Register the boarding disclosure with the round actor, then register a
   matching asset VTXO request.
9. Trigger the round join.

That last step is easy to mistake for redundancy, and it is not.
`Config.EagerRoundJoin` defaults to `false` on the standalone build, so
registering intents only queues them: the round actor then waits for a
`JoinNextRound` that a one-shot boarding caller never sends, and the intents
sit parked. `joinAssetBoardingRound` sends an `IntentRequested` notification to
close that gap, and it runs on the `already_boarded` path too, so a crash
between registration and join is recoverable. `round.ErrNoPendingRound` is
swallowed as benign, because it means a join is already in flight or an earlier
call committed the intents.

### Paying the round fee

An asset VTXO request is a fixed-amount request. A round carrying nothing but
one of those gives the operator no output to stamp the seal-time fee residual
on, and `resolveChangeDesignation` rejects the whole intent.

`fundAssetRoundFee` (`waved/taproot_asset_round_fee.go`) solves this by
refreshing one live Bitcoin VTXO into the same assembling round, reusing the
wallet's `RefreshVTXOsRequest` instead of composing forfeits itself. The
refreshed output is not fixed-amount, so the residual returns as change and the
operator has its slot. Selection is smallest-sufficient over `fee +
MinVTXOAmountFloor()` with an outpoint tie-break, so a large coin is never
churned to pay a small fee, and the fee quote is the boarding leg plus the
per-candidate forfeit leg, degrading to `terms.MinOperatorFee` if the operator
cannot quote.

Two guards keep this from firing when it should not. `hasFeeFundingSlot` skips
the whole thing when the client's intents for that round already include a
non-fixed output it owns, which is what leaves the same-round Bitcoin boarding
flow working unchanged. And asset-bearing rows are filtered out of the
candidate set, because an asset VTXO cannot pay the fee.

Nothing on the operator changes for this. A Bitcoin forfeit carries zero asset
units, so `validateAssetForfeitBalance` returns early and the operator's
boarding/refresh mix guard never sees it.

## Sending out of round

`SendOOR` with a `taproot_asset` intent moves units to another wallet without
waiting for a round. One call produces **one** atomic transfer, never a
sequence of them.

### One transfer, N inputs

`TaprootAssetOORIntent.InputVTXOOutpoints` is an ordered set holding up to
`oor.MaxTaprootAssetInputs = 8` asset VTXOs. Those N inputs become N checkpoint
transitions feeding a single merged ark transition. The first outpoint is the
*spine*, and the order matters at the consensus level: tap-sdk emits the merged
transition with `PrevOut` set to vPacket input 0, so the spine has to be
transition input 0.

Because the wallet's own lock returns inputs in whatever order suits it,
`orderAssetSelectedOutpoints` re-imposes the intent's order afterwards. It
returns a copy of the required set rather than a filtered view of the wallet's,
which also guarantees that a Bitcoin filler input the wallet added cannot slip
into an asset transition.

The wire field is singular (`input_vtxo_outpoint`). Leaving it empty asks the
daemon to select, and `selectTaprootAssetOORInputs` then does the work: live
candidates filtered to the exact asset ref, sorted descending by units with an
outpoint tie-break, then the smallest sufficient *single* VTXO if one exists,
otherwise largest-first accumulation. Preferring the smallest sufficient single
input keeps the larger leaves whole. Running past eight inputs is a
consolidate-first `FailedPrecondition` rather than a silent partial send. In
selection mode the caller's `asset_amount` is reinterpreted as the amount to
send, and becomes `RecipientAssetAmount` when it falls short of the selected
total.

Journaling mirrors the two questions a retry can ask, so there are two digests:

- `preparationRequestDigest` (`wavelength-asset-prepare-v3`) pins the exact
  ordered outpoint set along with everything else the package depends on. A
  different request is a different package.
- `preparationIntentDigest` (`wavelength-asset-intent-v3`) deliberately omits
  the inputs and the unit split, so it is selection-independent. A retry of a
  daemon-selected send resolves the *same* journal even though fresh selection
  could legitimately pick other coins. The receiver's effective units anchor the
  public intent instead, and they are the same under every split of one logical
  send.

`preparationState` is version 3 and holds one sealed checkpoint package per
asset ordinal, indexed by the spine-first input order. Each input's OP_TRUE
internal key is derived in its own domain: `checkpointAttempt(ordinal)` returns
`checkpoint/<ordinal>` and serves as both the journal attempt marker and the
key domain, so a marker and a key can never disagree about which input they
belong to.

### Operator-funded carriers, and reclaim

Every new asset leaf needs carrier satoshis. The operator funds them, from a
carrier-float VTXO the client leases for the transfer:
`GetInfo.oor_carrier_pubkey` advertises the float owner key, and
`ArkService.LeaseOORCarrier` hands back the float's outpoint, value, policy
template, pkScript, and expiry.

The lease is taken before any wallet reservation, so a lease failure costs
nothing. The client does not trust the response:
`BuildOperatorFundedTransferInput` re-derives the policy from the template,
requires it to match the returned pkScript, validates it against the operator's
long-term key, and requires its owner to be the advertised carrier key. The
owner leaf is rebuilt as the float's own collaborative multisig, and no client
key is stamped on the input, because the operator signs both legs. There is no
release RPC: an unused lease expires on its own, so a client that dies between
leasing and submitting needs no compensating call.

The leased input is marked `TransferInput.OperatorFunded`, and that marker has
to be honoured at every site that would otherwise assume local ownership:
`SignCheckpointPSBTs` (`oor/checkpoint_sign.go`), `signArkPSBTInput`
(`oor/ark_sign.go`), and `NormalizeCheckpointOwnerLeaves`
(`oor/transitions.go`), the last because normalizing would overwrite the
float's lease-supplied owner leaf with a locally derived one.
`WalletInputOutpoints` and `queueVTXOSent` exclude it too, so the float is
neither reserved as wallet liquidity nor booked as value the user sent. The
operator's own signatures are still verified on both legs. Because a resumed
session must not re-attempt local signing on the float, the marker is persisted
as TLV record 21 inside each transfer-input record, which is what outgoing
snapshot version 8 adds.

The second marker, `TransferInput.TaprootAssetRoundCreated`, answers a
different question: whose money is the carrier this input already has?
`BuildTransferInputs` derives it from whether the descriptor carries a sealed
package, since only a round-created leaf stores one. A round-created leaf's
carrier is the sender's own money and comes back as plain sender change. An
OOR-created leaf's carrier came from an operator lease in the first place, so
it is *reclaimed* into the operator's change when the leaf is spent.

`TaprootAssetOORPrepareRequest.CarrierAllocation()` is the single place the
layout is decided, with `leafCount` equal to 2 on a partial send and 1
otherwise, and `floors = OutputFloor * leafCount`:

```
recipient leaf   = OutputFloor                          (always)
asset change     = OutputFloor                          (partial send only)
sender change    = Σ carriers of ROUND-created inputs   (output omitted when 0)
operator change  = Lease.Value - floors
                     + Σ carriers of OOR-created inputs (reclaim)
```

A lease worth less than `floors` is refused outright.
`tapassets.Preparer.plannedRecipients` materializes the outputs in that order
(receiver, asset change, sender change, operator change), and
`TaprootAssetOORPreparation.Validate` re-checks all of it: exactly one output
paying the lease pkScript for the operator's change, a sender-change output if
and only if the sender-change amount is positive, every new asset leaf exactly
at the floor, and asset units conserved against the intent.

Two consequences worth stating plainly. A sender needs **no Bitcoin at all** to
send an asset out of round; Bitcoin in Ark exists to pay round fees. And the
operator's residual is not wallet money, which is why
`registerTaprootAssetChangeAliases` skips the recipient whose pkScript equals
the lease pkScript rather than giving it an owned receive script.

One more ordering difference: an asset transfer crosses the point of no return
at the tapd commit, earlier than a Bitcoin transfer's server co-signing.
`oor.prePONRInputOutpoints` therefore releases nothing once the state carries a
`TaprootAssetTransfer`, and a resumed request must reuse its journaled lease
rather than take a fresh one, since a second float would double the operator's
exposure for one transfer.

## Proof evidence

An output this wallet created is proved by an `AssetProofPath`: one verified
confirmed base proof, plus the unconfirmed transition steps stacked above it.
`tapassets.ResolveCreatedAssetProofSource` rebuilds that path from a sealed
package, along with the OP_TRUE witness the output is spent with.

With one asset input the path is a straight line. With N inputs it is a tree,
and the path is promoted to **version 2**: the spine's path carries the new
merging step, and each co-input's full compact path is attached as
`CoInputPaths` on that step. A version 2 path must leave `AdditionalBaseProofs`
empty, so `promoteAssetProofPathV2` migrates any additional bases into
`Steps[0].CoInputPaths` first. The spine is identified as the package input
whose anchor outpoint equals the new step's `PreviousAnchorOutpoint`; every
other asset input is a co-input, and co-inputs are sorted stably by logical
input index. That stability is what lets `resolveCreatedAssetProofSource`
rebuild the identical path at respend time, from the same sealed package,
months later.

`CollectAssetProofPathAnchors` walks the spine and recurses through every
co-input path, deduplicating by txid. It exists because the lineage of a
multi-input transfer is a DAG rather than a chain, and a claim has to confirm
every anchor in it.

### Verifying proofs across the DAG

`assetTransitionVerifier` (`tapassets/verifier.go`) fans one tap-sdk verifier
callback across every asset input of a build. It routes on two different keys,
because the two questions are genuinely different.

**Confirmed bases route by the hash of the proof bytes.** Every source's proof
file, every `AdditionalBaseProofs` entry, and recursively every `CoInputPaths`
entry is registered under `sha256` of its bytes. A miss fails closed. Verifiers
are scoped to the proof file rather than to a particular input, because two
inputs may legitimately share a base and would otherwise overwrite each other.

**Unconfirmed steps route by the `(previous, anchor)` outpoint edge**,
consuming tap-sdk's `PreviousAnchorOutpoints`. Every outpoint is consumed at
most once across the transfer DAG, so the edge is globally unique. A step
*index* is not: it repeats inside co-input paths, which is exactly why indexing
by it was wrong once co-inputs existed. A miss returns nil, because a
historical step was already bound when its own package committed. A hit
requires exactly one previous outpoint matching the expectation, and
byte-identical anchor transaction bytes; more than one previous outpoint is
rejected outright, since a merging step can never bind to a checkpoint edge.

The tree materializer's `treePathVerifier` is the strict counterpart: a tree
node's whole path is known up front, so it binds every step by exact step
index.

## Exit and claim

Unilateral exit for an asset leaf runs the normal machinery up to a point. The
unroll actor broadcasts the pre-signed tree path, putting the composed output
on chain under the owner's exit leaf. Then it stops:
`unroll.behavior.resolveExitSpendPolicy` refuses to produce a final sweep
policy when `desc.TaprootAssetRoot != nil`, reporting that the sweep is
withheld to preserve the asset commitment. This is deliberate. The generic
sweep spends the target to a plain wallet output, which would recover the
carrier satoshis and destroy the asset commitment along with them.

`ClaimTaprootAssetVTXO` is the asset-aware replacement, and it is a separate
spend after the exit delay matures. It requires the VTXO to be in
`VTXOStatusUnilateralExit`, and it does three things the generic sweep cannot:

1. **Gather the whole lineage.** `assetClaimConfirmations` collects every
   anchor `CollectAssetProofPathAnchors` names, spine and co-inputs alike,
   matching each against the descriptor's ancestry tree paths to supply the
   output script the chain backend needs beside the txid. Multi-input
   transfers are covered because the ancestry fragments span every
   Bitcoin-side branch.
2. **Complete the proof.** Those confirmations feed
   `path.ConfirmProofFile(...)`, which upgrades the compact path into a full
   confirmed proof file. The exit having put every hop on chain is precisely
   what makes that upgrade possible.
3. **Spend the composed exit leaf.** One custom-anchor transition spends the
   output through its exit path into a fresh tapd-owned anchor, with both the
   script key and the internal key derived by tapd, so the units become
   ordinary wallet balance. The claim's miner fee comes out of the carrier
   (`CarrierValueSat - FeeSat`), and the result must stay at or above the
   taproot dust floor.

The claim is stateless and retried from scratch, so every step it takes is
idempotent against tapd.

Asset-bearing VTXOs are also refused by every Bitcoin-shaped flow at the RPC
edge (`rejectAssetBearingTargets`): explicit refresh, leave, and send-on-chain
targets are `InvalidArgument`, sweep-all filters them out, and the exit
preflight short-circuits, because an asset VTXO's worth is the units riding on
it rather than its carrier satoshis.

## The surface

**Daemon RPCs** (`waverpc`): `OnboardTaprootAsset`, `BoardTaprootAsset`, and
`ClaimTaprootAssetVTXO`, all granted under the `onchain:write` macaroon entity.
Asset sends ride the existing `SendOOR` with a `taproot_asset` intent and stay
under `oor:write`. Reads are folded into existing calls:
`GetBalance.taproot_assets` breaks holdings down by asset ref (carrier satoshis
stay counted in the Bitcoin fields), `ListVTXOs.asset_ref` filters, and each
`VTXO` carries a `taproot_asset` sub-message.

**CLI**: the `assets` group, with `taproot-assets` as an alias, holding
`onboard`, `board`, `claim`, `list`, `balance`, and `send`. It lives in the
advanced (hidden) group while the integration is a prototype, and stays fully
runnable in the shipped binary.

**Operator RPCs consumed** (`arkrpc`): `GetInfo.oor_carrier_pubkey` and
`LeaseOORCarrier`.

**Configuration**: `taprootassets.enabled` opts the runtime in, with
`taprootassets.host`, `.tlscertpath`, `.macaroonpath`, `.insecure`,
`.rpctimeout`, and `.preparationdir` (the durable commit journal, defaulting
under the network directory). Registration is lazy: `ConfigureTaprootAssets`
appends a service registrar, so no authenticated tapd connection or journal is
opened until the gRPC services start, and it is a no-op when a caller injected
its own preparer.

**Operational constraint**: assets must be minted at **asset version 1**. An
unconfirmed proof path needs a V1 base, and tapd's address default is V0, so
addresses have to be created with `--asset_version 1`. Nothing in the client
enforces this; a V0 base fails later and less clearly.

## Known gaps

**An OOR-received asset VTXO cannot be claimed after a unilateral exit.** Both
claim entry points require a non-empty `Descriptor.TaprootAssetSealedPackage`,
and only a round-created leaf persists one: the OOR incoming path sets the
asset root, ref, and amount but never the package, because the receiver was not
party to the ceremony that produced it. Such a leaf can still be spent onward
out of round, since a respend rebuilds its path from the sender's package.
After an exit, though, the sweep is withheld and the claim refuses, so the
carrier is strandable value. Closing this means persisting the receiving side's
package, not relaxing the check.

**Sends are bounded at eight asset inputs.** `oor.MaxTaprootAssetInputs = 8`
caps how many leaves one transfer can merge, and exceeding it is a
consolidate-first error rather than an automatic multi-transfer send. A wallet
that accumulates many small leaves therefore needs an explicit consolidation
step, which does not exist as a first-class command yet.

**Carrier floor economics are unmodelled.** Every new asset leaf costs the
operator one `MinVTXOAmountFloor()` of locked satoshis, reclaimed only when the
leaf is eventually spent. The client side is complete: the lease is validated,
the reclaim arithmetic is enforced, and the residual returns to the float. What
is missing is any pricing, rate limiting, or accounting for that float, so the
operator currently subsidizes asset sends. The related follow-up is the
zero-valued pay-to-anchor output the mixed package already carries: moving fee
responsibility onto P2A would let the floor shrink toward dust and change these
economics substantially.

**Onboarding requires an isolated asset per anchor.** `verifyInputs` requires
`len(anchor.Assets) == 1`. Loosening it means teaching the verifiers about
passive assets rather than deleting the check.

## See also

- [`tapassets/CLAUDE.md`](../tapassets/CLAUDE.md) — package-level invariants for
  the tapd integration.
- [`oor/CLAUDE.md`](../oor/CLAUDE.md) — the asset-transfer contract, the two
  input markers, and carrier allocation.
- [`waved/CLAUDE.md`](../waved/CLAUDE.md) — the RPC-edge invariants for
  onboarding, boarding, sending, and claiming.
- [`vtxo/CLAUDE.md`](../vtxo/CLAUDE.md) — the asset fields on `Descriptor` and
  their exclusion from Bitcoin coin selection.
- [`arkrpc/CLAUDE.md`](../arkrpc/CLAUDE.md) — the carrier lease surface.
- [`oor_subsystem.md`](oor_subsystem.md) — the per-session actor model the asset
  transfer path runs inside.
- [`arkscript_spec.md`](arkscript_spec.md) — the tapscript policy system the
  asset root is composed beside.
- The ExecPlans that built this, in order:
  [`taproot-assets-onboarding-execplan.md`](taproot-assets-onboarding-execplan.md),
  [`taproot-assets-carrier-onboarding-execplan.md`](taproot-assets-carrier-onboarding-execplan.md),
  [`taproot-assets-oor-execplan.md`](taproot-assets-oor-execplan.md),
  [`taproot-assets-carrier-selection-execplan.md`](taproot-assets-carrier-selection-execplan.md),
  [`taproot-assets-asset-state-execplan.md`](taproot-assets-asset-state-execplan.md),
  [`taproot-assets-partial-oor-execplan.md`](taproot-assets-partial-oor-execplan.md).
