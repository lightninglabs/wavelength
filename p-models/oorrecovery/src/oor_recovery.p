// oor_recovery.p - Post-sign OOR ownership and terminal recovery model.
//
// The model treats lock plus signature persistence as one durable authority
// transition. It likewise treats finalization plus package materialization as
// one terminal transition. The paired events expose the two observations a
// production trace records, while durable state changes only at the second
// event in each pair.

enum RecoveryPolicy {
    SafeRecovery,
    SplitAuthorityWrite,
    ReleasePostSignOwnership,
    AdmitOwnedConflict,
    SplitTerminalWrite,
    RepeatTerminalCompletion,
    ExportCanonicalTrace
}

type RecoveryState = (
    post_sign_ever: bool,
    authority_pending: bool,
    durable_lock: bool,
    durable_signature: bool,
    terminal_pending: bool,
    finalized: bool,
    materialized: bool,
    completion_count: int
);

event eSubmit;
event eLock;
event ePersistSignature;
event eDisclose;
event eCrash;
event eRestart;
event eConflictingAdmission;
event eFinalize;
event eMaterialize;
event eNotify;
event eAck;

event eBridgeStep: (op: string, expect: string);
event eRecoveryStateObserved: RecoveryState;
event eConflictingAdmissionObserved: (
    post_sign_owned: bool,
    admitted: bool
);

machine OORRecoveryLifecycle {
    var policy: RecoveryPolicy;
    var submitted: bool;
    var post_sign_ever: bool;
    var authority_pending: bool;
    var durable_lock: bool;
    var durable_signature: bool;
    var disclosed: bool;
    var terminal_pending: bool;
    var finalized: bool;
    var materialized: bool;
    var notified: bool;
    var acked: bool;
    var completion_count: int;

    start state Running {
        entry (selected_policy: RecoveryPolicy) {
            policy = selected_policy;
        }

        on eSubmit do HandleSubmit;
        on eLock do HandleLock;
        on ePersistSignature do HandlePersistSignature;
        on eDisclose do HandleDisclose;
        on eCrash do HandleCrash;
        on eRestart do HandleRestart;
        on eConflictingAdmission do HandleConflictingAdmission;
        on eFinalize do HandleFinalize;
        on eMaterialize do HandleMaterialize;
        on eNotify do HandleNotify;
        on eAck do HandleAck;
    }

    fun ObserveState() {
        announce eRecoveryStateObserved, (
            post_sign_ever = post_sign_ever,
            authority_pending = authority_pending,
            durable_lock = durable_lock,
            durable_signature = durable_signature,
            terminal_pending = terminal_pending,
            finalized = finalized,
            materialized = materialized,
            completion_count = completion_count
        );
    }

    fun ExportStep(op: string, expect: string) {
        if (policy == ExportCanonicalTrace) {
            announce eBridgeStep, (op = op, expect = expect);
        }
    }

    fun HandleSubmit() {
        assert !submitted, "a transfer may be submitted only once";
        submitted = true;
        ExportStep("submit", "");
        ObserveState();
    }

    fun HandleLock() {
        assert submitted && !authority_pending && !durable_lock,
            "lock requires one submitted transfer";

        authority_pending = true;
        if (policy == SplitAuthorityWrite) {
            durable_lock = true;
        }

        ExportStep("lock", "");
        ObserveState();
    }

    fun HandlePersistSignature() {
        assert authority_pending,
            "signature persistence must complete the lock transition";

        durable_lock = true;
        durable_signature = true;
        post_sign_ever = true;
        authority_pending = false;
        ExportStep("persist_signature", "owned");
        ObserveState();
    }

    fun HandleDisclose() {
        assert durable_lock && durable_signature,
            "disclosure requires durable post-sign ownership";

        disclosed = true;
        ExportStep("disclose", "");
        ObserveState();
    }

    fun HandleCrash() {
        // Pending work is process-local. Neither atomic transition publishes
        // a durable partial state before its commit observation.
        authority_pending = false;
        terminal_pending = false;
        ExportStep("crash", "");
        ObserveState();
    }

    fun HandleRestart() {
        if (policy == ReleasePostSignOwnership && post_sign_ever &&
            !materialized) {

            durable_lock = false;
            durable_signature = false;
        }

        ExportStep("restart", "owned");
        ObserveState();
    }

    fun HandleConflictingAdmission() {
        var post_sign_owned: bool;
        var admitted: bool;

        post_sign_owned = durable_lock && durable_signature && !materialized;
        admitted = !post_sign_owned;
        if (policy == AdmitOwnedConflict && post_sign_owned) {
            admitted = true;
        }

        announce eConflictingAdmissionObserved, (
            post_sign_owned = post_sign_owned,
            admitted = admitted
        );
        ExportStep("conflicting_admission", "rejected");
        ObserveState();
    }

    fun HandleFinalize() {
        assert disclosed && durable_lock && durable_signature,
            "finalization requires disclosed post-sign authority";

        terminal_pending = true;
        if (policy == SplitTerminalWrite) {
            finalized = true;
        }

        ExportStep("finalize", "");
        ObserveState();
    }

    fun HandleMaterialize() {
        assert terminal_pending,
            "materialization must complete the finalization transition";

        finalized = true;
        materialized = true;
        terminal_pending = false;
        ExportStep("materialize", "terminal");
        ObserveState();
    }

    fun HandleNotify() {
        assert materialized,
            "recipient notification requires a materialized package";

        notified = true;
        ExportStep("notify", "");
        ObserveState();
    }

    fun HandleAck() {
        assert notified, "acknowledgement requires notification";

        if (!acked) {
            acked = true;
            completion_count = completion_count + 1;
        } else if (policy == RepeatTerminalCompletion) {
            completion_count = completion_count + 1;
        }

        ExportStep("ack", "completed");
        ObserveState();
        assert policy != ExportCanonicalTrace,
            "OOR_BRIDGE_EXPORT_COMPLETE";
    }
}

// AuthorityAndTerminalTransitionsAreAtomic rejects every observation in which
// only one half of either durable transition is visible.
spec AuthorityAndTerminalTransitionsAreAtomic observes
    eRecoveryStateObserved {

    start state Monitoring {
        on eRecoveryStateObserved do (observed: RecoveryState) {
            assert observed.durable_lock == observed.durable_signature,
                "lock and signature persistence split across durable states";
            assert observed.finalized == observed.materialized,
                "finalize and materialize split across durable states";
        }
    }
}

// PostSignOwnershipSurvivesRestart requires a non-terminal transfer to retain
// its durable ownership after its signature has ever been persisted.
spec PostSignOwnershipSurvivesRestart observes eRecoveryStateObserved {
    start state Monitoring {
        on eRecoveryStateObserved do (observed: RecoveryState) {
            if (observed.post_sign_ever && !observed.materialized) {
                assert observed.durable_lock &&
                    observed.durable_signature,
                    "restart released post-sign ownership";
            }
        }
    }
}

// OwnedConflictIsRejected prevents a second transfer from taking authority
// while the post-sign transfer remains non-terminal.
spec OwnedConflictIsRejected observes eConflictingAdmissionObserved {
    start state Monitoring {
        on eConflictingAdmissionObserved do (decision: (
            post_sign_owned: bool,
            admitted: bool
        )) {
            if (decision.post_sign_owned) {
                assert !decision.admitted,
                    "conflicting admission replaced post-sign ownership";
            }
        }
    }
}

// TerminalCompletionIsIdempotent permits replayed notification and
// acknowledgement without applying terminal completion more than once.
spec TerminalCompletionIsIdempotent observes eRecoveryStateObserved {
    start state Monitoring {
        on eRecoveryStateObserved do (observed: RecoveryState) {
            assert observed.completion_count <= 1,
                "terminal completion was applied more than once";
        }
    }
}
