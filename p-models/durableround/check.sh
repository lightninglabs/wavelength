#!/usr/bin/env bash
# Check the round commit boundary and require each named counterexample.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
P_BIN="${P_BIN:-p}"
"$P_BIN" compile -pp p-models/durableround/round.pproj
DLL=RoundPGenerated/PChecker/net8.0/DurableRoundModels.dll
OUTPUT="$(mktemp -d "${TMPDIR:-/tmp}/round-models.XXXXXX")"

"$P_BIN" check "$DLL" --testcase tcRoundCommitHistory \
    --schedules "${SCHEDULES:-500}" --max-steps 300 --fail-on-maxsteps \
    --outdir "$OUTPUT/history"

# A tool failure, timeout, or different assertion must not count as success.
negative() {
    local testcase="$1"
    local expected="$2"
    local output="$OUTPUT/$testcase"
    if "$P_BIN" check "$DLL" --testcase "$testcase" --schedules 1 \
        --max-steps 300 --outdir "$output"; then
        echo "Expected counterexample was not found: $testcase" >&2
        return 1
    fi
    grep -F -r -q -- "$expected" "$output"
}

negative tcRoundLostOutbox \
    "round acknowledgement lost an outgoing effect"
negative tcRoundOverlappingFallback "operation has overlapping attempts"
negative tcRoundUncertainRelease "uncertain signer released operation"
go test ./p-models/durableactor/bridge -run '^TestRoundCommitBoundary$' -count=1
echo "Round model outputs: $OUTPUT"
