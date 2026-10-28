package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

type githubSpecIssue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// raiseSpecReadyToDeliver tells the Lead that the parent spec of a merged
// ticket has no open sub-issue left, once per spec. GitHub closes the merged
// ticket's issue asynchronously, so that issue counts as closed.
func (s *Service) raiseSpecReadyToDeliver(ctx context.Context, db *store.DB, project store.Project, task store.Task, forge repositoryForge) error {
	if forge.Kind != "github" {
		return nil
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	brief, err := readTaskBrief(home, project, task)
	if os.IsNotExist(err) || err == nil && (brief.Ticket == nil || brief.Ticket.Repository != "") {
		return nil
	}
	if err != nil {
		return err
	}
	ticket := brief.Ticket.Number
	spec, found, err := githubParentIssue(ctx, forge, ticket)
	if err != nil || !found {
		return err
	}
	children, err := githubSubIssues(ctx, forge, spec)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.Number != ticket && child.State != "closed" {
			return nil
		}
	}
	_, err = db.CreateSpecReadyNoticeOnce(ctx, project.ID, task.ID, spec, fmt.Sprintf("Spec #%d is ready to deliver: every sub-issue is closed after %s merged", spec, task.Title))
	return err
}

func githubParentIssue(ctx context.Context, forge repositoryForge, number int) (int, bool, error) {
	endpoint := fmt.Sprintf("repos/%s/issues/%d/parent", strings.Trim(forge.Path, "/"), number)
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "api", "--hostname", forge.Host, endpoint)
	if err != nil {
		if issueNotFound(output, err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read GitHub parent issue: %w: %s", err, strings.TrimSpace(output))
	}
	var parent githubSpecIssue
	if err := json.Unmarshal([]byte(output), &parent); err != nil || parent.Number < 1 {
		return 0, false, fmt.Errorf("GitHub parent issue response was invalid")
	}
	return parent.Number, true, nil
}

func githubSubIssues(ctx context.Context, forge repositoryForge, number int) ([]githubSpecIssue, error) {
	endpoint := fmt.Sprintf("repos/%s/issues/%d/sub_issues?per_page=100", strings.Trim(forge.Path, "/"), number)
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "api", "--hostname", forge.Host, "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("read GitHub sub-issues: %w: %s", err, strings.TrimSpace(output))
	}
	var pages [][]githubSpecIssue
	if err := json.Unmarshal([]byte(output), &pages); err != nil {
		return nil, fmt.Errorf("GitHub sub-issues response did not contain complete paginated JSON")
	}
	var children []githubSpecIssue
	for _, page := range pages {
		for _, child := range page {
			if child.Number < 1 || child.State == "" {
				return nil, fmt.Errorf("GitHub sub-issues response contains an incomplete issue")
			}
			children = append(children, child)
		}
	}
	return children, nil
}
