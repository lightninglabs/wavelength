# Mailbox ingress safety and recovery

A mailbox acknowledgement lets the peer delete an envelope. For durable event
routes, the client first commits either the consumer handoff or the complete
rejected envelope in local quarantine. In-memory routes retain the durability
limit described below. Cursor consistency is checked before any dispatch; see
[the cursor contract](RPC_MAILBOX_CONTRACT.md#client-cursor-validation-and-omission-limits).

## Delivery occurrences and replay retention

A producer can opt into durable transport replay protection by setting `msg_id`
to `delivery-v1:` followed by a canonical UUID. It creates that ID once before
enqueuing a new delivery, persists it, and reuses it on every transport retry.
A separately enqueued delivery gets a new ID even if its body is identical.
The operation's `idempotency_key` stays separate and continues to protect the
operation itself.

This distinction matters for responses carried as events. Two independent
requests may receive the same error text. A body-derived key cannot distinguish
those fresh responses from a replay; suppressing all equal keys would break
recovery. Legacy untagged events and ordinary RPC requests/responses therefore
retain their existing behavior. They are not protected by the new receipts.

For tagged events, the client scopes the occurrence to the configured remote
and local mailbox, sender, service, and method. The first durable inbox insert
records the adapted message fingerprint and a receipt in the same transaction.
An identical retained occurrence does not enqueue another message. Reusing the
identity for a different adapted payload preserves the contradiction in
quarantine. The receiver's ordinary inbox UUID and `processed_messages` record
remain independent from the network receipt.

A receipt expires exactly 30 days after its first durable admission. Replays
within that window do not renew it. Expiry is checked during admission, even
if physical cleanup has not run. On startup and then hourly, ingress removes at
most 512 expired receipts. Migration creates empty new tables; previously
consumed identities cannot be reconstructed.

A replay after expiry can pass transport deduplication. Existing operation and
state-machine idempotency still apply, but this mechanism does not prove every
consumer is idempotent. A peer that changes its claimed occurrence identity
also escapes transport deduplication. These IDs are not authenticated proofs
of semantic uniqueness.

## Poison quarantine

A registered route that cannot decode or adapt an envelope retains the complete
serialized envelope and the first failure reason in `ingress_quarantine`.
Those writes and cursor advancement share the same transaction. Later healthy
envelopes can proceed. Consumer/store failures retain the existing retry path;
they do not become successful quarantine admissions.

Quarantine reserves at most 128 envelopes and 8 MiB of envelope bytes for each
connection lane. A second global bound caps the process at 256 envelopes and
16 MiB. One peer therefore cannot consume the whole process budget. At either
capacity, the client leaves additional poison unacknowledged. Healthy messages
can still be delivered when they do not sit behind a capacity failure in the
same pull. Existing evidence is never overwritten or expired. A peer can still
deny service to its own lane by withholding messages or filling it with invalid
input; bounded quarantine does not promise availability for that peer.

Each process start retries each retained envelope once. This is the bounded
recovery policy: deploy an adapter correction, then restart the client to
re-attempt retained inputs. A failed retry leaves the exact evidence in place
and logs its quarantine ID. A successful retry removes evidence only in the
same transaction that proves a durable inbox handoff. Do not edit cursor
checkpoints or delete quarantine rows to make a lane appear healthy.

The durable store exposes `ListIngressQuarantine` for inspection. For local
SQL diagnosis, list `id`, `lane`, `reason`, and `attempts` from
`ingress_quarantine`; `envelope` is the original `mailbox.pb.Envelope` protobuf.
`lane` hashes the configured local/remote pair, while the full envelope retains
its routing metadata. Startup logs include the retained ID and each recovery
outcome. This ingress evidence is separate from actor `dead_letters`: it has
not yet reached a consumer and must not be requeued as an actor codec payload.

## Consumer durability limit

Some routes deliver to in-memory actors. A successful `TryTell` creates no
receipt and cannot justify deleting quarantine evidence. Such recovered
messages remain retained and may be redelivered on another restart. Universal
replay suppression and definitive recovery for those routes require a durable
consumer handoff or an explicit consumer completion acknowledgement. This
change does not silently migrate consumer state machines or claim that an
in-memory handoff survives a crash.

## Compatibility

| Producer | Client | Behavior |
| --- | --- | --- |
| Legacy IDs | New client | Existing redelivery behavior; cursor validation and poison quarantine apply. |
| Tagged occurrence IDs | Old client | IDs remain opaque wire strings; existing delivery continues. |
| Tagged occurrence IDs | New client, durable route | Thirty-day occurrence receipts commit with the inbox. |
| Tagged occurrence IDs | New client, in-memory route | No durable consumption receipt; consumer durability remains required. |

There is no required coordinated wire-version rollout. Receipt protection on a
connection requires a producer that supplies stable occurrence IDs. Quarantine
recovery may require an adapter upgrade and, for in-memory targets, a later
consumer durability change.
