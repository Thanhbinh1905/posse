package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type discardPullRequest struct {
	project store.Project
	task    store.Task
	repo    string
	url     string
}

func (s *Service) discardPullRequests(ctx context.Context, db *store.DB, project store.Project, task store.Task) ([]discardPullRequest, error) {
	// A merged Task's recorded PR is already authoritative. The only approved
	// discard path from merged is the unrecoverable-Orphan Decision, which
	// must dispose of local unsaved work without treating the merged PR as open.
	if task.State == store.StateMerged {
		return nil, nil
	}
	if !project.IsWorkspace() {
		if task.PRURL == "" {
			return nil, nil
		}
		return []discardPullRequest{{project: project, task: task, url: task.PRURL}}, nil
	}
	members, err := workspaceMembers(ctx, db, project, task)
	if err != nil {
		return nil, err
	}
	requests := make([]discardPullRequest, 0, len(members))
	for _, member := range members {
		if member.repo.PRURL == "" {
			continue
		}
		requests = append(requests, discardPullRequest{
			project: member.project,
			task:    member.task,
			repo:    member.repo.Repo,
			url:     member.repo.PRURL,
		})
	}
	return requests, nil
}

func (s *Service) requireDiscardPullRequestsUnmerged(ctx context.Context, db *store.DB, requests []discardPullRequest, cfg config.Config) error {
	for _, request := range requests {
		if err := verifyDiscardPRStillRecorded(ctx, db, request); err != nil {
			return err
		}
		observation, err := s.inspectDiscardPullRequest(ctx, request, cfg)
		if err != nil {
			return err
		}
		if observation.State == "MERGED" {
			return discardMergedPullRequest(request)
		}
	}
	return nil
}

// closeAndVerifyDiscardPullRequests is idempotent across retries: already
// CLOSED requests are accepted, OPEN requests are closed, and every close is
// followed by a fresh forge read. It runs before pane or Sandbox teardown.
func (s *Service) closeAndVerifyDiscardPullRequests(ctx context.Context, db *store.DB, requests []discardPullRequest, cfg config.Config) error {
	for _, request := range requests {
		if err := verifyDiscardPRStillRecorded(ctx, db, request); err != nil {
			return err
		}
		before, err := s.inspectDiscardPullRequest(ctx, request, cfg)
		if err != nil {
			return err
		}
		switch before.State {
		case "MERGED":
			return discardMergedPullRequest(request)
		case "CLOSED":
			if err := s.recordDiscardPRClosure(ctx, db, request, before); err != nil {
				return err
			}
			continue
		case "OPEN":
		default:
			return axi.Failure("pr_close_failed", "forge returned an unsupported pull request state", true, before.State)
		}

		closeOutput, closeErr := closeRecordedPullRequest(ctx, request, cfg)
		verified, verifyErr := s.inspectDiscardPullRequest(ctx, request, cfg)
		if verifyErr != nil {
			return axi.Failure("pr_close_failed", "could not verify the recorded pull request after its close request", true, strings.TrimSpace(closeOutput), errorText(closeErr), verifyErr.Error())
		}
		switch verified.State {
		case "CLOSED":
			if err := s.recordDiscardPRClosure(ctx, db, request, verified); err != nil {
				return err
			}
		case "MERGED":
			return discardMergedPullRequest(request)
		case "OPEN":
			return axi.Failure("pr_close_failed", "forge left the recorded pull request open; discard keeps the Sandbox held", true, strings.TrimSpace(closeOutput), errorText(closeErr))
		default:
			return axi.Failure("pr_close_failed", "forge returned an unsupported pull request state after close", true, verified.State, strings.TrimSpace(closeOutput), errorText(closeErr))
		}
		if err := verifyDiscardPRStillRecorded(ctx, db, request); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) inspectDiscardPullRequest(ctx context.Context, request discardPullRequest, cfg config.Config) (store.PRObservation, error) {
	forge, err := forgeForRepository(ctx, request.project.Root, cfg, request.repo)
	if err != nil {
		return store.PRObservation{}, err
	}
	number, err := forgeReference(request.url, forge)
	if err != nil {
		return store.PRObservation{}, axi.Failure("pr_url_invalid", "recorded pull request URL does not match the configured forge repository", false, err.Error())
	}
	observation := store.PRObservation{
		ProjectID:  request.task.ProjectID,
		TaskID:     request.task.ID,
		PRURL:      request.url,
		ObservedAt: time.Now().UnixMilli(),
	}
	if forge.Kind == "gitlab" {
		mr, err := gitlabMR(ctx, forge, number)
		if err != nil {
			return store.PRObservation{}, axi.Failure("pr_view_failed", "could not verify the recorded merge request", true, err.Error())
		}
		if mr.WebURL != request.url {
			return store.PRObservation{}, axi.Failure("pr_url_invalid", "forge returned a different URL for the recorded merge request", false, mr.WebURL)
		}
		observation.HeadSHA = mr.SHA
		observation.State = map[string]string{"opened": "OPEN", "merged": "MERGED", "closed": "CLOSED"}[strings.ToLower(mr.State)]
		if observation.State == "" {
			return store.PRObservation{}, axi.Failure("pr_view_failed", "forge returned an unknown merge request state", true, mr.State)
		}
		return observation, nil
	}

	output, err := commandOutputArgs(ctx, request.project.Root, "gh", "pr", "view", request.url, "--json", "url,state,headRefOid")
	if err != nil {
		return store.PRObservation{}, axi.Failure("pr_view_failed", "could not verify the recorded pull request", true, strings.TrimSpace(output))
	}
	var pull struct {
		URL        string `json:"url"`
		State      string `json:"state"`
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return store.PRObservation{}, axi.Failure("pr_view_failed", "forge returned invalid pull request data", true, err.Error())
	}
	if pull.URL != request.url {
		return store.PRObservation{}, axi.Failure("pr_url_invalid", "forge returned a different URL for the recorded pull request", false, pull.URL)
	}
	observation.HeadSHA = pull.HeadRefOID
	observation.State = strings.ToUpper(pull.State)
	if observation.State != "OPEN" && observation.State != "CLOSED" && observation.State != "MERGED" {
		return store.PRObservation{}, axi.Failure("pr_view_failed", "forge returned an unknown pull request state", true, pull.State)
	}
	return observation, nil
}

func closeRecordedPullRequest(ctx context.Context, request discardPullRequest, cfg config.Config) (string, error) {
	forge, err := forgeForRepository(ctx, request.project.Root, cfg, request.repo)
	if err != nil {
		return "", err
	}
	number, err := forgeReference(request.url, forge)
	if err != nil {
		return "", axi.Failure("pr_url_invalid", "recorded pull request URL does not match the configured forge repository", false, err.Error())
	}
	if forge.Kind == "gitlab" {
		return commandOutputArgs(ctx, request.project.Root, "glab", "mr", "close", strconv.Itoa(number), "--repo", "https://"+forge.Host+"/"+forge.Path)
	}
	return commandOutputArgs(ctx, request.project.Root, "gh", "pr", "close", request.url)
}

func (s *Service) recordDiscardPRClosure(ctx context.Context, db *store.DB, request discardPullRequest, observation store.PRObservation) error {
	if err := verifyDiscardPRStillRecorded(ctx, db, request); err != nil {
		return err
	}
	if request.repo == "" {
		previous, err := db.LatestPRObservation(ctx, request.task.ID)
		if err == nil && previous.PRURL == observation.PRURL && previous.HeadSHA == observation.HeadSHA {
			observation.Checks = previous.Checks
			observation.Review = previous.Review
			observation.Mergeable = previous.Mergeable
			observation.MergeCommit = previous.MergeCommit
		} else if err != nil && !store.IsNotFound(err) {
			return err
		}
		if _, err := db.RecordPRObservation(ctx, observation, store.PRObservationEffect{}, request.task); err != nil {
			return err
		}
		return verifyDiscardPRStillRecorded(ctx, db, request)
	}
	previous, err := db.LatestMemberPRObservation(ctx, request.task.ID, request.repo)
	if err == nil && previous.PRURL == observation.PRURL && previous.HeadSHA == observation.HeadSHA {
		observation.Checks = previous.Checks
		observation.Review = previous.Review
		observation.Mergeable = previous.Mergeable
		observation.MergeCommit = previous.MergeCommit
	} else if err != nil && !store.IsNotFound(err) {
		return err
	}
	if _, err = db.RecordMemberPRObservation(ctx, request.repo, observation, store.MemberPREffect{}); err != nil {
		return err
	}
	return verifyDiscardPRStillRecorded(ctx, db, request)
}

func verifyDiscardPRStillRecorded(ctx context.Context, db *store.DB, request discardPullRequest) error {
	current, err := db.TaskByID(ctx, request.task.ProjectID, request.task.ID)
	if err != nil {
		return err
	}
	if current.State != request.task.State {
		if current.State == store.StateMerged {
			return discardMergedPullRequest(request)
		}
		return axi.Failure("pr_discard_raced", "Task state changed while verifying its recorded pull request; discard keeps the Sandbox held", true)
	}
	if request.repo == "" {
		if current.PRURL != request.url || current.GatedSHA != request.task.GatedSHA {
			return axi.Failure("pr_discard_raced", "recorded pull request changed while discard was in progress; discard keeps the Sandbox held", true)
		}
		return nil
	}
	repos, err := db.TaskRepos(ctx, request.task.ID)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if repo.Repo == request.repo {
			if repo.PRURL != request.url || repo.GatedSHA != request.task.GatedSHA {
				return axi.Failure("pr_discard_raced", "recorded Member pull request changed while discard was in progress; discard keeps the Sandbox held", true)
			}
			return nil
		}
	}
	return axi.Failure("pr_discard_raced", "recorded Member pull request disappeared while discard was in progress; discard keeps the Sandbox held", true)
}

func discardMergedPullRequest(request discardPullRequest) error {
	return axi.Failure("pr_merged", "recorded pull request has merged; approved discard was refused", false, request.url, fmt.Sprintf("Run `posse show %s` to inspect its Merge state", taskIDString(request.task.Seq)))
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
