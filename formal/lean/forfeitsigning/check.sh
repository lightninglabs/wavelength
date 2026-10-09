#!/usr/bin/env bash
# Build the Lean proof, verify its vectors, and replay them against Go.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
VECTOR_OUTPUT="$(mktemp)"
trap 'rm -f "${VECTOR_OUTPUT}"' EXIT

cd "${SCRIPT_DIR}"

if ! command -v lake >/dev/null 2>&1; then
    echo "Error: Lake is required. Install elan from https://lean-lang.org/."
    exit 1
fi

lake build
lake exe bridgeVectors > "${VECTOR_OUTPUT}"

if ! cmp -s vectors.tsv "${VECTOR_OUTPUT}"; then
    echo "Error: Lean bridge vectors do not match vectors.tsv."
    diff -u vectors.tsv "${VECTOR_OUTPUT}" || true
    exit 1
fi

cd "${REPO_ROOT}"
go test ./sdk/swaps -run '^TestForfeitSigningLeanBridge$'
