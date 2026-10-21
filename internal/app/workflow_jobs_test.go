package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWorkflow(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, ".github", "workflows", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalWorkflowJobsRunSupportedPullRequestShellStepsWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	t.Setenv("GH_TOKEN", "must-not-reach-workflow")
	writeWorkflow(t, root, "ci.yml", `name: local fixture
on: pull_request
jobs:
  shell:
    runs-on: ubuntu-latest
    steps:
      - name: Candidate checkout
        uses: actions/checkout@v4
        with:
          persist-credentials: false
      - name: Local Go setup
        uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: false
      - name: Check isolated execution
        run: |
          test -d "$GITHUB_WORKSPACE"
          test -z "${GH_TOKEN:-}"
          printf 'ran\n' > "$POSSE_TASK_TEMP/workflow-ran"
`)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\ngo 1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	coverage, err := runLocalWorkflowJobs(context.Background(), root, scratch, true, "fixture commands passed")
	if err != nil {
		t.Fatalf("run workflow: %v; coverage=%+v", err, coverage)
	}
	if len(coverage.Workflows) != 1 || len(coverage.Workflows[0].Jobs) != 1 {
		t.Fatalf("unexpected workflow coverage: %+v", coverage)
	}
	job := coverage.Workflows[0].Jobs[0]
	if job.Name != "shell" || job.Status != "passed" || len(job.Steps) != 3 || job.Steps[0].Status != "not_applicable" || job.Steps[1].Status != "not_applicable" || job.Steps[2].Status != "passed" {
		t.Fatalf("local job results do not identify executed and substituted steps: %+v", job)
	}
	if data, err := os.ReadFile(filepath.Join(scratch, "workflow-ran")); err != nil || string(data) != "ran\n" {
		t.Fatalf("workflow did not run in Task scratch: %q, %v", data, err)
	}
}

func TestLocalWorkflowJobsRecordUnsupportedJobsAsNotApplicable(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	writeWorkflow(t, root, "ci.yml", `name: unsupported fixtures
on: pull_request
jobs:
  secret:
    runs-on: ubuntu-latest
    env:
      ACCESS_TOKEN: ${{ secrets.ACCESS_TOKEN }}
    steps:
      - run: exit 99
  service:
    runs-on: ubuntu-latest
    services:
      redis:
        image: redis
    steps:
      - run: exit 99
  windows:
    runs-on: windows-latest
    steps:
      - run: exit 99
  matrix:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        shard: [1, 2]
    steps:
      - run: exit 99
`)
	coverage, err := runLocalWorkflowJobs(context.Background(), root, scratch, true, "")
	if err != nil {
		t.Fatalf("unsupported jobs should be explicit, not fail as executed: %v", err)
	}
	jobs := coverage.Workflows[0].Jobs
	if len(jobs) != 4 {
		t.Fatalf("jobs=%+v", jobs)
	}
	for _, job := range jobs {
		if job.Status != "not_applicable" || strings.TrimSpace(job.Reason) == "" {
			t.Errorf("unsupported job is not visibly not applicable: %+v", job)
		}
	}
	encoded, err := workflowCoverageJSON(coverage)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil || decoded["schema"] != localWorkflowCoverageSchema {
		t.Fatalf("coverage is not machine-readable and versioned: %s err=%v", encoded, err)
	}
}

func TestLocalWorkflowDefaultsAreNotSilentlyIgnored(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	writeWorkflow(t, root, "ci.yml", `name: defaults fixture
on: pull_request
defaults:
  run:
    working-directory: nested
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: test -f expected-file
`)
	coverage, err := runLocalWorkflowJobs(context.Background(), root, scratch, true, "")
	if err != nil {
		t.Fatalf("unsupported workflow semantics should be reported, not executed: %v", err)
	}
	workflow := coverage.Workflows[0]
	if workflow.Status != "not_applicable" || !strings.Contains(workflow.Reason, "defaults") || len(workflow.Jobs) != 1 || workflow.Jobs[0].Status != "not_applicable" {
		t.Fatalf("workflow defaults were not explicitly excluded: %+v", workflow)
	}
}

func TestLocalWorkflowJobFailureBlocksAndDisabledExecutionIsVisible(t *testing.T) {
	root := t.TempDir()
	scratch := t.TempDir()
	writeWorkflow(t, root, "ci.yml", `name: failure fixture
on:
  pull_request:
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - name: fail
        run: echo failed; exit 7
`)
	coverage, err := runLocalWorkflowJobs(context.Background(), root, scratch, true, "")
	if err == nil || !failedWorkflowJob(mustWorkflowCoverageJSON(t, coverage)) {
		t.Fatalf("failed workflow did not block: coverage=%+v err=%v", coverage, err)
	}
	if !strings.Contains(workflowCoverageFailureOutput(mustWorkflowCoverageJSON(t, coverage)), "failed") {
		t.Fatalf("failed job output was not retained: %+v", coverage)
	}

	coverage, err = runLocalWorkflowJobs(context.Background(), root, scratch, false, "")
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("disabled supported workflow was treated as a pass: coverage=%+v err=%v", coverage, err)
	}
	job := coverage.Workflows[0].Jobs[0]
	if job.Status != "not_applicable" || !strings.Contains(job.Reason, "full_pipeline.run_workflows") {
		t.Fatalf("disabled workflow execution is not visible: %+v", job)
	}
}

func mustWorkflowCoverageJSON(t *testing.T, coverage localWorkflowCoverage) string {
	t.Helper()
	encoded, err := workflowCoverageJSON(coverage)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
