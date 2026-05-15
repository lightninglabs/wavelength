#!/usr/bin/env python3
"""Run go test -json and emit a slow-test timing summary."""

import argparse
import json
import os
import subprocess
import sys
import time


def parse_args():
    """Parse command line arguments."""
    parser = argparse.ArgumentParser(
        description="Run go test -json and summarize per-test timings.",
    )
    parser.add_argument(
        "--label",
        default="go-test",
        help="Human-readable label printed in the timing summary.",
    )
    parser.add_argument(
        "--out-dir",
        default="test-artifacts/go-test-timing",
        help="Directory for timing JSONL and summary artifacts.",
    )
    parser.add_argument(
        "--summary-count",
        type=int,
        default=20,
        help="Number of slow tests to print in the summary.",
    )
    parser.add_argument(
        "cmd",
        nargs=argparse.REMAINDER,
        help="Command to run after --, usually go test -json ...",
    )

    args = parser.parse_args()
    if args.cmd and args.cmd[0] == "--":
        args.cmd = args.cmd[1:]
    if not args.cmd:
        parser.error("missing command after --")

    return args


def sanitize_label(label):
    """Return a filesystem-friendly label."""
    out = []
    for ch in label:
        if ch.isalnum() or ch in ("-", "_", "."):
            out.append(ch)
        else:
            out.append("_")

    return "".join(out).strip("_") or "go-test"


def test_key(event):
    """Return a stable key for a test event."""
    return event.get("Package", ""), event.get("Test", "")


def record_test_event(events, event):
    """Record final pass/fail/skip timing for one test."""
    action = event.get("Action")
    test = event.get("Test")
    if action not in ("pass", "fail", "skip") or not test:
        return

    package, test = test_key(event)
    events[(package, test)] = {
        "package": package,
        "test": test,
        "action": action,
        "elapsed_seconds": float(event.get("Elapsed") or 0),
    }


def write_artifacts(args, started_at, ended_at, returncode, events):
    """Write timing artifacts and return sorted test records."""
    os.makedirs(args.out_dir, exist_ok=True)

    records = sorted(
        events.values(),
        key=lambda item: item["elapsed_seconds"],
        reverse=True,
    )
    summary = {
        "label": args.label,
        "command": args.cmd,
        "started_at_unix": started_at,
        "ended_at_unix": ended_at,
        "duration_seconds": ended_at - started_at,
        "returncode": returncode,
        "tests": records,
    }

    prefix = sanitize_label(args.label)
    summary_path = os.path.join(args.out_dir, f"{prefix}-summary.json")
    text_path = os.path.join(args.out_dir, f"{prefix}-summary.txt")

    with open(summary_path, "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=2, sort_keys=True)
        f.write("\n")

    with open(text_path, "w", encoding="utf-8") as f:
        f.write(render_summary(args, summary, records))

    return records, summary_path, text_path


def render_summary(args, summary, records):
    """Render a human-readable slow-test report."""
    lines = [
        f"Go test timing summary: {summary['label']}",
        f"Command: {' '.join(summary['command'])}",
        f"Exit code: {summary['returncode']}",
        f"Total wall-clock: {summary['duration_seconds']:.2f}s",
        f"Completed tests: {len(records)}",
        "",
        f"Slowest {min(args.summary_count, len(records))} tests:",
    ]

    for record in records[: args.summary_count]:
        lines.append(
            f"{record['elapsed_seconds']:9.2f}s "
            f"{record['action']:>4} "
            f"{record['test']} "
            f"({record['package']})",
        )

    lines.append("")

    return "\n".join(lines)


def main():
    """Run go test and summarize timings."""
    args = parse_args()
    os.makedirs(args.out_dir, exist_ok=True)

    prefix = sanitize_label(args.label)
    jsonl_path = os.path.join(args.out_dir, f"{prefix}.jsonl")
    started_at = time.time()
    events = {}

    with open(jsonl_path, "w", encoding="utf-8") as jsonl:
        proc = subprocess.Popen(
            args.cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )

        if proc.stdout is None:
            sys.exit("go_test_timing: could not open subprocess stdout")

        for line in proc.stdout:
            jsonl.write(line)
            jsonl.flush()

            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                sys.stdout.write(line)
                sys.stdout.flush()
                continue

            output = event.get("Output")
            if output:
                sys.stdout.write(output)
                sys.stdout.flush()

            record_test_event(events, event)

        returncode = proc.wait()

    ended_at = time.time()
    records, summary_path, text_path = write_artifacts(
        args, started_at, ended_at, returncode, events,
    )

    print()
    print(render_summary(args, {
        "label": args.label,
        "command": args.cmd,
        "returncode": returncode,
        "duration_seconds": ended_at - started_at,
    }, records))
    print(f"Timing JSONL: {jsonl_path}")
    print(f"Timing JSON: {summary_path}")
    print(f"Timing text: {text_path}")

    return returncode


if __name__ == "__main__":
    raise SystemExit(main())
