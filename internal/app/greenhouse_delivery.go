package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
		} `json:"pipeline"`
	} `json:"run"`
	Delivery struct {
		PushedHead string `json:"pushed_head"`
		PRURL      string `json:"pr_url"`
		PRNumber   int    `json:"pr_number"`
		Current    bool   `json:"current"`
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
	if (submittedBranch != branch && submittedBranch != "refs/heads/"+branch) || result.Run.Submitted.Head != head || result.Run.Candidate != head {
		return result, fmt.Errorf("greenhouse run does not bind submitted and candidate heads to Task branch %s at %s", branch, head)
	}
	if result.Run.Status != "ci-ready" || result.Run.Pipeline.Status != "ci-ready" {
		return result, fmt.Errorf("greenhouse run %s has not reached ci-ready", runID)
	}
	if !result.Delivery.Current || result.Delivery.PushedHead != head || result.Delivery.PRURL == "" || result.Delivery.PRNumber <= 0 {
		return result, fmt.Errorf("greenhouse delivery is not current at Task head %s", head)
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

func inspectGreenhouseDelivery(ctx context.Context, worktree, runID, branch, head string) (greenhouseDeliveryResult, error) {
	request, _ := json.Marshal(map[string]any{"version": 1, "op": "inspect", "run_id": runID})
	command := exec.CommandContext(ctx, "greenhouse", "axi")
	command.Dir = worktree
	command.Stdin = bytes.NewReader(request)
	command.Env = environmentWithout(os.Environ(), "GREENHOUSE_HOME")
	output, err := command.Output()
	if err != nil {
		return greenhouseDeliveryResult{}, fmt.Errorf("greenhouse AXI inspect failed: %w", err)
	}
	return parseGreenhouseDelivery(output, runID, branch, head)
}

func validateGreenhousePullRequest(output []byte, head, expectedURL string, expectedNumber int) error {
	var pull struct {
		State  string            `json:"state"`
		Head   string            `json:"headRefOid"`
		URL    string            `json:"url"`
		Number int               `json:"number"`
		Checks []json.RawMessage `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(output, &pull); err != nil {
		return fmt.Errorf("decode Greenhouse PR observation: %w", err)
	}
	if pull.State != "OPEN" || pull.Head != head || pull.URL != expectedURL || pull.Number != expectedNumber || len(pull.Checks) == 0 {
		return fmt.Errorf("PR is not open with checks at gated head %s", head)
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
	if forge.Kind == "gitlab" {
		number, err := forgeReference(delivery.Delivery.PRURL, forge)
		if err != nil || number != delivery.Delivery.PRNumber {
			return fmt.Errorf("greenhouse merge request does not match the Project forge")
		}
		mr, err := gitlabMR(ctx, forge, number)
		if err != nil {
			return fmt.Errorf("forge could not verify Greenhouse merge request: %w", err)
		}
		if mr.WebURL != delivery.Delivery.PRURL || mr.State != "opened" || mr.SHA != head || mr.HeadPipeline == nil || mr.HeadPipeline.Status != "success" {
			return fmt.Errorf("greenhouse merge request is not open with a successful pipeline at gated head %s", head)
		}
	} else {
		command := exec.CommandContext(ctx, "gh", "pr", "view", delivery.Delivery.PRURL, "--json", "state,headRefOid,url,number,statusCheckRollup")
		command.Dir = task.WorktreePath
		output, err := command.Output()
		if err != nil {
			return fmt.Errorf("forge could not verify Greenhouse PR: %w", err)
		}
		if err := validateGreenhousePullRequest(output, head, delivery.Delivery.PRURL, delivery.Delivery.PRNumber); err != nil {
			return err
		}
	}
	task.PRURL = delivery.Delivery.PRURL
	if err := validateWorkerPullRequest(ctx, project, task, forge, task.PRURL, head); err != nil {
		return fmt.Errorf("greenhouse PR does not match the Task branch and target: %w", err)
	}
	return nil
}

func environmentWithout(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}
