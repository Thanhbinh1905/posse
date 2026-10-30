package app

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

type goTestFailure struct {
	Package string
	Test    string
}

type goTestFailures struct {
	Tests       []goTestFailure
	BuildFailed bool
}

var (
	goTestFailLine    = regexp.MustCompile(`--- FAIL: (\S+)`)
	goPackageFailLine = regexp.MustCompile(`(?:^|\s)FAIL\s+(\S+)\s+(\[[a-z ]+ failed\]|[0-9.]+s)\s*$`)
)

// parseGoTestFailures reads top-level failed tests from go test output, which
// may carry forge log prefixes. A test before its package's FAIL line belongs
// to that package; a test whose package line is outside the tail keeps an
// empty Package.
func parseGoTestFailures(log string) goTestFailures {
	var result goTestFailures
	pending := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimRight(line, "\r")
		if match := goTestFailLine.FindStringSubmatch(line); match != nil {
			test, _, _ := strings.Cut(match[1], "/")
			if !seen[test] {
				seen[test] = true
				pending = append(pending, test)
			}
			continue
		}
		if match := goPackageFailLine.FindStringSubmatch(line); match != nil {
			if strings.HasPrefix(match[2], "[") {
				result.BuildFailed = true
			}
			for _, test := range pending {
				result.Tests = append(result.Tests, goTestFailure{Package: match[1], Test: test})
			}
			pending, seen = pending[:0], map[string]bool{}
		}
	}
	for _, test := range pending {
		result.Tests = append(result.Tests, goTestFailure{Test: test})
	}
	return result
}

// rerunKnownFlakes records the parsed test failures, reruns each known flaky
// check once per head and returns the checks that still need a fix round.
func (s *Service) rerunKnownFlakes(ctx context.Context, db *store.DB, project store.Project, task store.Task, plan *taskplan.Plan, observation store.PRObservation, forge repositoryForge, member string, checks []failedCheck, tails map[string]string) ([]failedCheck, error) {
	now := time.Now()
	parsed := make(map[string]goTestFailures, len(checks))
	history := []store.CITestFailure{}
	for _, check := range checks {
		failures := parseGoTestFailures(tails[check.Name])
		parsed[check.Name] = failures
		for _, failure := range failures.Tests {
			history = append(history, store.CITestFailure{ProjectID: project.ID, TaskID: task.ID, Repo: member, PRURL: observation.PRURL, HeadSHA: observation.HeadSHA, CheckName: check.Name, JobID: check.JobID, Package: failure.Package, Test: failure.Test, ObservedAt: now.UnixMilli()})
		}
	}
	if err := db.RecordCITestFailures(ctx, history); err != nil {
		return nil, err
	}
	window := plan.ValidationLoops.CI.FlakeWindowDuration()
	if window <= 0 || len(history) == 0 {
		return checks, nil
	}
	changed, err := prChangedFiles(ctx, forge.Root, taskTargetBranch(project, task), observation.HeadSHA)
	if err != nil {
		return checks, nil
	}
	since := now.Add(-window).UnixMilli()
	remaining := make([]failedCheck, 0, len(checks))
	for _, check := range checks {
		tests, err := knownFlakyTests(ctx, db, project.ID, observation.PRURL, check, parsed[check.Name], changed, since)
		if err != nil {
			return nil, err
		}
		if len(tests) == 0 {
			remaining = append(remaining, check)
			continue
		}
		id, inserted, err := db.RecordCIRerun(ctx, store.CIRerun{ProjectID: project.ID, TaskID: task.ID, Repo: member, PRURL: observation.PRURL, HeadSHA: observation.HeadSHA, CheckName: check.Name, RunID: check.RunID, JobID: check.JobID, Reason: "flake", Tests: tests})
		if err != nil {
			return nil, err
		}
		if !inserted {
			remaining = append(remaining, check)
			continue
		}
		if err := requestCIRerun(ctx, forge.Root, observation.PRURL, forge, ciRerunGroup{runID: check.RunID, jobID: check.JobID, checks: []failedCheck{check}}); err != nil {
			if err := db.FailCIRerunRequest(ctx, id); err != nil {
				return nil, err
			}
			tails[check.Name] = strings.TrimSpace(tails[check.Name] + "\nFlake rerun request failed: " + err.Error())
			remaining = append(remaining, check)
			continue
		}
		memberLabel := ""
		if member != "" {
			memberLabel = member + ": "
		}
		if _, err := db.CreateNotice(ctx, store.Notice{
			ProjectID: project.ID, TaskID: task.ID, Kind: "ci_flake_rerun",
			Summary:  fmt.Sprintf("%s: %sreran CI check %s once on gated head %s instead of queueing a fix round: %s failed on another PR within %s and the PR does not change their packages", task.Title, memberLabel, check.Name, observation.HeadSHA, strings.Join(tests, ", "), window),
			DataJSON: marshalJSON(map[string]any{"url": observation.PRURL, "head_sha": observation.HeadSHA, "check_name": check.Name, "run_id": check.RunID, "job_id": check.JobID, "tests": tests, "repository": member}),
		}); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}

func knownFlakyTests(ctx context.Context, db *store.DB, projectID int64, prURL string, check failedCheck, failures goTestFailures, changed []string, since int64) ([]string, error) {
	if check.RunID == "" || check.JobID == "" || failures.BuildFailed || len(failures.Tests) == 0 {
		return nil, nil
	}
	tests := make([]string, 0, len(failures.Tests))
	for _, failure := range failures.Tests {
		if failure.Package == "" || packageTouched(failure.Package, changed) {
			return nil, nil
		}
		found, err := db.CITestFailedElsewhere(ctx, projectID, prURL, failure.Package, failure.Test, since)
		if err != nil || !found {
			return nil, err
		}
		tests = append(tests, failure.Test)
	}
	return tests, nil
}

func prChangedFiles(ctx context.Context, root, targetBranch, head string) ([]string, error) {
	var target string
	for _, ref := range []string{"refs/remotes/origin/" + targetBranch, "refs/heads/" + targetBranch} {
		if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			target = ref
			break
		}
	}
	if target == "" {
		return nil, fmt.Errorf("target branch %s is not available locally", targetBranch)
	}
	output, err := gitOutputRaw(ctx, root, "diff", "--name-only", "-z", target+"..."+head)
	if err != nil {
		return nil, err
	}
	return strings.FieldsFunc(output, func(r rune) bool { return r == 0 }), nil
}

// packageTouched matches changed directories against the end of importPath
// because the module path is unknown; a root-level Go or module file touches
// every package.
func packageTouched(importPath string, changed []string) bool {
	for _, file := range changed {
		dir := path.Dir(file)
		if dir == "." {
			if path.Ext(file) == ".go" || file == "go.mod" || file == "go.sum" {
				return true
			}
			continue
		}
		for ; dir != "."; dir = path.Dir(dir) {
			if importPath == dir || strings.HasSuffix(importPath, "/"+dir) {
				return true
			}
		}
	}
	return false
}

func settleCIReruns(ctx context.Context, db *store.DB, forge repositoryForge, taskID int64, member string, observation store.PRObservation) error {
	snapshot := decodeChecks(observation.Checks)
	if snapshot.Running || (snapshot.State != "SUCCESS" && snapshot.State != "FAILURE") {
		return nil
	}
	return db.SettleCIReruns(ctx, taskID, member, observation.PRURL, observation.HeadSHA, func() (map[string]bool, error) {
		failures := snapshot.Failures
		if forge.Kind == "gitlab" && len(failures) > 0 {
			expanded, err := expandGitLabFailureChecks(ctx, forge, failures)
			if err != nil {
				return nil, err
			}
			failures = expanded
		}
		failing := make(map[string]bool, len(failures))
		for _, failure := range failures {
			failing[failure.Name] = true
		}
		return failing, nil
	})
}

func recordCIRerun(ctx context.Context, db *store.DB, rerun store.CIRerun, requestErr error) error {
	id, inserted, err := db.RecordCIRerun(ctx, rerun)
	if err != nil || !inserted || requestErr == nil {
		return err
	}
	return db.FailCIRerunRequest(ctx, id)
}
