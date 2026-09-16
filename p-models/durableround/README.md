# Durable round boundary

This model specifies the storage and ownership boundary required by both
participant and operator round actors. The role identifier distinguishes
their checkpoints and deliveries. It does not yet model their protocol
states, signer sessions, timers, or network exchange.

The commit monitor requires every acknowledged delivery to have its checkpoint
and all outgoing effects committed. Publication may repeat after a crash; a
delivery may be applied only once. Failed transactions announce no committed
facts. The ownership monitor prohibits concurrent attempts for one operation
and prohibits releasing that operation while an external signing outcome is
uncertain. An authoritative resolution may clear that uncertainty.

Run from the repository root:

```shell
bash p-models/durableround/check.sh
```

The script runs 500 bounded histories and three negative controls. Each
negative control must produce its specific assertion, so a compiler failure
or an unrelated checker error cannot pass the check. Histories explore
precommit failure and repeated publication for both roles. They check safety
within the bound, not eventual completion.

`../durableactor/bridge/round_commit_test.go` exercises the real SQLite
transaction store. It fails after checkpoint persistence, outgoing enqueue,
and acknowledgement, then reopens the database and checks that none of those
facts survived. A successful commit must preserve all three. This verifies
the storage primitive independently of the abstract model. It does not yet
establish conformance of either production round actor; that requires driving
the migrated behaviors through their real durable mailboxes.

External wallet calls cannot join the SQLite transaction. Runtime durability
does not restore a lost external MuSig2 session or prove a signature was never
exposed. The operation ownership rule therefore remains necessary across
delivery IDs. The model makes no deadline guarantee about outbox publication:
protocol deadline enforcement belongs at the receiving transition.
