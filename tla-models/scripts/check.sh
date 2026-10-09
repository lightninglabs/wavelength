#!/usr/bin/env bash
# Run the TLA+ models and prove that weakened profiles still fail.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODEL_ROOT="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(dirname "$MODEL_ROOT")"
MODEL_DIR="${MODEL_ROOT}/durablehandoff"
MODULE="${MODEL_DIR}/DurableHandoff.tla"

TLA_VERSION="1.8.0"
TLA_SHA256="7beec0f04818732a62fa193731711a99aa4f11279499b2360a7d156c519ea78d"
TLA_URL="https://github.com/tlaplus/tlaplus/releases/download/v${TLA_VERSION}/tla2tools.jar"
CACHE_ROOT="${XDG_CACHE_HOME:-${HOME}/.cache}/wavelength-tla"
JAR="${TLA2TOOLS_JAR:-${CACHE_ROOT}/tla2tools-${TLA_VERSION}.jar}"
TLC_STATE_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/wavelength-tla.XXXXXX")"

trap 'rm -rf "$TLC_STATE_ROOT"' EXIT

cd "$REPO_ROOT"

if ! command -v java >/dev/null 2>&1; then
    echo "Error: Java 17 or newer is required."
    exit 1
fi

java_major="$(java -version 2>&1 | awk -F '[\".]' 'NR == 1 {print $2}')"
if [ "${java_major:-0}" -lt 17 ]; then
    echo "Error: Java 17 or newer is required; found Java ${java_major:-unknown}."
    exit 1
fi

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
        return
    fi

    shasum -a 256 "$1" | awk '{print $1}'
}

if [ -z "${TLA2TOOLS_JAR:-}" ]; then
    mkdir -p "$CACHE_ROOT"
    if [ ! -f "$JAR" ]; then
        curl --fail --location --retry 3 \
            --connect-timeout 15 --max-time 120 \
            --output "${JAR}.tmp" "$TLA_URL"
        mv "${JAR}.tmp" "$JAR"
    fi

    actual_sha="$(sha256 "$JAR")"
    if [ "$actual_sha" != "$TLA_SHA256" ]; then
        echo "Error: unexpected SHA-256 for $JAR"
        echo "Expected: $TLA_SHA256"
        echo "Actual:   $actual_sha"
        exit 1
    fi
fi

run_tlc() {
    local config="$1"
    local state_dir="${TLC_STATE_ROOT}/${config}"

    mkdir -p "$state_dir"

    java -cp "$JAR" tlc2.TLC \
        -cleanup \
        -deadlock \
        -metadir "$state_dir" \
        -noGenerateSpecTE \
        -workers 1 \
        -config "${MODEL_DIR}/${config}.cfg" \
        "$MODULE"
}

check_negative() {
    local config="$1"
    local invariant="$2"
    local output

    output="$(mktemp "${TMPDIR:-/tmp}/tla-${config}.XXXXXX")"
    if run_tlc "$config" >"$output" 2>&1; then
        cat "$output"
        rm -f "$output"
        echo "Error: ${config} found no counterexample."
        return 1
    fi

    if ! grep -Fq "Invariant ${invariant} is violated" "$output"; then
        cat "$output"
        rm -f "$output"
        echo "Error: ${config} failed without the expected ${invariant} counterexample."
        return 1
    fi

    echo "OK: ${config} violates ${invariant} as expected."
    rm -f "$output"
}

echo "=== production durable handoff model ==="
run_tlc Production

echo "=== expected counterexamples ==="
check_negative SplitHandoff AckHasDurableSuccessor
check_negative OmitDelayedRecovery PendingTargetHasConsumer
check_negative OmitLeasedRecovery PendingTargetHasConsumer
check_negative ReapTerminalConsumer PendingTargetHasConsumer
