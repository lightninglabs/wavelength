# Accepted round deadlines

A wallet reserves its inputs before joining a round. Operator acceptance
cancels the registration timer, but the operator may then stop sending messages.
The wallet therefore records a separate deadline for the accepted attempt.

The default participation budget is 30 minutes. `admissiontimeout` configures
that budget; zero or a negative value selects the default. Quote expiry bounds
the acceptance decision only; an accepted quote does not shorten the signing
budget. Repeated acceptance and quote reseals cannot extend the saved deadline.
The longer default allows slow or large rounds while bounding reservation
lifetime if the operator goes silent.

## Timeout and signature safety

Every accepted pre-checkpoint state checks the deadline before processing an
event. This includes the wait for the first quote and intermediate local signing
states. A one-second timer also sends a typed deadline check, so operator silence
does not prevent expiry. Timer delivery can be delayed by actor backpressure;
late events still check the deadline before advancing the state.

Before `InputSigSentState`, the wallet has not handed the operator its boarding
input or VTXO forfeit signatures. Expiry follows the existing failure path:
clean up ephemeral signing sessions, return safe reservations, and report the
failed attempt. A VTXO is a virtual transaction output held by the wallet.
Duplicate or late timer delivery cannot reopen a failed attempt.

`InputSigSentState` is the point of no return. The wallet commits that checkpoint
and closes admission in one database transaction before emitting signatures.
Admission expiry cannot release its inputs. The existing confirmation watch and
operator-status reconciliation continue to resolve that round. A timeout or lost
response is not evidence that the operator cannot complete a transaction.

## Restart and clocks

Pre-checkpoint MuSig2 signing sessions are ephemeral. Restart abandons those
attempts; this change does not add nonce or mid-signing recovery. Startup closes
their admission records before replaying pending wallet work. The existing VTXO
manager sweep releases reservations only under its checkpoint-aware recovery
rules. Any fresh authorized attempt uses a new round ID and fresh signing
sessions. Replaying an old admission cannot reopen it or renew its budget.

A checkpointed round resumes its existing reconciliation path. Deadline records
live in a separate table and never count as signature checkpoints.

Expiry is stored as an absolute UTC instant with nanosecond precision. The live
FSM also keeps a monotonic cutoff. A forward wall-clock correction is observed
on the next event or timer check. A backward correction cannot extend the
process-local budget. Restart terminates the interrupted attempt instead of
reconstructing a longer budget from a corrected clock.

## Compatibility

Migration 21 adds `round_admission_deadlines`. There is no wire-protocol change
or operator dependency. Existing signature checkpoints retain their recovery
behavior. Older binaries cannot open the migrated database because the existing
schema-version guard prevents downgrade. Back up the database before upgrading
when rollback to an older binary is required.

The regression suite covers every accepted state, silence after quote acceptance,
clock changes, persistence failure, duplicate and foreign timer delivery, late
operator messages, database reopen, checkpoint ambiguity, and real-daemon
reservation reuse after timeout or restart on either side of expiry.
