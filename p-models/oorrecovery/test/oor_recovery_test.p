// oor_recovery_test.p - Model-checking profiles for post-sign recovery.

enum RecoveryScenario {
    CanonicalRecovery,
    AuthoritySplitCounterexample,
    CrashDuringAuthorityTransitionCounterexample,
    OwnershipReleaseCounterexample,
    ConflictAdmissionCounterexample,
    TerminalSplitCounterexample,
    CrashDuringTerminalTransitionCounterexample,
    RepeatedCompletionCounterexample
}

machine RecoveryScenarioDriver {
    var lifecycle: OORRecoveryLifecycle;

    start state Init {
        entry (scenario: RecoveryScenario) {
            if (scenario == CanonicalRecovery) {
                lifecycle = new OORRecoveryLifecycle(SafeRecovery);
                RunThroughCompletion();

                // Notification and acknowledgement replay after terminal
                // recovery must not apply completion twice.
                send lifecycle, eNotify;
                send lifecycle, eAck;
            } else if (scenario == AuthoritySplitCounterexample) {
                lifecycle = new OORRecoveryLifecycle(
                    SplitAuthorityWrite
                );
                send lifecycle, eSubmit;
                send lifecycle, eLock;
            } else if (scenario ==
                CrashDuringAuthorityTransitionCounterexample) {

                lifecycle = new OORRecoveryLifecycle(SafeRecovery);
                send lifecycle, eSubmit;
                send lifecycle, eLock;
                send lifecycle, eCrash;
                send lifecycle, eRestart;
                send lifecycle, ePersistSignature;
            } else if (scenario == OwnershipReleaseCounterexample) {
                lifecycle = new OORRecoveryLifecycle(
                    ReleasePostSignOwnership
                );
                RunThroughRestart();
            } else if (scenario == ConflictAdmissionCounterexample) {
                lifecycle = new OORRecoveryLifecycle(
                    AdmitOwnedConflict
                );
                RunThroughConflict();
            } else if (scenario == TerminalSplitCounterexample) {
                lifecycle = new OORRecoveryLifecycle(SplitTerminalWrite);
                RunThroughFinalize();
            } else if (scenario ==
                CrashDuringTerminalTransitionCounterexample) {

                lifecycle = new OORRecoveryLifecycle(SafeRecovery);
                RunThroughFinalize();
                send lifecycle, eCrash;
                send lifecycle, eRestart;
                send lifecycle, eMaterialize;
            } else {
                lifecycle = new OORRecoveryLifecycle(
                    RepeatTerminalCompletion
                );
                RunThroughCompletion();
                send lifecycle, eNotify;
                send lifecycle, eAck;
            }

            goto Done;
        }
    }

    state Done {}

    fun RunThroughPostSign() {
        send lifecycle, eSubmit;
        send lifecycle, eLock;
        send lifecycle, ePersistSignature;
        send lifecycle, eDisclose;
    }

    fun RunThroughRestart() {
        RunThroughPostSign();
        send lifecycle, eCrash;
        send lifecycle, eRestart;
    }

    fun RunThroughConflict() {
        RunThroughRestart();
        send lifecycle, eConflictingAdmission;
    }

    fun RunThroughFinalize() {
        RunThroughConflict();
        send lifecycle, eFinalize;
    }

    fun RunThroughCompletion() {
        RunThroughFinalize();
        send lifecycle, eMaterialize;
        send lifecycle, eNotify;
        send lifecycle, eAck;
    }
}

machine OORRecoveryGreenDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(CanonicalRecovery);
            goto Done;
        }
    }

    state Done {}
}

machine OORAuthoritySplitDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(AuthoritySplitCounterexample);
            goto Done;
        }
    }

    state Done {}
}

machine OOROwnershipReleaseDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(OwnershipReleaseCounterexample);
            goto Done;
        }
    }

    state Done {}
}

machine OORCrashDuringAuthorityTransitionDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(
                CrashDuringAuthorityTransitionCounterexample
            );
            goto Done;
        }
    }

    state Done {}
}

machine OORConflictAdmissionDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(ConflictAdmissionCounterexample);
            goto Done;
        }
    }

    state Done {}
}

machine OORTerminalSplitDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(TerminalSplitCounterexample);
            goto Done;
        }
    }

    state Done {}
}

machine OORRepeatedCompletionDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(RepeatedCompletionCounterexample);
            goto Done;
        }
    }

    state Done {}
}

machine OORCrashDuringTerminalTransitionDriver {
    start state Init {
        entry {
            new RecoveryScenarioDriver(
                CrashDuringTerminalTransitionCounterexample
            );
            goto Done;
        }
    }

    state Done {}
}

// OORBridgeTraceExportDriver runs one deterministic canonical schedule. The
// lifecycle emits normalized bridge-step announcements and then fails with a
// sentinel after acknowledgement so the checker writes a counterexample.
machine OORBridgeTraceExportDriver {
    var lifecycle: OORRecoveryLifecycle;

    start state Init {
        entry {
            lifecycle = new OORRecoveryLifecycle(ExportCanonicalTrace);
            send lifecycle, eSubmit;
            send lifecycle, eLock;
            send lifecycle, ePersistSignature;
            send lifecycle, eDisclose;
            send lifecycle, eCrash;
            send lifecycle, eRestart;
            send lifecycle, eConflictingAdmission;
            send lifecycle, eFinalize;
            send lifecycle, eMaterialize;
            send lifecycle, eNotify;
            send lifecycle, eAck;
            goto Done;
        }
    }

    state Done {}
}

test tcOORRecovery [main=OORRecoveryGreenDriver]:
    assert AuthorityAndTerminalTransitionsAreAtomic,
        PostSignOwnershipSurvivesRestart,
        OwnedConflictIsRejected,
        TerminalCompletionIsIdempotent in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORRecoveryGreenDriver };

test tcOORAuthoritySplitCounterexample [main=OORAuthoritySplitDriver]:
    assert AuthorityAndTerminalTransitionsAreAtomic in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORAuthoritySplitDriver };

test tcOORCrashDuringAuthorityTransitionCounterexample
    [main=OORCrashDuringAuthorityTransitionDriver]:
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORCrashDuringAuthorityTransitionDriver };

test tcOOROwnershipReleaseCounterexample [main=OOROwnershipReleaseDriver]:
    assert PostSignOwnershipSurvivesRestart in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OOROwnershipReleaseDriver };

test tcOORConflictAdmissionCounterexample [main=OORConflictAdmissionDriver]:
    assert OwnedConflictIsRejected in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORConflictAdmissionDriver };

test tcOORTerminalSplitCounterexample [main=OORTerminalSplitDriver]:
    assert AuthorityAndTerminalTransitionsAreAtomic in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORTerminalSplitDriver };

test tcOORCrashDuringTerminalTransitionCounterexample
    [main=OORCrashDuringTerminalTransitionDriver]:
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORCrashDuringTerminalTransitionDriver };

test tcOORRepeatedCompletionCounterexample [main=OORRepeatedCompletionDriver]:
    assert TerminalCompletionIsIdempotent in
    { OORRecoveryLifecycle, RecoveryScenarioDriver,
      OORRepeatedCompletionDriver };

test tcOORBridgeTraceExport [main=OORBridgeTraceExportDriver]:
    { OORRecoveryLifecycle, OORBridgeTraceExportDriver };
