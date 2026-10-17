#!/usr/bin/env python3
"""Normalize the P checker bridge announcements into the versioned trace."""

import argparse
import hashlib
import json
from pathlib import Path


MODEL_PATH = "src/oor_recovery.p"
PUBLIC_MODEL_PATH = "p-models/oorrecovery/src/oor_recovery.p"

EXPECTED_STEPS = (
    ("submit", ""),
    ("lock", ""),
    ("persist_signature", "owned"),
    ("disclose", ""),
    ("crash", ""),
    ("restart", "owned"),
    ("conflicting_admission", "rejected"),
    ("finalize", ""),
    ("materialize", "terminal"),
    ("notify", ""),
    ("ack", "completed"),
)


def model_digest(model_root: Path) -> str:
    """Return the digest of the public model named by producer.path."""

    return hashlib.sha256((model_root / MODEL_PATH).read_bytes()).hexdigest()


def bridge_steps(checker_trace: Path) -> list[dict[str, str]]:
    """Extract only structured eBridgeStep announcements from a trace."""

    records = json.loads(checker_trace.read_text(encoding="utf-8"))
    steps = []
    observed = []

    for record in records:
        if record.get("type") != "Announce":
            continue

        details = record.get("details", {})
        if details.get("event") != "eBridgeStep":
            continue

        payload = details.get("payload", {})
        op = payload.get("op", "")
        expect = payload.get("expect", "")
        observed.append((op, expect))

        step = {"op": op}
        if expect:
            step["expect"] = expect

        steps.append(step)

    if tuple(observed) != EXPECTED_STEPS:
        raise ValueError(
            "unexpected P bridge steps: "
            f"got {observed!r}, want {EXPECTED_STEPS!r}"
        )

    return steps


def main() -> None:
    """Write the canonical normalized trace for byte comparison."""

    parser = argparse.ArgumentParser()
    parser.add_argument("--checker-trace", type=Path, required=True)
    parser.add_argument("--model-root", type=Path, required=True)
    parser.add_argument("--tool-version", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    normalized = {
        "schema_version": 1,
        "trace_id": "post_sign_recovery",
        "description": (
            "Retain post-sign ownership across restart, reject conflicting "
            "admission, and complete the terminal transition once."
        ),
        "producer": {
            "kind": "p",
            "path": PUBLIC_MODEL_PATH,
            "model": "OORRecoveryModels",
            "testcase": "tcOORBridgeTraceExport",
            "tool_version": args.tool_version,
            "model_sha256": model_digest(args.model_root),
        },
        "steps": bridge_steps(args.checker_trace),
    }
    args.output.write_text(
        json.dumps(normalized, indent=2) + "\n",
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
