#!/usr/bin/env bash
# Run the P models and their Go bridge conformance tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(dirname "$PROJECT_DIR")"
MAILBOX_P_PROJ="${PROJECT_DIR}/durableactor/infra.pproj"
FORFEIT_P_PROJ="${PROJECT_DIR}/forfeitsigning/infra.pproj"
FORFEIT_MODEL_PATH="${PROJECT_DIR}/forfeitsigning/src/forfeit_signing.p"
FORFEIT_SPEC_PATH="${PROJECT_DIR}/forfeitsigning/SPEC.md"
OOR_RECOVERY_P_PROJ="${PROJECT_DIR}/oorrecovery/infra.pproj"
BUILD_DIR="${REPO_ROOT}/PGenerated/PChecker/net8.0"
MAILBOX_DLL_PATH="${BUILD_DIR}/MailboxInfraModels.dll"
FORFEIT_DLL_PATH="${BUILD_DIR}/ForfeitSigningModels.dll"
OOR_RECOVERY_DLL_PATH="${BUILD_DIR}/OORRecoveryModels.dll"

SCHEDULES="${SCHEDULES:-50}"
MAX_STEPS="${MAX_STEPS:-700}"
TIMEOUT="${TIMEOUT:-300}"

cd "$REPO_ROOT"

echo "=== P Model Checking ==="
echo "Bounds:"
echo "  SCHEDULES: $SCHEDULES"
echo "  MAX_STEPS: $MAX_STEPS"
echo "  TIMEOUT: ${TIMEOUT}s"
echo ""

if ! command -v p >/dev/null 2>&1; then
    echo "Error: P compiler not found"
    echo "Install with: dotnet tool install --global P --version 3.0.4"
    exit 1
fi

run_with_heartbeat() {
    local label="$1"

    shift

    "$@" &
    local cmd_pid="$!"

    (
        while kill -0 "$cmd_pid" >/dev/null 2>&1; do
            sleep 30

            if kill -0 "$cmd_pid" >/dev/null 2>&1; then
                echo "... ${label} still running ($(date -u +%H:%M:%SZ))"
            fi
        done
    ) &
    local heartbeat_pid="$!"

    local status=0

    wait "$cmd_pid" || status="$?"

    kill "$heartbeat_pid" >/dev/null 2>&1 || true
    wait "$heartbeat_pid" 2>/dev/null || true

    return "$status"
}

# sha256_file prints the SHA-256 digest with the utility available on the
# current host.
sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# check_forfeit_spec_pin prevents the normative specification from silently
# referring to a different revision of its authoritative model.
check_forfeit_spec_pin() {
    local actual
    local pinned

    actual="$(sha256_file "$FORFEIT_MODEL_PATH")"
    pinned="$(sed -n 's/^`\([0-9a-f]\{64\}\)`$/\1/p' \
        "$FORFEIT_SPEC_PATH")"

    if [ -z "$pinned" ] || [ "${pinned#*$'\n'}" != "$pinned" ]; then
        echo "ERROR: expected exactly one model SHA-256 in ${FORFEIT_SPEC_PATH}"
        return 1
    fi

    if [ "$actual" != "$pinned" ]; then
        echo "ERROR: forfeit signing specification model digest is stale"
        echo "  expected: $actual"
        echo "  pinned:   $pinned"
        return 1
    fi
}

EXPECTED_P_VERSION="3.0.4"
P_VERSION="$(p --version 2>/dev/null | awk '/^P version / {print $NF; exit}')"
case "$P_VERSION" in
    "${EXPECTED_P_VERSION}")
        TRACE_P_VERSION="$P_VERSION"
        ;;
    "${EXPECTED_P_VERSION}.0")
        TRACE_P_VERSION="$EXPECTED_P_VERSION"
        ;;
    *)
        echo "Error: expected P ${EXPECTED_P_VERSION}, got ${P_VERSION:-unknown}"
        exit 1
        ;;
esac

check_forfeit_spec_pin

rm -rf "${REPO_ROOT}/PGenerated"
run_with_heartbeat "mailbox P compile" p compile -pp "$MAILBOX_P_PROJ"

# check_green runs a test case that must hold: p check exits non-zero if it
# finds any bug, so set -e fails the script on a regression.
check_green() {
    local testcase="$1"
    local dll_path="${2:-$MAILBOX_DLL_PATH}"

    echo ""
    echo "=== green: ${testcase} (expect 0 bugs) ==="
    run_with_heartbeat "$testcase" timeout "$TIMEOUT" p check "$dll_path" \
        --testcase "$testcase" \
        --schedules "$SCHEDULES" \
        --max-steps "$MAX_STEPS"
}

# check_negative runs a test case that must find a bug. P reports a discovered
# bug with exit 1 and a checker diagnostic. Requiring both keeps a timeout or
# tool failure from masquerading as the counterexample this test exists to
# preserve.
check_negative() {
    local testcase="$1"
    local schedules="${2:-$SCHEDULES}"
    local dll_path="${3:-$MAILBOX_DLL_PATH}"
    local expected_assertion="${4:-}"
    local model_name
    local output
    local trace_dir
    local trace_txt
    local status=0

    output="$(mktemp "${TMPDIR:-/tmp}/p-negative.XXXXXX")"
    trace_dir="$(mktemp -d "${TMPDIR:-/tmp}/p-negative-trace.XXXXXX")"
    model_name="$(basename "$dll_path" .dll)"
    trace_txt="${trace_dir}/BugFinding/${model_name}_0_0.txt"

    echo ""
    echo "=== negative: ${testcase} (expect a bug) ==="
    run_with_heartbeat "$testcase" timeout "$TIMEOUT" \
        p check "$dll_path" \
        --testcase "$testcase" \
        --schedules "$schedules" \
        --max-steps "$MAX_STEPS" \
        --outdir "$trace_dir" >"$output" 2>&1 || status="$?"

    cat "$output"

    if [ "$status" -eq 0 ]; then
        rm -f "$output"
        rm -rf "$trace_dir"
        echo "ERROR: ${testcase} found no bug, but a bug was expected"
        return 1
    fi

    if [ "$status" -ne 1 ]; then
        rm -f "$output"
        rm -rf "$trace_dir"
        echo "ERROR: ${testcase} exited ${status}; expected checker bug exit 1"
        return 1
    fi

    if ! grep -Fq "Checker found a bug." "$output" ||
        ! grep -Fq "Found 1 bug." "$output"; then

        rm -f "$output"
        rm -rf "$trace_dir"
        echo "ERROR: ${testcase} failed without a checker bug diagnostic"
        return 1
    fi

    if [ -n "$expected_assertion" ] &&
        ! grep -Fq "$expected_assertion" "$trace_txt"; then

        rm -f "$output"
        rm -rf "$trace_dir"
        echo "ERROR: ${testcase} found the wrong counterexample"
        return 1
    fi

    rm -f "$output"
    rm -rf "$trace_dir"
    echo "OK: ${testcase} found the expected bug"
}

# Safety and liveness properties must hold.
check_green tcMailboxCorrelationKeyFIFO
check_green tcMailboxLiveness

# The Read/Commit consume step must apply a message's behavior effect exactly
# once even when the row's lease expires mid-IO and the row is reclaimed and
# reprocessed: the stale consumer's lease-fenced commit must be an ErrLeaseLost
# no-op.
check_green tcMailboxReadCommitFence

# The legacy reorder must still be caught two independent ways: once by the
# in-machine assertion, and once by the SameKeyFIFOClaimsRespectLiveHead monitor
# with no in-machine assertion. A single schedule is enough to surface it.
check_negative tcMailboxLegacyReorderCounterexample 1
check_negative tcMailboxMonitorCatchesLegacyReorder 1

# The unfenced-commit counterexample must be caught by the
# LeaseFencedCommitAppliesEffectAtMostOnce monitor: a stale consumer that
# applies its effect after the row was reclaimed double-applies it.
check_negative tcMailboxUnfencedCommitCounterexample 1

# The early-durable-write (Stage) path must replay safely: a checkpoint Staged
# and broadcast, then crashed before Commit, is reclaimed and replayed without
# double-broadcasting or regressing the checkpoint, and consumed exactly once.
check_green tcMailboxStageCommitExactlyOnce

# The unstable-broadcast counterexample must be caught by the
# StagedEffectAppliedAtMostOnceUnderReplay monitor: a behavior that re-derives a
# fresh broadcast id on replay double-broadcasts.
check_negative tcMailboxStagedDoubleBroadcastCounterexample 1

# The unfenced-stage counterexample must be caught by the
# CheckpointAdvancesMonotonically monitor: a stale consumer whose stage is not
# lease-fenced overwrites a newer owner's checkpoint with an older level.
check_negative tcMailboxStaleStageRegressesCounterexample 1

# The CDC outbox fold must commit the target enqueue and the outbox completion
# atomically: a failed fold rolls back with no orphan and redelivers after claim
# expiry, completion is token-fenced, and the target is delivered exactly once.
check_green tcOutboxFold

# The split-write counterexample must be caught by the
# OutboxCompletionImpliesDelivery monitor: a non-transactional two-step that
# completes the outbox without a durable enqueue loses the message.
check_negative tcOutboxSplitWriteCounterexample 1

# The ingress cursor fold must never persist a cursor covering an envelope
# whose local enqueue did not commit, under nondeterministic batch sizes,
# rolled-back commits, and crash-restarts.
check_green tcIngressFoldNoLoss

# The two ways the fold's ordering can be broken must both be caught by the
# IngressCursorCoversOnlyCommittedEnvelopes monitor: keeping an eagerly
# advanced in-memory cursor after a rollback, and checkpointing the cursor in
# its own commit ahead of the enqueues.
check_negative tcIngressEagerCursorCounterexample 1
check_negative tcIngressCheckpointFirstCounterexample 1

# Ingress dispatch into a BOUNDED in-memory mailbox: a full target must defer
# rather than park, the committed cursor must stop at the undelivered
# envelope, a replayed transaction body must not re-send what it already
# handed over, and a hoisted request must be answered once per process
# lifetime however many times the redrive re-pulls it.
check_green tcIngressDeferralNoLoss

# Deferring must delay the stream, never stop it: once the target keeps up the
# cursor reaches the end.
check_green tcIngressDeferralLiveness

# The pre-fix blocking send must be caught two independent ways: as a safety
# violation by IngressWriterNeverParks, and as the starvation an operator
# actually sees by IngressBacklogEventuallyDrains.
check_negative tcIngressParkedWriterCounterexample 1
check_negative tcIngressParkedWriterStarvesCounterexample 1

# Dropping the per-invocation delivery record must be caught two independent
# ways: as a duplicate within one folded dispatch, and as two deliveries in
# one redrive epoch.
check_negative tcIngressUntrackedRetryDuplicateCounterexample 1
check_negative tcIngressMonitorCatchesRetryDuplicate 1

# Dropping the served watermark must be caught by
# IngressNonTxRequestServedOncePerIncarnation: a redrive answers a hoisted
# request the operator has already had answered.
check_negative tcIngressUnwatermarkedServeCounterexample 1

# P 3.0.4 emits project-global helpers into its output directory. Compile the
# independent project into a clean directory so generated helpers from the
# mailbox project cannot collide with the forfeit-signing project.
rm -rf "${REPO_ROOT}/PGenerated"
run_with_heartbeat "forfeit signing P compile" \
    p compile -pp "$FORFEIT_P_PROJ"

# Receive-side signing authority must be published after the configured
# persistence step, every identity field must match it, and a replayed valid
# answer must preserve the broker's first accepted signature set.
check_green tcForfeitSigningAuthorityAndReplay "$FORFEIT_DLL_PATH"

# The unsafe request-derived profile must still reach the signing oracle before
# authority exists, where the authority monitor catches it.
check_negative tcForfeitRequestDerivedCounterexample 1 \
    "$FORFEIT_DLL_PATH"

# The post-sign recovery project has its own generated helpers and therefore
# compiles into a clean output directory after the other independent projects.
rm -rf "${REPO_ROOT}/PGenerated"
run_with_heartbeat "OOR recovery P compile" \
    p compile -pp "$OOR_RECOVERY_P_PROJ"

# The canonical trace keeps durable authority across restart, rejects a
# conflicting admission, commits terminal state atomically, and tolerates
# replayed notification and acknowledgement.
check_green tcOORRecovery "$OOR_RECOVERY_DLL_PATH"

# Each unsafe profile must remain observable as a counterexample. Together
# these checks prevent either durable transition from splitting, ownership
# from being released after restart, a conflict from replacing the owner, or
# terminal replay from applying completion twice.
check_negative tcOORAuthoritySplitCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "lock and signature persistence split across durable states"
check_negative tcOORCrashDuringAuthorityTransitionCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "signature persistence must complete the lock transition"
check_negative tcOOROwnershipReleaseCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "restart released post-sign ownership"
check_negative tcOORConflictAdmissionCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "conflicting admission replaced post-sign ownership"
check_negative tcOORTerminalSplitCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "finalize and materialize split across durable states"
check_negative tcOORCrashDuringTerminalTransitionCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "materialization must complete the finalization transition"
check_negative tcOORRepeatedCompletionCounterexample 1 \
    "$OOR_RECOVERY_DLL_PATH" \
    "terminal completion was applied more than once"

# The export profile ends with one known sentinel failure after announcing
# every canonical bridge step. The checker writes a structured counterexample,
# which is normalized and byte-compared with the checked-in trace.
export_dir="$(mktemp -d "${TMPDIR:-/tmp}/oor-trace-export.XXXXXX")"
export_output="${export_dir}/check.out"
export_status=0
run_with_heartbeat "tcOORBridgeTraceExport" timeout "$TIMEOUT" \
    p check "$OOR_RECOVERY_DLL_PATH" \
    --testcase tcOORBridgeTraceExport \
    --schedules 1 \
    --max-steps "$MAX_STEPS" \
    --outdir "$export_dir" \
    --xml-trace >"$export_output" 2>&1 || export_status="$?"
cat "$export_output"

export_trace_txt="${export_dir}/BugFinding/OORRecoveryModels_0_0.txt"
if [ "$export_status" -ne 1 ] ||
    ! grep -Fq "Checker found a bug." "$export_output" ||
    ! grep -Fq "OOR_BRIDGE_EXPORT_COMPLETE" "$export_trace_txt"; then

    rm -rf "$export_dir"
    echo "ERROR: OOR bridge export did not reach its sentinel"
    exit 1
fi

python3 "${PROJECT_DIR}/oorrecovery/scripts/normalize_trace.py" \
    --checker-trace \
    "${export_dir}/BugFinding/OORRecoveryModels_0_0.trace.json" \
    --model-root "${PROJECT_DIR}/oorrecovery" \
    --tool-version "$TRACE_P_VERSION" \
    --output "${export_dir}/post_sign_recovery.json"

if ! cmp -s \
    "${PROJECT_DIR}/oorrecovery/traces/post_sign_recovery.json" \
    "${export_dir}/post_sign_recovery.json"; then

    diff -u \
        "${PROJECT_DIR}/oorrecovery/traces/post_sign_recovery.json" \
        "${export_dir}/post_sign_recovery.json" || true
    rm -rf "$export_dir"
    echo "ERROR: checked-in OOR bridge trace is stale"
    exit 1
fi

rm -rf "$export_dir"
echo "OK: P checker export matches the checked-in OOR bridge trace"

echo ""
echo "=== Go Bridge Conformance ==="
go test ./p-models/durableactor/bridge
go test ./sdk/swaps ./waved \
    -run 'TestForfeitSigning(Session|Broker)ModelTrace$'
