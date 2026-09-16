// The role is 0 for a participant and 1 for an operator. A delivery may
// produce several effects; publication may repeat, but consumption may not.
event eRoundCheckpoint: (role: int, delivery: int);
event eRoundOutbox: (role: int, delivery: int, effect: int);
event eRoundConsumed: (role: int, delivery: int, effects: int);
event eRoundPublished: (role: int, delivery: int, effect: int);

// RoundCommitBoundary checks observations of committed facts, not attempted
// writes. Transaction rollback must therefore announce none of these facts.
spec RoundCommitBoundary observes eRoundCheckpoint, eRoundOutbox,
    eRoundConsumed, eRoundPublished {
    var checkpoints: map[(int, int), bool];
    var outbox: map[(int, int, int), bool];
    var consumed: map[(int, int), bool];

    start state Monitoring {
        on eRoundCheckpoint do (cp: (role: int, delivery: int)) {
            checkpoints[(cp.role, cp.delivery)] = true;
        }
        on eRoundOutbox do (o: (role: int, delivery: int, effect: int)) {
            outbox[(o.role, o.delivery, o.effect)] = true;
        }
        on eRoundConsumed do (c: (role: int, delivery: int, effects: int)) {
            var effect: int;
            assert !((c.role, c.delivery) in consumed),
                "round delivery applied twice";
            assert (c.role, c.delivery) in checkpoints,
                "round acknowledgement has no checkpoint";
            effect = 0;
            while (effect < c.effects) {
                assert (c.role, c.delivery, effect) in outbox,
                    "round acknowledgement lost an outgoing effect";
                effect = effect + 1;
            }
            consumed[(c.role, c.delivery)] = true;
        }
        on eRoundPublished do (o: (role: int, delivery: int, effect: int)) {
            assert (o.role, o.delivery, o.effect) in outbox,
                "round effect published without durable outbox";
        }
    }
}

// An operation may outlive a mailbox delivery. Its ownership must survive a
// crash even when the external signer outcome cannot be recovered. Release
// requires proof of safe termination; a timeout alone is not that proof.
event eRoundAdmitted: int;
event eRoundSigning: int;
event eRoundReleased: int;
event eRoundResolved: int;

spec RoundExclusiveAttempt observes eRoundAdmitted, eRoundSigning,
    eRoundReleased, eRoundResolved {
    var owner: int;
    var uncertain: bool;

    start state Monitoring {
        on eRoundAdmitted do (attempt: int) {
            assert owner == 0 || owner == attempt,
                "operation has overlapping attempts";
            owner = attempt;
        }
        on eRoundSigning do (attempt: int) {
            assert owner == attempt, "signer does not own operation";
            uncertain = true;
        }
        on eRoundReleased do (attempt: int) {
            assert owner == attempt, "release does not own operation";
            assert !uncertain, "uncertain signer released operation";
            owner = 0;
        }
        on eRoundResolved do (attempt: int) {
            assert owner == attempt, "resolution does not own operation";
            uncertain = false;
        }
    }
}
