#!/usr/bin/env bash
# Run the durable mailbox P model and the Go bridge conformance tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(dirname "$PROJECT_DIR")"
P_PROJ="${PROJECT_DIR}/durableactor/infra.pproj"
BUILD_DIR="${REPO_ROOT}/PGenerated/PChecker/net8.0"
DLL_PATH="${BUILD_DIR}/MailboxInfraModels.dll"

SCHEDULES="${SCHEDULES:-50}"
MAX_STEPS="${MAX_STEPS:-700}"
TIMEOUT="${TIMEOUT:-300}"

cd "$REPO_ROOT"

echo "=== P Mailbox Infra Checking ==="
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

EXPECTED_P_VERSION="3.0.4"
P_VERSION="$(p --version 2>/dev/null | awk '{print $NF}')"
case "$P_VERSION" in
    "${EXPECTED_P_VERSION}"|"${EXPECTED_P_VERSION}".*)
        ;;
    *)
        echo "Warning: expected P ${EXPECTED_P_VERSION}, got ${P_VERSION:-unknown}"
        ;;
esac

rm -rf "${REPO_ROOT}/PGenerated"
run_with_heartbeat "P compile" p compile -pp "$P_PROJ"

# check_green runs a test case that must hold: p check exits non-zero if it
# finds any bug, so set -e fails the script on a regression.
check_green() {
    local testcase="$1"

    echo ""
    echo "=== green: ${testcase} (expect 0 bugs) ==="
    run_with_heartbeat "$testcase" timeout "$TIMEOUT" p check "$DLL_PATH" \
        --testcase "$testcase" \
        --schedules "$SCHEDULES" \
        --max-steps "$MAX_STEPS"
}

# check_negative runs a test case that must find a bug. A clean run is itself a
# regression: it means the model no longer detects the failure mode the test
# exists to catch, so we invert the exit code and fail loudly.
check_negative() {
    local testcase="$1"
    local schedules="${2:-$SCHEDULES}"

    echo ""
    echo "=== negative: ${testcase} (expect a bug) ==="
    if run_with_heartbeat "$testcase" timeout "$TIMEOUT" p check "$DLL_PATH" \
        --testcase "$testcase" \
        --schedules "$schedules" \
        --max-steps "$MAX_STEPS"; then

        echo "ERROR: ${testcase} found no bug, but a bug was expected"
        return 1
    fi

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

echo ""
echo "=== Go Bridge Conformance ==="
go test ./p-models/durableactor/bridge
