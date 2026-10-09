# Lean Receive Forfeit Signing Proof

This Lean project proves the admission boundary for receive-side forfeit
signing. A mailbox request is transport. It reaches the signer only when a
receive session has published a funded binding and the request matches that
binding in every authority-bearing field.

The main theorem is `admit_sound`. If `admit authority request` produces a
`SigningContext`, the theorem proves all of the following:

- `authority.published` contains the context's funded binding;
- the admitted request is the request supplied to `admit`;
- payment hash, outpoint, amount, script, policy, and policy-embedded payment
  hash satisfy `ExactBinding`.

The file also compile-checks rejection for missing authority and for each
single-field mismatch. `legacy_request_derived_counterexample` witnesses why a
request-derived rule is insufficient: it can manufacture an exact-looking
binding while published authority is absent.

## Implementation Bridge

Lean represents hashes, scripts, policies, and outpoints as opaque natural
number atoms because the theorem concerns equality and publication. The
production bridge closes that abstraction at the real Go boundary:

1. `bridgeVectors` evaluates the proved `admit` function and emits one decision
   for publication and every identity field.
2. `check.sh` requires that output to match `vectors.tsv`.
3. `sdk/swaps/forfeit_lean_bridge_test.go` replays the same vectors through
   `receiveForfeitBindingGate.load` and
   `validateOutSwapForfeitSignaturePayload`, including real vHTLC policy
   decoding for the policy-payment-hash case.

The bridge detects drift in either direction: changing Lean admission changes
the generated vectors, while changing Go admission changes the replay result.
The proof does not verify policy decoding, transaction construction, sighash
calculation, Schnorr signing, persistence, or concurrency. Those remain Go,
P-model, and cryptographic-library obligations.

## Verification

The project pins Lean `v4.34.1` in `lean-toolchain` and has no package
dependencies. Run the complete proof and implementation bridge from the
repository root:

```shell
./formal/lean/forfeitsigning/check.sh
```

For a proof-only build:

```shell
cd formal/lean/forfeitsigning
lake build
```
