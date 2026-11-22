#!/usr/bin/env python3
"""Record Go test JSON events and summarize result artifacts."""

import argparse
import collections
import datetime
import json
import os
import pathlib
import statistics
import subprocess
import sys
import tempfile


def record(args):
    result_file = os.environ.get("POSSE_TEST_RESULTS")
    if not result_file:
        result_file = str(pathlib.Path(tempfile.gettempdir()) / "posse-test-results.jsonl")
    result_path = pathlib.Path(result_file)
    result_path.parent.mkdir(parents=True, exist_ok=True)
    process = subprocess.Popen(args.command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    with result_path.open("a", encoding="utf-8") as output:
        for line in process.stdout:
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                sys.stdout.write(line)
                continue
            if event.get("Action") == "output":
                sys.stdout.write(event.get("Output", ""))
                sys.stdout.flush()
            if "Test" not in event or event.get("Action") not in ("pass", "fail"):
                continue
            outcome = {
                "test": event["Test"],
                "package": event.get("Package", ""),
                "shard": args.shard,
                "head": os.environ.get("POSSE_TEST_HEAD", os.environ.get("GITHUB_SHA", "local")),
                "run_id": os.environ.get("GITHUB_RUN_ID", "local"),
                "attempt": os.environ.get("GITHUB_RUN_ATTEMPT", "1"),
                "job": os.environ.get("GITHUB_JOB", "local"),
                "result": event["Action"],
                "duration_seconds": event.get("Elapsed", 0),
                "recorded_at": event.get("Time", datetime.datetime.now(datetime.timezone.utc).isoformat()),
            }
            output.write(json.dumps(outcome, sort_keys=True) + "\n")
            output.flush()
    return process.wait()


def summarize(args):
    rows = []
    for path in args.files:
        with open(path, encoding="utf-8") as source:
            rows.extend(json.loads(line) for line in source if line.strip())
    grouped = collections.defaultdict(list)
    for row in rows:
        grouped[(row["package"], row["test"])].append(row)
    tests = []
    for (package, test), records in sorted(grouped.items()):
        records.sort(key=lambda row: (row.get("recorded_at", ""), row.get("run_id", ""), row.get("attempt", "")))
        records = records[-args.last:]
        failures = sum(row["result"] == "fail" for row in records)
        passes = sum(row["result"] == "pass" for row in records)
        reruns = collections.Counter()
        by_head = collections.defaultdict(list)
        for row in records:
            by_head[row["head"]].append(row)
        for head_records in by_head.values():
            ordered = sorted(head_records, key=lambda row: (row.get("recorded_at", ""), row.get("run_id", ""), row.get("attempt", "")))
            for row in ordered[1:]:
                reruns["passed" if row["result"] == "pass" else "failed"] += 1
        durations = [row["duration_seconds"] for row in records]
        tests.append({
            "test": test,
            "package": package,
            "runs": len(records),
            "failures": failures,
            "passes": passes,
            "same_head_reruns": {"passed": reruns["passed"], "failed": reruns["failed"]},
            "median_duration_seconds": statistics.median(durations) if durations else None,
        })
    print(json.dumps({"last_runs_per_test": args.last, "tests": tests}, indent=2, sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="mode", required=True)
    record_parser = commands.add_parser("record")
    record_parser.add_argument("--shard", required=True)
    record_parser.add_argument("command", nargs=argparse.REMAINDER)
    summary_parser = commands.add_parser("summarize")
    summary_parser.add_argument("--last", type=int, required=True)
    summary_parser.add_argument("files", nargs="+")
    args = parser.parse_args()
    if args.mode == "record":
        if args.command and args.command[0] == "--":
            args.command = args.command[1:]
        if not args.command:
            parser.error("record requires a command after --")
        sys.exit(record(args))
    if args.last < 1:
        parser.error("--last must be at least 1")
    summarize(args)


if __name__ == "__main__":
    main()
