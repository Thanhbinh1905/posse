package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func validateBriefIssueReferences(project store.Project, brief dispatch.Brief, targets []repoTarget) error {
	members := make(map[string]bool, len(targets))
	for _, target := range targets {
		members[target.Name] = true
	}
	for _, list := range []struct {
		field string
		refs  []dispatch.IssueRef
	}{{"issues", brief.Issues}, {"refs", brief.Refs}} {
		for _, ref := range list.refs {
			if project.IsWorkspace() {
				if ref.Repository == "" {
					return &dispatch.BriefError{Field: list.field, Reason: "workspace issue references must use member#number"}
				}
				if !members[ref.Repository] {
					return &dispatch.BriefError{Field: list.field, Reason: ref.Repository + " is not an active workspace member"}
				}
			} else if ref.Repository != "" {
				return &dispatch.BriefError{Field: list.field, Reason: "member#number is valid only in a workspace Project"}
			}
		}
	}
	return nil
}

func validateShipIssueTargets(brief dispatch.Brief, taskTargets []repoTarget) error {
	if brief.Type != "ship" {
		return nil
	}
	members := make(map[string]bool, len(taskTargets))
	for _, target := range taskTargets {
		members[target.Name] = true
	}
	for _, list := range []struct {
		field string
		refs  []dispatch.IssueRef
	}{{"issues", brief.Issues}, {"refs", brief.Refs}} {
		for _, ref := range list.refs {
			if !members[ref.Repository] {
				return &dispatch.BriefError{Field: list.field, Reason: ref.String() + " targets a member not listed in repos:"}
			}
		}
	}
	return nil
}

func (s *Service) checkBriefIssues(ctx context.Context, cfg config.Config, brief dispatch.Brief, targets []repoTarget) []any {
	byName := make(map[string]repoTarget, len(targets))
	for _, target := range targets {
		byName[target.Name] = target
	}
	forgeByName := make(map[string]repositoryForge)
	forgeErrors := make(map[string]error)
	forgeChecked := make(map[string]bool)
	warnings := make([]any, 0)
	for _, list := range []struct {
		refs []dispatch.IssueRef
	}{{brief.Issues}, {brief.Refs}} {
		for _, ref := range list.refs {
			key := ref.Repository
			target, found := byName[key]
			if !found {
				warnings = append(warnings, "could not verify issue "+ref.String()+": repository is unavailable")
				continue
			}
			forge := forgeByName[key]
			if !forgeChecked[key] {
				forgeChecked[key] = true
				var err error
				forge, err = forgeForRepository(ctx, target.Root, cfg, target.Name)
				if err != nil {
					forgeErrors[key] = err
				} else {
					forgeByName[key] = forge
				}
			}
			if err := forgeErrors[key]; err != nil {
				warnings = append(warnings, "could not verify issue "+ref.String()+": "+truncate(err.Error(), 180))
				continue
			}
			state, output, err := issueState(ctx, forge, ref.Number)
			if err != nil {
				if issueNotFound(output, err) {
					warnings = append(warnings, "issue "+ref.String()+" does not exist")
				} else {
					warnings = append(warnings, "could not verify issue "+ref.String()+": "+truncate(strings.TrimSpace(output+" "+err.Error()), 180))
				}
				continue
			}
			if strings.EqualFold(state, "closed") {
				warnings = append(warnings, "issue "+ref.String()+" is already closed")
			}
		}
	}
	return warnings
}

func issueState(ctx context.Context, forge repositoryForge, number int) (string, string, error) {
	if forge.Kind == "gitlab" {
		output, err := gitlabAPI(ctx, forge, "issues/"+strconv.Itoa(number))
		if err != nil {
			return "", output, err
		}
		var issue struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(output), &issue); err != nil || issue.State == "" {
			return "", output, fmt.Errorf("glab returned invalid issue data")
		}
		return issue.State, output, nil
	}
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "issue", "view", strconv.Itoa(number), "--repo", forge.Host+"/"+forge.Path, "--json", "state")
	if err != nil {
		return "", output, err
	}
	var issue struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(output), &issue); err != nil || issue.State == "" {
		return "", output, fmt.Errorf("gh returned invalid issue data")
	}
	return issue.State, output, nil
}

func (s *Service) ensureExistingPRIssueLinks(ctx context.Context, db *store.DB, intent store.Intent, forge repositoryForge, prURL, member string, brief dispatch.Brief) error {
	if len(issueLinkLines(brief, member)) == 0 {
		return nil
	}
	body := ""
	if forge.Kind == "gitlab" {
		number, err := forgeReference(prURL, forge)
		if err != nil {
			return err
		}
		endpoint := "projects/" + url.PathEscape(forge.Path) + "/merge_requests/" + strconv.Itoa(number)
		output, err := runOutputStep(ctx, db, intent, "pr.issue_links.read", forge.Root, "glab", "api", "--hostname", forge.Host, endpoint)
		if err != nil {
			return axi.Failure("pr_issue_links_failed", "could not read the existing merge request description", true, err.Error())
		}
		var request gitlabMergeRequest
		if err := json.Unmarshal([]byte(output), &request); err != nil {
			return axi.Failure("pr_issue_links_failed", "GitLab returned invalid merge request data", true, err.Error())
		}
		body = request.Description
	} else {
		output, err := runOutputStep(ctx, db, intent, "pr.issue_links.read", forge.Root, "gh", "pr", "view", prURL, "--json", "body")
		if err != nil {
			return axi.Failure("pr_issue_links_failed", "could not read the existing pull request body", true, err.Error())
		}
		var pull struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(output), &pull); err != nil {
			return axi.Failure("pr_issue_links_failed", "gh returned invalid pull request body data", true, err.Error())
		}
		body = pull.Body
	}
	updated, changed := appendMissingIssueLinks(body, brief, member)
	if !changed {
		return nil
	}
	if forge.Kind == "gitlab" {
		number, _ := forgeReference(prURL, forge)
		endpoint := "projects/" + url.PathEscape(forge.Path) + "/merge_requests/" + strconv.Itoa(number)
		_, err := runOutputStep(ctx, db, intent, "pr.issue_links.write", forge.Root, "glab", "api", "--hostname", forge.Host, "--method", "PUT", "--field", "description="+updated, endpoint)
		if err != nil {
			return axi.Failure("pr_issue_links_failed", "could not add linked issues to the merge request", true, err.Error())
		}
		return nil
	}
	if _, err := runOutputStep(ctx, db, intent, "pr.issue_links.write", forge.Root, "gh", "pr", "edit", prURL, "--body", updated); err != nil {
		return axi.Failure("pr_issue_links_failed", "could not add linked issues to the pull request", true, err.Error())
	}
	return nil
}

func issueNotFound(output string, err error) bool {
	message := strings.ToLower(output + " " + err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "could not resolve to an issue") || strings.Contains(message, "404")
}

func taskIssueReferences(home string, project store.Project, task store.Task) ([]string, []string, error) {
	brief, err := readTaskBrief(home, project, task)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	issues := make([]string, 0, len(brief.Issues))
	for _, ref := range brief.Issues {
		issues = append(issues, ref.String())
	}
	refs := make([]string, 0, len(brief.Refs))
	for _, ref := range brief.Refs {
		refs = append(refs, ref.String())
	}
	return issues, refs, nil
}

func readTaskBrief(home string, project store.Project, task store.Task) (dispatch.Brief, error) {
	path := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "brief.md")
	contents, err := os.ReadFile(path)
	if err != nil {
		return dispatch.Brief{}, err
	}
	brief, err := dispatch.ParseBriefText(string(contents))
	if err != nil {
		return dispatch.Brief{}, axi.Failure("brief_invalid", "could not parse the Task Brief", false, err.Error())
	}
	return brief, nil
}

func issueLinkLines(brief dispatch.Brief, member string) []string {
	lines := make([]string, 0, len(brief.Issues)+len(brief.Refs))
	for _, ref := range brief.Issues {
		if ref.Repository == member {
			lines = append(lines, "Closes #"+strconv.Itoa(ref.Number))
		}
	}
	for _, ref := range brief.Refs {
		if ref.Repository == member {
			lines = append(lines, "Refs #"+strconv.Itoa(ref.Number))
		}
	}
	return lines
}

func issueLinkBody(brief dispatch.Brief, member string) string {
	lines := issueLinkLines(brief, member)
	if len(lines) == 0 {
		return ""
	}
	return "## Linked issues\n\n" + strings.Join(lines, "\n") + "\n"
}

func appendMissingIssueLinks(body string, brief dispatch.Brief, member string) (string, bool) {
	lines := issueLinkLines(brief, member)
	if len(lines) == 0 {
		return body, false
	}
	existing := make(map[string]bool)
	for _, line := range strings.Split(body, "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	missing := make([]string, 0, len(lines))
	for _, line := range lines {
		if !existing[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		return body, false
	}
	section := "## Linked issues\n\n" + strings.Join(missing, "\n") + "\n"
	if strings.TrimSpace(body) == "" {
		return section, true
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section, true
}
