package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// repositoryForge is resolved for each repository, never once for a workspace.
type repositoryForge struct{ Kind, Host, Path, Root string }

func forgeForRepository(ctx context.Context, root string, cfg config.Config, member string) (repositoryForge, error) {
	remote, err := gitOutput(ctx, root, "config", "--get", "remote.origin.url")
	if err != nil {
		return repositoryForge{}, err
	}
	host, path, err := remoteRepository(remote)
	if err != nil {
		return repositoryForge{}, axi.Failure("pr_origin_invalid", err.Error(), false)
	}
	kind := cfg.Defaults.Forge
	if member != "" {
		if override, ok := cfg.Repositories[member]; ok && override.Forge != "" {
			kind = override.Forge
		}
	}
	if kind == "" || kind == "auto" {
		switch host {
		case "github.com":
			kind = "github"
		case "gitlab.com":
			kind = "gitlab"
		default:
			// An enterprise host must be authenticated with exactly one forge CLI.
			_, glabErr := commandOutputArgs(ctx, root, "glab", "auth", "status", "--hostname", host)
			_, ghErr := commandOutputArgs(ctx, root, "gh", "auth", "status", "--hostname", host)
			switch {
			case glabErr == nil && ghErr == nil:
				return repositoryForge{}, axi.Failure("pr_forge_ambiguous", "origin host is authenticated with both gh and glab: "+host, false, "Set defaults.forge or repositories.<repository>.forge")
			case glabErr == nil:
				kind = "gitlab"
			case ghErr == nil:
				kind = "github"
			}
		}
	}
	if kind != "github" && kind != "gitlab" {
		return repositoryForge{}, axi.Failure("pr_forge_unknown", "origin host has no configured forge: "+host, false, "Set defaults.forge to github or gitlab for this Project")
	}
	return repositoryForge{Kind: kind, Host: host, Path: path, Root: root}, nil
}

func validateForgeMergeMethod(forge repositoryForge, cfg config.Config) error {
	if forge.Kind == "gitlab" && cfg.Defaults.MergeMethod == "rebase" {
		return axi.Failure("pr_merge_method_unsupported", "GitLab cannot use defaults.merge_method = rebase for pushed Task branches", false, "Set defaults.merge_method to squash or merge for this Project")
	}
	return nil
}

func remoteRepository(value string) (string, string, error) {
	remote := strings.TrimSpace(value)
	var host, path string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
			return "", "", errors.New("origin must use HTTPS or SSH")
		}
		host, path = parsed.Hostname(), parsed.Path
	} else {
		if at := strings.LastIndex(remote, "@"); at >= 0 {
			remote = remote[at+1:]
		}
		parts := strings.SplitN(remote, ":", 2)
		if len(parts) != 2 {
			return "", "", errors.New("origin must use HTTPS, SSH or SCP syntax")
		}
		host, path = parts[0], parts[1]
	}
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	if host == "" || path == "" || strings.Contains(path, "..") || strings.ContainsAny(path, "?#") {
		return "", "", errors.New("invalid origin repository")
	}
	return strings.ToLower(host), path, nil
}

func forgeReference(value string, forge repositoryForge) (int, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.EqualFold(parsed.Host, forge.Host) {
		return 0, errors.New("pull request URL has a different host or is not HTTPS")
	}
	prefix := "/" + forge.Path
	suffix := strings.TrimPrefix(parsed.EscapedPath(), prefix)
	if !strings.HasPrefix(parsed.EscapedPath(), prefix+"/") {
		return 0, errors.New("pull request repository differs from origin")
	}
	marker := "/pull/"
	if forge.Kind == "gitlab" {
		marker = "/-/merge_requests/"
	}
	if !strings.HasPrefix(suffix, marker) {
		return 0, errors.New("invalid pull request URL path")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(suffix, marker))
	if err != nil || n < 1 {
		return 0, errors.New("invalid pull request number")
	}
	return n, nil
}

func gitlabAPI(ctx context.Context, forge repositoryForge, endpoint string) (string, error) {
	return commandOutputArgs(ctx, forge.Root, "glab", "api", "--hostname", forge.Host, "projects/"+url.PathEscape(forge.Path)+"/"+endpoint)
}

func gitlabMR(ctx context.Context, forge repositoryForge, number int) (gitlabMergeRequest, error) {
	output, err := gitlabAPI(ctx, forge, "merge_requests/"+strconv.Itoa(number))
	if err != nil {
		return gitlabMergeRequest{}, fmt.Errorf("glab MR view: %w: %s", err, output)
	}
	var mr gitlabMergeRequest
	if err := json.Unmarshal([]byte(output), &mr); err != nil {
		return mr, err
	}
	return mr, nil
}

// Keeps GitLab REST shape at the forge boundary.
type gitlabMergeRequest struct {
	WebURL            string `json:"web_url"`
	Description       string `json:"description"`
	State             string `json:"state"`
	SHA               string `json:"sha"`
	SourceBranch      string `json:"source_branch"`
	TargetBranch      string `json:"target_branch"`
	SourceProjectID   int64  `json:"source_project_id"`
	TargetProjectID   int64  `json:"target_project_id"`
	MergeStatus       string `json:"detailed_merge_status"`
	LegacyMergeStatus string `json:"merge_status"`
	MergeCommitSHA    string `json:"merge_commit_sha"`
	SquashCommitSHA   string `json:"squash_commit_sha"`
	HasConflicts      bool   `json:"has_conflicts"`
	HeadPipeline      *struct {
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

func (s *Service) findOrCreateGitLabMR(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent, forge repositoryForge, member string, summary ...string) (string, bool, error) {
	output, err := runOutputStep(ctx, db, intent, "pr.lookup", forge.Root, "glab", "api", "--hostname", forge.Host, "projects/"+url.PathEscape(forge.Path)+"/merge_requests?state=opened&source_branch="+url.QueryEscape(task.Branch)+"&target_branch="+url.QueryEscape(project.DefaultBranch))
	if err != nil {
		return "", false, axi.Failure("pr_list_failed", "could not list GitLab merge requests", true, err.Error())
	}
	var existing []gitlabMergeRequest
	if err := json.Unmarshal([]byte(output), &existing); err != nil {
		return "", false, err
	}
	for _, mr := range existing {
		if mr.SourceBranch != task.Branch || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID {
			continue // A fork's branch (or an unidentified source) is not this Project's Worker branch.
		}
		if mr.SHA != task.GatedSHA {
			return "", false, axi.Failure("branch_moved", "existing merge request has a different head", false)
		}
		if _, err := forgeReference(mr.WebURL, forge); err != nil {
			return "", false, err
		}
		return mr.WebURL, false, nil
	}
	title, body, err := prDetails(ctx, db, project, task, s.homePath, member, summary...)
	if err != nil {
		return "", false, err
	}
	created, err := runOutputStep(ctx, db, intent, "pr.create", forge.Root, "glab", "mr", "create", "--repo", "https://"+forge.Host+"/"+forge.Path, "--source-branch", task.Branch, "--target-branch", project.DefaultBranch, "--title", title, "--description", body, "--yes")
	if err != nil {
		return "", false, axi.Failure("pr_create_failed", "could not create GitLab merge request", true, err.Error())
	}
	for _, line := range strings.Fields(created) {
		if _, err := forgeReference(line, forge); err == nil {
			return line, true, nil
		}
	}
	return "", false, axi.Failure("pr_create_failed", "glab returned no merge request URL", true, created)
}

func gitlabObservation(ctx context.Context, forge repositoryForge, task store.Task, now int64) (store.PRObservation, []failedCheck, error) {
	number, err := forgeReference(task.PRURL, forge)
	if err != nil {
		return store.PRObservation{}, nil, err
	}
	mr, err := gitlabMR(ctx, forge, number)
	if err != nil {
		return store.PRObservation{}, nil, err
	}
	if mr.WebURL != task.PRURL || mr.SHA == "" {
		return store.PRObservation{}, nil, errors.New("GitLab MR URL or head SHA mismatch")
	}
	state := map[string]string{"opened": "OPEN", "merged": "MERGED", "closed": "CLOSED"}[mr.State]
	if mr.MergeStatus == "" {
		mr.MergeStatus = mr.LegacyMergeStatus
	}
	if state == "" {
		return store.PRObservation{}, nil, fmt.Errorf("unknown GitLab MR state %q", mr.State)
	}
	checks := checkSnapshot{State: "SUCCESS"}
	if state == "OPEN" && mr.MergeStatus != "mergeable" {
		checks.State = "PENDING"
	}
	if mr.HeadPipeline != nil {
		switch mr.HeadPipeline.Status {
		case "success":
			// Mergeability still controls land_ready below.
		case "running", "pending", "created", "waiting_for_resource", "preparing", "manual", "scheduled":
			checks.State = "PENDING"
		case "failed", "canceled", "skipped":
			checks.State = "FAILURE"
			checks.Failures = []failedCheck{{Name: "pipeline", URL: mr.HeadPipeline.WebURL}}
		default:
			checks.State = "PENDING"
		}
	}
	output := ""
	if state == "OPEN" {
		output, err = gitlabAPI(ctx, forge, "merge_requests/"+strconv.Itoa(number)+"/approvals")
	}
	if err != nil {
		return store.PRObservation{}, nil, err
	}
	var approvals struct {
		Approved      bool  `json:"approved"`
		ApprovedBy    []any `json:"approved_by"`
		ApprovalsLeft int   `json:"approvals_left"`
	}
	if state == "OPEN" {
		if err := json.Unmarshal([]byte(output), &approvals); err != nil {
			return store.PRObservation{}, nil, err
		}
	}
	review := "APPROVED"
	if !approvals.Approved && (approvals.ApprovalsLeft > 0 || mr.MergeStatus == "not_approved") {
		review = "REVIEW_REQUIRED"
	}
	if mr.MergeStatus == "discussions_not_resolved" {
		review = "CHANGES_REQUESTED"
	}
	mergeable := "UNKNOWN"
	if mr.HasConflicts || mr.MergeStatus == "conflict" {
		mergeable = "CONFLICTING"
	} else if mr.MergeStatus == "mergeable" {
		mergeable = "MERGEABLE"
	}
	encoded := marshalJSON(checks)
	commit := mr.MergeCommitSHA
	if commit == "" {
		commit = mr.SquashCommitSHA
	}
	if state == "MERGED" && commit == "" {
		// Fast-forward merges do not have a distinct merge or squash commit.
		commit = mr.SHA
	}
	return store.PRObservation{ProjectID: task.ProjectID, TaskID: task.ID, PRURL: task.PRURL, HeadSHA: mr.SHA, State: state, Checks: encoded, Review: review, Mergeable: mergeable, MergeCommit: commit, ObservedAt: now}, checks.Failures, nil
}
