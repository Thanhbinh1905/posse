package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type greenhouseDeliveryResult struct {
	RunID string `json:"run_id"`
	Run   struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Submitted struct {
			Branch string `json:"branch"`
			Head   string `json:"head"`
		} `json:"submitted"`
		Candidate string `json:"candidate"`
		Pipeline  struct {
			Status string `json:"status"`
			Ref    string `json:"ref"`
		} `json:"pipeline"`
	} `json:"run"`
	Delivery struct {
		PushedHead string `json:"pushed_head"`
		PRURL      string `json:"pr_url"`
		PRNumber   int    `json:"pr_number"`
		Checks     []struct {
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
			Head       string  `json:"head"`
		} `json:"checks"`
	} `json:"delivery"`
}

func parseGreenhouseDelivery(output []byte, runID, branch, head string) (greenhouseDeliveryResult, error) {
	var result greenhouseDeliveryResult
	if err := json.Unmarshal(output, &result); err != nil {
		return result, fmt.Errorf("decode Greenhouse AXI inspect: %w", err)
	}
	if result.RunID != runID || result.Run.ID != runID {
		return result, fmt.Errorf("greenhouse AXI returned a different run than %s", runID)
	}
	submittedBranch := result.Run.Submitted.Branch
	if (submittedBranch != branch && submittedBranch != "refs/heads/"+branch) || !validGreenhouseObjectID(result.Run.Submitted.Head) || !validGreenhouseObjectID(result.Run.Candidate) || result.Run.Candidate != head {
		return result, fmt.Errorf("greenhouse run does not bind its submitted Task branch and candidate to Task branch %s at %s", branch, head)
	}
	if result.Run.Pipeline.Ref != greenhouseCandidateRef(runID) {
		return result, fmt.Errorf("greenhouse run %s has an unexpected candidate branch %q", runID, result.Run.Pipeline.Ref)
	}
	if result.Run.Status != "ci-ready" || result.Run.Pipeline.Status != "ci-ready" {
		return result, fmt.Errorf("greenhouse run %s has not reached ci-ready", runID)
	}
	if result.Delivery.PushedHead != head || result.Delivery.PRURL == "" || result.Delivery.PRNumber <= 0 {
		return result, fmt.Errorf("greenhouse delivery does not identify the current Task head %s", head)
	}
	if len(result.Delivery.Checks) == 0 {
		return result, fmt.Errorf("greenhouse run %s has no required CI checks", runID)
	}
	seen := make(map[string]bool, len(result.Delivery.Checks))
	for _, check := range result.Delivery.Checks {
		if check.Name == "" || seen[check.Name] || check.Status != "completed" || check.Conclusion == nil || *check.Conclusion != "success" || check.Head != head {
			return result, fmt.Errorf("greenhouse CI check %q is not successful at Task head %s", check.Name, head)
		}
		seen[check.Name] = true
	}
	return result, nil
}

func validGreenhouseObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func greenhouseCandidateRef(runID string) string {
	return "refs/heads/greenhouse/" + runID
}

func inspectGreenhouseDelivery(ctx context.Context, worktree, runID, branch, head string) (greenhouseDeliveryResult, error) {
	request, _ := json.Marshal(map[string]any{"version": 1, "op": "inspect", "run_id": runID})
	command := exec.CommandContext(ctx, "greenhouse", "axi")
	command.Dir = worktree
	command.Stdin = bytes.NewReader(request)
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return greenhouseDeliveryResult{}, fmt.Errorf("greenhouse AXI inspect failed: %w", err)
	}
	delivery, err := parseGreenhouseDelivery(output, runID, branch, head)
	if err != nil {
		return greenhouseDeliveryResult{}, err
	}
	if _, err := gitOutput(ctx, worktree, "merge-base", "--is-ancestor", delivery.Run.Submitted.Head, head); err != nil {
		return greenhouseDeliveryResult{}, fmt.Errorf("greenhouse candidate %s does not descend from submitted head %s", head, delivery.Run.Submitted.Head)
	}
	return delivery, nil
}

func validateGreenhousePullRequest(output []byte, head, expectedURL string, expectedNumber int, expectedBranch, expectedTarget, expectedRepository string) error {
	var pull struct {
		State      string `json:"state"`
		Head       string `json:"headRefOid"`
		Branch     string `json:"headRefName"`
		Target     string `json:"baseRefName"`
		Repository struct {
			Name string `json:"nameWithOwner"`
		} `json:"headRepository"`
		URL    string            `json:"url"`
		Number int               `json:"number"`
		Checks []json.RawMessage `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(output, &pull); err != nil {
		return fmt.Errorf("decode Greenhouse PR observation: %w", err)
	}
	if pull.State != "OPEN" || pull.Head != head || pull.Branch != expectedBranch || pull.Target != expectedTarget || !strings.EqualFold(pull.Repository.Name, expectedRepository) || pull.URL != expectedURL || pull.Number != expectedNumber || len(pull.Checks) == 0 {
		return fmt.Errorf("PR is not open with checks at gated head %s on the expected Greenhouse branch and target (state=%q head=%q branch=%q target=%q repository=%q URL=%q number=%d checks=%d)", head, pull.State, pull.Head, pull.Branch, pull.Target, pull.Repository.Name, pull.URL, pull.Number, len(pull.Checks))
	}
	for _, encoded := range pull.Checks {
		var check struct {
			Status     string  `json:"status"`
			Conclusion *string `json:"conclusion"`
			State      string  `json:"state"`
		}
		if err := json.Unmarshal(encoded, &check); err != nil {
			return fmt.Errorf("decode PR check: %w", err)
		}
		if check.State != "" {
			if check.State != "SUCCESS" {
				return fmt.Errorf("PR has a non-success status context")
			}
			continue
		}
		if check.Status != "COMPLETED" || check.Conclusion == nil || *check.Conclusion != "SUCCESS" {
			return fmt.Errorf("PR has a non-success check run")
		}
	}
	return nil
}

func verifyGreenhouseDeliveryPR(ctx context.Context, project store.Project, cfg config.Config, task store.Task, delivery greenhouseDeliveryResult, head string) error {
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return err
	}
	return verifyGreenhousePRAtHead(ctx, project, task, forge, delivery.RunID, delivery.Delivery.PRURL, delivery.Delivery.PRNumber, head)
}

func verifyGreenhousePRAtHead(ctx context.Context, project store.Project, task store.Task, forge repositoryForge, runID, prURL string, prNumber int, head string) error {
	candidateRef := greenhouseCandidateRef(runID)
	candidateRemoteHead, err := remoteBranchHead(ctx, task.WorktreePath, candidateRef)
	if err != nil {
		return fmt.Errorf("could not verify the pushed Greenhouse candidate branch: %w", err)
	}
	if candidateRemoteHead != head {
		return fmt.Errorf("pushed Greenhouse candidate branch %s is not at Task head %s", candidateRef, head)
	}
	taskRemoteHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
	if err != nil {
		return fmt.Errorf("could not verify the pushed Task branch: %w", err)
	}
	if taskRemoteHead != head {
		return fmt.Errorf("pushed Task branch %s is not at gated head %s", task.Branch, head)
	}
	branch := strings.TrimPrefix(candidateRef, "refs/heads/")
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil || number != prNumber {
			return fmt.Errorf("greenhouse merge request does not match the Project forge")
		}
		mr, err := gitlabMR(ctx, forge, number)
		if err != nil {
			return fmt.Errorf("forge could not verify Greenhouse merge request: %w", err)
		}
		if mr.WebURL != prURL || mr.State != "opened" || mr.SHA != head || mr.SourceBranch != branch || mr.TargetBranch != taskTargetBranch(project, task) || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID || mr.HeadPipeline == nil || mr.HeadPipeline.Status != "success" {
			return fmt.Errorf("greenhouse merge request is not open with a successful pipeline at gated head %s", head)
		}
		return nil
	}
	command := exec.CommandContext(ctx, "gh", "pr", "view", prURL, "--json", "state,headRefOid,headRefName,baseRefName,headRepository,url,number,statusCheckRollup")
	command.Dir = task.WorktreePath
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("forge could not verify Greenhouse PR: %w", err)
	}
	if err := validateGreenhousePullRequest(output, head, prURL, prNumber, branch, taskTargetBranch(project, task), forge.Path); err != nil {
		return err
	}
	return nil
}

func greenhouseRunIDForHead(ctx context.Context, db *store.DB, task store.Task, prURL, head string) (string, error) {
	signals, err := db.TaskSignals(ctx, task.ID, 20)
	if err != nil {
		return "", err
	}
	for _, signal := range signals {
		if signal.Verb != "done" {
			continue
		}
		var data struct {
			RunID       string `json:"greenhouse_run_id"`
			PendingHead string `json:"greenhouse_pending_head"`
			PRURL       string `json:"pr_url"`
		}
		if json.Unmarshal([]byte(signal.DataJSON), &data) != nil {
			break
		}
		if data.RunID != "" && data.PendingHead == head && data.PRURL == prURL {
			return data.RunID, nil
		}
		break
	}
	return "", axi.Failure("greenhouse_run_missing", "the latest done Signal does not bind a Greenhouse run to this PR head", false)
}

func validateTaskPullRequest(ctx context.Context, db *store.DB, project store.Project, task store.Task, forge repositoryForge, prURL, head string) error {
	if task.MergeMode != "greenhouse" {
		return validateWorkerPullRequest(ctx, project, task, forge, prURL, head)
	}
	runID, err := greenhouseRunIDForHead(ctx, db, task, prURL, head)
	if err != nil {
		return err
	}
	number, err := forgeReference(prURL, forge)
	if err != nil {
		return err
	}
	return verifyGreenhousePRAtHead(ctx, project, task, forge, runID, prURL, number, head)
}
