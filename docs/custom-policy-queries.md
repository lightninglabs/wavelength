# Query a custom output before funding

A client must prove ownership before it can query an unfunded custom Taproot
output. Supply its canonical `policy_template` and exact `pk_script` to
`GetIndexedVTXOByPkScript`. The daemon reconstructs the output locally and signs
with its identity key. The operator accepts only a non-operator participant
with a valid operator-backed settlement pair in that policy.

The signed TLV proof uses the distinct type `policy_script_scope`. It commits
the script, policy (record 12), participant key, server, principal, purpose,
nonce, issue time and short expiry. It is accepted only for
`ListVTXOsByScripts`. Existing `script_scope` proofs and ordinary wallet receive
registrations retain their behavior. Each read creates a fresh proof; no ACL
row, subscription or registration lifetime is involved.

Callers that supply a policy must require `policy_authorized = true` in the
response, even when no VTXO is returned. An old daemon ignores unknown request
fields and leaves the flag false. An old operator rejects the new proof type.
Deploy operator support before the daemon and policy-query callers.

A successful empty response is an observation at one database snapshot. A
co-signed pending transfer remains inconclusive until finalization materializes
its output. Timeouts, authorization failures and unavailable responses cannot
prove absence. A later transfer can still fund the script, so applications must
retain their late-funding and recovery rules. Materialized outputs retain their
persisted policy authorization.

This RPC is a read under the existing `vtxo:read` permission. It never funds or
spends an output. Ordinary `NewReceiveScript` registrations still serve wallet
receives and event routing, with their existing quotas and retention policy.
