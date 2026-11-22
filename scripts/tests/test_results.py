import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parents[1] / "test-results.py"


class TestResultsCLI(unittest.TestCase):
    def test_summary_counts_results_and_same_head_reruns(self):
        with tempfile.TemporaryDirectory() as directory:
            result_file = pathlib.Path(directory) / "results.jsonl"
            rows = [
                {"test": "TestFlaky", "package": "./internal/x", "shard": "1", "head": "abc", "run_id": "10", "attempt": "1", "result": "fail", "duration_seconds": 2},
                {"test": "TestFlaky", "package": "./internal/x", "shard": "1", "head": "abc", "run_id": "10", "attempt": "2", "result": "pass", "duration_seconds": 4},
                {"test": "TestStillFlaky", "package": "./internal/x", "shard": "1", "head": "ghi", "run_id": "12", "attempt": "1", "result": "fail", "duration_seconds": 2},
                {"test": "TestStillFlaky", "package": "./internal/x", "shard": "1", "head": "ghi", "run_id": "12", "attempt": "2", "result": "fail", "duration_seconds": 2},
                {"test": "TestRepeatAcrossRuns", "package": "./internal/x", "shard": "1", "head": "jkl", "run_id": "13", "attempt": "1", "result": "fail", "duration_seconds": 2},
                {"test": "TestRepeatAcrossRuns", "package": "./internal/x", "shard": "2", "head": "jkl", "run_id": "14", "attempt": "1", "result": "pass", "duration_seconds": 3},
                {"test": "TestStable", "package": "./internal/x", "shard": "1", "head": "def", "run_id": "11", "attempt": "1", "result": "pass", "duration_seconds": 6},
            ]
            result_file.write_text("".join(json.dumps(row) + "\n" for row in rows))
            completed = subprocess.run(
                [sys.executable, str(SCRIPT), "summarize", "--last", "10", str(result_file)],
                check=True, text=True, capture_output=True,
            )
            summary = json.loads(completed.stdout)
            flaky = next(test for test in summary["tests"] if test["test"] == "TestFlaky")
            self.assertEqual(flaky["failures"], 1)
            self.assertEqual(flaky["passes"], 1)
            self.assertEqual(flaky["same_head_reruns"], {"passed": 1, "failed": 0})
            self.assertEqual(flaky["median_duration_seconds"], 3)
            still_flaky = next(test for test in summary["tests"] if test["test"] == "TestStillFlaky")
            self.assertEqual(still_flaky["same_head_reruns"], {"passed": 0, "failed": 1})
            across_runs = next(test for test in summary["tests"] if test["test"] == "TestRepeatAcrossRuns")
            self.assertEqual(across_runs["same_head_reruns"], {"passed": 1, "failed": 0})

    def test_recorder_preserves_go_exit_status_and_records_test_outcome(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "results.jsonl"
            env = os.environ | {
                "POSSE_TEST_RESULTS": str(output),
                "GITHUB_SHA": "abc",
                "GITHUB_RUN_ID": "10",
                "GITHUB_RUN_ATTEMPT": "1",
            }
            completed = subprocess.run(
                [sys.executable, str(SCRIPT), "record", "--shard", "3", "--", "go", "test", "-json", "./internal/taskplan", "-run", "^TestFullPipelineReviewModeCanBeFrozenBeforeVPCRuntime$"],
                env=env, text=True, capture_output=True,
            )
            self.assertEqual(completed.returncode, 0)
            row = json.loads(output.read_text())
            self.assertEqual(row["package"], "github.com/thanhbinh1905/posse/internal/taskplan")
            self.assertEqual(row["shard"], "3")
            self.assertEqual(row["head"], "abc")
            self.assertEqual(row["result"], "pass")

    def test_recorder_preserves_go_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / "results.jsonl"
            env = os.environ | {
                "POSSE_TEST_RESULTS": str(output),
                "GITHUB_SHA": "abc",
                "GITHUB_RUN_ID": "10",
                "GITHUB_RUN_ATTEMPT": "1",
            }
            completed = subprocess.run(
                [sys.executable, str(SCRIPT), "record", "--shard", "3", "--", sys.executable, "-c",
                 "print('{\\\"Time\\\":\\\"2026-01-01T00:00:00Z\\\",\\\"Action\\\":\\\"run\\\",\\\"Package\\\":\\\"./pkg\\\",\\\"Test\\\":\\\"TestBad\\\"}'); print('{\\\"Time\\\":\\\"2026-01-01T00:00:01Z\\\",\\\"Action\\\":\\\"fail\\\",\\\"Package\\\":\\\"./pkg\\\",\\\"Test\\\":\\\"TestBad\\\",\\\"Elapsed\\\":1}') ; raise SystemExit(1)"],
                env=env, text=True, capture_output=True,
            )
            self.assertEqual(completed.returncode, 1)
            row = json.loads(output.read_text())
            self.assertEqual((row["test"], row["package"], row["shard"], row["head"], row["result"]),
                             ("TestBad", "./pkg", "3", "abc", "fail"))
            self.assertEqual(row["duration_seconds"], 1)


if __name__ == "__main__":
    unittest.main()
