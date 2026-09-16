// The driver varies crashes before commit and repeated outbox publication
// for both roles. Its bounded history is a safety check, not a liveness proof.
machine RoundCommitHistory {
    start state Init {
        entry {
            var step: int;
            var role: int;
            var delivery: int;
            var effect: int;
            var effects: int;
            var next: map[int, int];

            next[0] = 1;
            next[1] = 1;
            step = 0;
            announce eRoundAdmitted, 1;
            while (step < 24) {
                role = choose(2);
                delivery = next[role];
                effects = choose(3);
                if ($) {
                    // A crash or failed commit leaves the delivery pending.
                    // The next read restores its last committed checkpoint.
                } else {
                    announce eRoundCheckpoint,
                        (role = role, delivery = delivery);
                    effect = 0;
                    while (effect < effects) {
                        announce eRoundOutbox,
                            (role = role, delivery = delivery,
                             effect = effect);
                        effect = effect + 1;
                    }
                    announce eRoundConsumed,
                        (role = role, delivery = delivery,
                         effects = effects);
                    effect = 0;
                    while (effect < effects) {
                        announce eRoundPublished,
                            (role = role, delivery = delivery,
                             effect = effect);
                        if ($) {
                            // Crash after sending but before marking sent.
                            announce eRoundPublished,
                                (role = role, delivery = delivery,
                                 effect = effect);
                        }
                        effect = effect + 1;
                    }
                    next[role] = delivery + 1;
                }
                step = step + 1;
            }
            if ($) {
                announce eRoundSigning, 1;
                // A cold restart cannot infer external signer completion.
                // Keep the owner until an authoritative resolution arrives.
                announce eRoundAdmitted, 1;
                announce eRoundResolved, 1;
            }
            announce eRoundReleased, 1;
            announce eRoundAdmitted, 2;
            goto Done;
        }
    }
    state Done {}
}

machine RoundLostOutbox {
    start state Init {
        entry {
            announce eRoundCheckpoint, (role = 0, delivery = 1);
            announce eRoundConsumed, (role = 0, delivery = 1, effects = 1);
        }
    }
}

machine RoundOverlappingFallback {
    start state Init {
        entry {
            announce eRoundAdmitted, 1;
            announce eRoundAdmitted, 2;
        }
    }
}

machine RoundUncertainRelease {
    start state Init {
        entry {
            announce eRoundAdmitted, 1;
            announce eRoundSigning, 1;
            announce eRoundReleased, 1;
        }
    }
}

test tcRoundCommitHistory [main=RoundCommitHistory]:
    assert RoundCommitBoundary, RoundExclusiveAttempt in {RoundCommitHistory};
test tcRoundLostOutbox [main=RoundLostOutbox]:
    assert RoundCommitBoundary in {RoundLostOutbox};
test tcRoundOverlappingFallback [main=RoundOverlappingFallback]:
    assert RoundExclusiveAttempt in {RoundOverlappingFallback};
test tcRoundUncertainRelease [main=RoundUncertainRelease]:
    assert RoundExclusiveAttempt in {RoundUncertainRelease};
