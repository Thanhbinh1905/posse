package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) prepareTaskAdoption(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, source store.Task) (*store.TaskAdoptionRequest, error) {
	refuse := func(reason string) (*store.TaskAdoptionRequest, error) {
		return nil, axi.Failure("adoption_refused", reason, false)
	}
	if project.IsWorkspace() {
		return refuse("adopt currently requires a single-repository Project")
	}
	if source.State == store.StateMerged || source.State == store.StateMerging {
		return refuse("the source Task is merged or merging")
	}
	if len(taskContainmentProcessDetails(source)) > 0 {
		return refuse("the source Task still has contained or detached executor processes")
	}
	home, err := s.homePath()
	if err != nil {
		return nil, err
	}
	processes, err := taskScratchProcessDetails(home, project, source)
	if err != nil {
		return nil, err
	}
	if len(processes) > 0 {
		return refuse("the source Task still has processes in its scratch directory")
	}
	if source.SandboxID != 0 {
		head, err := gitOutput(ctx, source.WorktreePath, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		branch, err := gitOutput(ctx, source.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return nil, err
		}
		expected, err := taskBranchAuthorHead(ctx, project.Root, source.Branch)
		if err != nil {
			return nil, err
		}
		if head != expected || branch != source.Branch {
			return refuse("the source Sandbox does not hold the expected branch and head")
		}
		processes, err := sandboxTaskProcessDetails(source.WorktreePath)
		if err != nil {
			return nil, err
		}
		if len(processes) > 0 {
			return refuse("the source Task still has processes in its Sandbox")
		}
		status, err := gitStatusOutput(ctx, source.WorktreePath, "--porcelain")
		if err != nil {
			return nil, err
		}
		if status != "" {
			return refuse("the source Sandbox has uncommitted changes")
		}
	}
	repository, err := gitCommonDir(ctx, project.Root)
	if err != nil {
		return nil, err
	}
	claim, err := db.BranchAuthor(ctx, repository, source.Branch)
	if err != nil {
		if !store.IsNotFound(err) {
			return nil, err
		}
		return refuse("the source Task has no exclusive branch claim")
	}
	if claim.TaskID != source.ID {
		return refuse("the source Task no longer owns its branch")
	}
	head, err := taskBranchAuthorHead(ctx, project.Root, source.Branch)
	if err != nil {
		return nil, err
	}
	if head != claim.HeadSHA {
		return refuse("the source branch differs from its expected head")
	}
	remote, err := remoteBranchHead(ctx, project.Root, "refs/heads/"+source.Branch)
	if err != nil {
		return nil, err
	}
	if remote != head {
		return refuse("the remote branch differs from its expected head")
	}
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return nil, err
	}
	number, err := forgeReference(source.PRURL, forge)
	if err != nil {
		return nil, err
	}
	if forge.Kind == "gitlab" {
		mr, err := gitlabMR(ctx, forge, number)
		if err != nil {
			return nil, err
		}
		if mr.State != "opened" || mr.SHA != head || mr.SourceBranch != source.Branch || mr.TargetBranch != taskTargetBranch(project, source) || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID {
			return refuse("the source merge request is not open at its expected branch and head")
		}
	} else {
		output, err := commandOutputArgs(ctx, project.Root, "gh", "pr", "view", source.PRURL, "--json", "url,state,headRefOid,headRefName,baseRefName,headRepository")
		if err != nil {
			return nil, err
		}
		var pull struct {
			URL        string `json:"url"`
			State      string `json:"state"`
			Head       string `json:"headRefOid"`
			Branch     string `json:"headRefName"`
			Base       string `json:"baseRefName"`
			Repository struct {
				Name string `json:"nameWithOwner"`
			} `json:"headRepository"`
		}
		if err := json.Unmarshal([]byte(output), &pull); err != nil {
			return nil, fmt.Errorf("read adoption pull request: %w", err)
		}
		if pull.URL != source.PRURL || pull.State != "OPEN" || pull.Head != head || pull.Branch != source.Branch || pull.Base != taskTargetBranch(project, source) || !strings.EqualFold(pull.Repository.Name, forge.Path) {
			return refuse("the source pull request is not open at its expected branch and head")
		}
	}
	confirmed, err := taskBranchAuthorHead(ctx, project.Root, source.Branch)
	if err != nil {
		return nil, err
	}
	if confirmed != head {
		return refuse("the source branch changed while adoption was checked")
	}
	remote, err = remoteBranchHead(ctx, project.Root, "refs/heads/"+source.Branch)
	if err != nil {
		return nil, err
	}
	if remote != head {
		return refuse("the remote branch changed while adoption was checked")
	}
	return &store.TaskAdoptionRequest{SourceTaskID: source.ID, Repository: repository, Branch: source.Branch, PRURL: source.PRURL, ExpectedHead: head, ExpectedGeneration: claim.Generation, PreviousWriterStopped: true}, nil
}
