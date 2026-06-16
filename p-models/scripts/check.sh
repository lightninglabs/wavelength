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
p compile -pp "$P_PROJ"

# check_green runs a test case that must hold: p check exits non-zero if it
# finds any bug, so set -e fails the script on a regression.
check_green() {
    local testcase="$1"

    echo ""
    echo "=== green: ${testcase} (expect 0 bugs) ==="
    timeout "$TIMEOUT" p check "$DLL_PATH" \
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
    if timeout "$TIMEOUT" p check "$DLL_PATH" \
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

echo ""
echo "=== Go Bridge Conformance ==="
go test ./p-models/durableactor/bridge
