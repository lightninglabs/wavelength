# Custom Scripting State

This note captures the current implementation state for custom-script VTXOs,
especially vHTLC-like policies, and the next steps for productionizing OOR,
settlement, and round forfeits.

## Working Terminology

This note uses old-Ark terminology:

- `OOR send` creates the off-round output
- `settle` means later enrolling that OOR-created output into a normal round
- settlement does not need to change the custom spend semantics of the live
  output; it mainly reduces trust by tying the output into the normal
  connector/forfeit model

This is important: in this note, `settle` does not mean "collapse the script
to a winner-owned VTXO". It means "join a round so the live output is covered
by normal Ark round safety again".

For the current swap work, the practical default is narrower:

- unresolved live vHTLCs do not need a neutral settlement mode to ship swaps
- the default settlement mode is side-specific and follows the natural HTLC
  outcome
- a future full-multisig settlement mode can later re-anchor a live vHTLC
  without revealing the preimage or choosing claim vs refund early

## Current Model

The current OOR model is:

1. The old VTXO is spent into a checkpoint transaction.
2. The checkpoint input is where the old script path is actually fulfilled.
3. The Ark transaction then spends the checkpoint output and creates the new
   recipient outputs.

For a custom spend such as a vHTLC preimage claim, the checkpoint input carries
the custom witness material, and the server validates that witness in the
Bitcoin script engine during finalize.

Relevant code:

- [client/lib/tx/checkpoint/build.go](../client/lib/tx/checkpoint/build.go)
- [client/oor/checkpoint_sign.go](../client/oor/checkpoint_sign.go)
- [oor/finalize_signature_validation.go](../oor/finalize_signature_validation.go)
- [client/lib/tx/oor/build.go](../client/lib/tx/oor/build.go)

## Answer To The Core Question

For custom scripts, the checkpoint transaction is the transaction that fulfills
the old script. The Ark transaction does not re-fulfill the old script. It only
spends the checkpoint output and creates the refreshed or transferred outputs.

So for a vHTLC:

- old vHTLC claim/refund path is satisfied on the checkpoint input
- checkpoint output becomes the temporary custody object
- Ark tx spends that checkpoint and creates the next output set

This matches the current darepo OOR implementation.

## Immediate Decisions

The following questions are now answered for the current design.

### Who May Settle A Custom Script?

For a two-party custom output such as a vHTLC between Alice and Bob, both
participants should be allowed to initiate settlement.

That means:

- Alice may drive timeout-side settlement
- Bob may drive preimage-side settlement

More generally:

- every non-operator participant key that appears in a valid settlement pair
  may initiate settlement
- the indexer/auth implication is the same: those participant keys should be
  allowed to inspect the output metadata

### What Does Settlement Use?

Settlement should use the spend semantics that are already present on the live
custom output.

For a participant settling a live custom output into a round, there are two
relevant paths:

- an auth/proof path that the participant can satisfy alone
- a forfeit path that the operator can complete later if the participant cheats

For a vHTLC this means:

- Bob's preimage-side settlement should use:
  - unilateral preimage path for proof/auth
  - cooperative preimage path for the round forfeit tx
- Alice's timeout-side settlement should use:
  - unilateral timeout path for proof/auth
  - cooperative timeout path for the round forfeit tx

So settlement does not have to collapse the output into a simpler owner model
first. It can operate directly on the live custom policy as long as the policy
contains the paired Ark branches needed for:

- local proof of control
- operator-backed forfeit enforcement

### Default vHTLC Settlement Mode

For the current swap-oriented vHTLC, the default settlement model is
intentionally side-specific rather than neutral:

- Bob may settle before expiry using the preimage side
- Alice may settle only after the refund CLTV has matured using the timeout
  side

This matches the intended swap semantics:

- Bob-side settlement means the success branch is being exercised
- Alice-side settlement means the refund branch is being exercised

This is a good default for swaps because it minimizes coordination and matches
the natural business resolution of the HTLC.

The tradeoff is that Bob-side settlement reveals the preimage to the operator.
That is acceptable for the default swap-success path, but it is not a neutral
privacy-preserving settlement of an unresolved contract.

This is also why the current default is acceptable for Lightning swaps but not
the final story for generic custom contracts:

- swap success already implies Bob is exercising the preimage side
- swap timeout already implies Alice is exercising the refund side
- generic neutral "make this less trusted without resolving it" should be
  treated as a later extension

### Future Neutral Settlement Mode

If we later want to settle a live vHTLC into a round without revealing the
preimage or choosing the claim/refund outcome, we should add a separate
full-multisig settlement mode.

That mode would use a dedicated cooperative settlement branch and require both
Alice and Bob to participate in the settlement/forfeit signing flow.

So the intended split is:

- default mode: unilateral participant-based settlement for swaps
- future addon: full multisig settlement mode for neutral/privacy-preserving
  re-anchoring

## Forfeit Generation

We do want deterministic settlement/forfeit derivation from semantic policies.
With the current AST model, candidate extraction is straightforward because
every leaf is a serialized tree over:

- `Condition`
- `CSV`
- `Multisig`

So for a valid Ark policy we can mechanically:

1. decode the serialized policy
2. identify leaves keyed by participant
3. identify which of those leaves also contain the operator
4. pair unilateral and operator-backed leaves that represent the same business
   branch
5. compile those leaves to tapscript/control blocks

The paired-branch rule is the key idea:

- unilateral leaf proves the participant can authorize settlement
- operator-backed sibling leaf becomes the forfeit path for the later round

The practical restriction is that a valid Ark custom policy must expose those
paired branches explicitly. A leaf that contains the operator may still be
unsuitable as a settlement forfeit path if it depends on:

- a secret the operator does not know
- a counterparty signature the operator cannot force
- a timeout that is not yet mature

So the right project statement is:

- every valid Ark custom script should support deterministic extraction of its
  participant settlement pairs
- each pair should contain:
  - a participant-only proof/auth leaf
  - an operator-backed forfeit leaf for the same branch

For the current vHTLC shape, this is straightforward because the policy already
has those paired branches:

- `receiver`:
  - auth: `preimage + receiver + ark CSV`
  - forfeit: `preimage + receiver + operator`
- `sender`:
  - auth: `timeout + sender + ark CSV`
  - forfeit: `timeout + sender + operator`

This is the default settlement model we should build around.

For swap use cases, this is likely the only mode we need initially. A neutral
multisig settlement mode can be layered on later without blocking the default
swap flow.

## Default Forfeit Leaf

There is no single global "default forfeit leaf" for arbitrary custom
policies.

The correct default is:

- for the chosen settlement participant, use the operator-backed sibling of
  that participant's unilateral/auth branch

For standard VTXOs, that degenerates to the familiar:

- `owner + operator`

For custom policies, the forfeit output script remains external and unchanged:

- the round forfeit tx still pays to the server's forfeit output script
- what changes is the input leaf used to spend the settled custom VTXO into
  that tx

## What Works Today

Today, OOR custom-script refresh is conceptually supportable:

- the client can attach a custom spend path for the old VTXO spend
- the operator can co-sign and validate the exact revealed script path
- finalize runs the full witness through `txscript`
- the checkpoint output can carry semantic owner-leaf policy sidecar data for
  later reconstruction

The practical implication is:

- OOR custom outputs are already compatible with checkpoint/Ark construction
- round settlement and forfeits are the next place that needs custom-policy
  awareness

## What Does Not Yet Generalize

Batch settlement and forfeits are only partially generalized today.

What now works:

- the policy layer can derive participant settlement pairs from the serialized
  AST
- client join-auth can use a custom unilateral settlement path
- client forfeit construction can use a custom operator-backed settlement path
- indexer auth can derive participant query subjects from persisted policy
  bytes
- server forfeit completion can reconstruct that chosen path from witness
  metadata and validate the completed spend in `txscript`

What still does not generalize fully:

- the chosen settlement pair is still local client metadata rather than an
  explicit server-understood round concept
- server-side validation for arbitrary custom settlement paths still relies
  primarily on the revealed witness path and the completed script-engine check
- standard-policy forfeits now use the semantic owner key from the policy
  template, but fully general custom-policy semantic validation is still
  incomplete

Relevant code:

- [client/round/transitions.go](../client/round/transitions.go)
- [client/lib/tx/forfeit.go](../client/lib/tx/forfeit.go)

This means the hard open problem is not "can checkpoint spend an arbitrary
script?" The hard problem is "how does the round flow consume the participant's
chosen settlement pair instead of assuming the standard VTXO collab leaf?"

## Important Distinction: Operator Leaf vs Settlement Pair

Not every leaf that contains the operator is automatically a valid settlement
forfeit leaf in isolation.

The safe rule is narrower:

- a valid settlement forfeit leaf is the operator-backed sibling of the
  participant's chosen auth branch

That is why the policy layer should expose actual settlement pairs rather than
just "all leaves containing operator".

## Comparison With Old Ark

Old Ark modeled this more explicitly:

- custom policies exposed operator-side closures and exit closures
- settlement into a round implicitly relied on those closures being paired
- batch forfeit construction derived its spend material from the operator-side
  member of that pair

Relevant old Ark references:

- `/home/kon-dev/lightninglabs/ark/sdk/vhtlc/vhtlc.go`
- `/home/kon-dev/lightninglabs/ark/sdk/batch_session.go`

One important caveat from reviewing old Ark: its generic settlement path was
fairly crude. It effectively relied on `ForfeitClosures()[0]` for custom
scripts, which works for standard cases but does not amount to a clean general
three-party neutral-settlement protocol for vHTLCs.

That is directionally useful for darepo as well.

## Current Implementation Direction

The current implementation direction is:

1. Serialize the semantic policy AST.
2. Derive settlement pairs from that serialized policy.
3. Use the unilateral member of the pair for join-auth proof of control.
4. Use the operator-backed member of the pair for round forfeit signing.
5. Keep the live custom policy semantics unchanged across settlement.

For vHTLCs, "unchanged across settlement" should be read as:

- default settlement follows the winning/available branch
- Bob-side settlement is claim-side settlement
- Alice-side settlement is timeout-side settlement
- neutral re-anchoring is not required for the first production swap path

This keeps the AST useful for:

- developer ergonomics
- deterministic custom-script compilation
- settlement-pair derivation
- participant extraction for indexer auth

## Implementation Status

The current codebase now has a meaningful first implementation of this model:

- `client/lib/arkscript/settlement.go` derives participant settlement pairs
  from the serialized AST
- client round/join-auth can carry a custom unilateral auth path
- client forfeit creation can carry a custom operator-backed forfeit path
- indexer auth derives queryable participant subjects from persisted policy
  templates
- server forfeit completion can reconstruct that custom path from witness
  metadata and validate the completed witness in `txscript`
- standard VTXO forfeits use the owner key from the policy template for the
  collaborative leaf instead of assuming the ephemeral tree signing key

What is still incomplete:

- round registration still treats the custom forfeit path as local metadata
  rather than a first-class wire-level concept
- server-side validation before completion still does not understand arbitrary
  custom settlement paths semantically; it relies on the revealed witness path
  and the completed script-engine check
- the server still does not carry an explicit semantic "chosen settlement pair"
  concept through the round lifecycle

## Remaining Work

The next custom-scripting tranche should answer these questions explicitly:

1. Promote the chosen settlement pair from local client metadata to an explicit
   round-level concept if we decide the server should reason about it before
   witness completion.

2. Decide how much semantic pre-validation the server should do for arbitrary
   custom settlement paths before it falls back to witness revelation and
   `txscript` execution.

3. Add end-to-end settlement tests for the default vHTLC settlement model and
   valid completed forfeits.

4. Later, if we want neutral re-anchoring of unresolved live contracts, add a
   full-multisig settlement mode as a separate extension.

## Current Working Conclusion

The current architecture is still:

- checkpoint fulfills the old script
- Ark tx spends the checkpoint
- refreshed outputs can themselves be custom-script outputs

So the next implementation step is not a rethink of checkpoint vs Ark tx. The
next step is to make round settlement consume semantic custom policies by
deriving:

- participant proof/auth paths
- operator-backed forfeit paths
- indexer authorization subjects
