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
	}{{"ticket", issueReferences(brief)}, {"refs", brief.Refs}} {
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

func validateChangeIssueTargets(brief dispatch.Brief, taskTargets []repoTarget) error {
	if brief.Type != "change" {
		return nil
	}
	members := make(map[string]bool, len(taskTargets))
	for _, target := range taskTargets {
		members[target.Name] = true
	}
	for _, list := range []struct {
		field string
		refs  []dispatch.IssueRef
	}{{"ticket", ticketReferences(brief)}, {"refs", brief.Refs}} {
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
	}{{issueReferences(brief)}, {brief.Refs}} {
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

type githubIssueBlocker struct {
	Number        int    `json:"number"`
	State         string `json:"state"`
	RepositoryURL string `json:"repository_url"`
	HTMLURL       string `json:"html_url"`
}

func githubIssueBlockers(ctx context.Context, forge repositoryForge, number int) ([]githubIssueBlocker, error) {
	if forge.Kind != "github" || number < 1 {
		return nil, fmt.Errorf("native issue blockers require a GitHub repository and valid issue number")
	}
	endpoint := fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by?per_page=100", strings.Trim(forge.Path, "/"), number)
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "api", "--hostname", forge.Host, "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("read GitHub blocked_by relationships: %w: %s", err, strings.TrimSpace(output))
	}
	var pages []json.RawMessage
	if err := json.Unmarshal([]byte(output), &pages); err != nil || pages == nil {
		return nil, fmt.Errorf("GitHub blocked_by response did not contain complete paginated JSON")
	}
	var blockers []githubIssueBlocker
	for _, page := range pages {
		var rows []githubIssueBlocker
		if err := json.Unmarshal(page, &rows); err != nil {
			return nil, fmt.Errorf("GitHub blocked_by page is incomplete or invalid")
		}
		for _, blocker := range rows {
			if blocker.Number < 1 || blocker.State == "" || !sameGitHubRepositoryIdentity(blocker.RepositoryURL, forge) {
				return nil, fmt.Errorf("GitHub blocked_by response contains an incomplete or non-canonical issue identity")
			}
			blockers = append(blockers, blocker)
		}
	}
	return blockers, nil
}

func sameGitHubRepositoryIdentity(repositoryURL string, forge repositoryForge) bool {
	parsed, err := url.Parse(repositoryURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return false
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	marker := "/repos/"
	if index := strings.LastIndex("/"+path, marker); index >= 0 {
		path = ("/" + path)[index+len(marker):]
	}
	hostMatches := strings.EqualFold(parsed.Host, forge.Host) || strings.EqualFold(parsed.Host, "api."+forge.Host)
	return hostMatches && strings.EqualFold(path, forge.Path)
}

func validateTicketBlockers(ctx context.Context, cfg config.Config, brief dispatch.Brief, targets []repoTarget) error {
	if brief.Type != "change" || len(issueReferences(brief)) == 0 {
		return nil
	}
	ref := issueReferences(brief)[0]
	repository := ref.Repository
	if repository == "" && len(targets) == 1 {
		repository = targets[0].Name
	}
	var target *repoTarget
	for i := range targets {
		if targets[i].Name == repository {
			if target != nil {
				return axi.Failure("ticket_repository_ambiguous", "ticket repository identity is ambiguous", false)
			}
			target = &targets[i]
		}
	}
	if target == nil {
		return axi.Failure("ticket_repository_unavailable", "ticket repository is unavailable for blocker checks", false)
	}
	// A ticket link alone does not imply a native GitHub relationship. Only
	// query blocked_by after resolving a canonical remote repository identity.
	// Local repositories without an origin retain their normal local flow.
	if originHost(ctx, target.Root) == "" {
		return nil
	}
	forge, err := forgeForRepository(ctx, target.Root, cfg, target.Name)
	if err != nil {
		return axi.Failure("ticket_blockers_unavailable", "cannot verify native GitHub blockers: "+truncate(err.Error(), 180), true, "Restore GitHub access and retry; dispatch is held until blockers can be verified")
	}
	if forge.Kind != "github" {
		return nil
	}
	blockers, err := githubIssueBlockers(ctx, forge, ref.Number)
	if err != nil {
		return axi.Failure("ticket_blockers_unavailable", "cannot verify native GitHub blockers: "+truncate(err.Error(), 180), true, "Restore GitHub access and retry; dispatch is held until blockers can be verified")
	}
	for _, blocker := range blockers {
		switch strings.ToUpper(blocker.State) {
		case "CLOSED":
			continue
		case "OPEN":
			identity := fmt.Sprintf("#%d", blocker.Number)
			if blocker.HTMLURL != "" {
				identity = blocker.HTMLURL
			}
			return axi.Failure("ticket_blocked", "Change Task waits for open native blocker "+identity, false, "Wait for the blocker to close, then retry `posse start`")
		default:
			return axi.Failure("ticket_blockers_unavailable", "GitHub returned an unknown blocker state", true, "Retry after GitHub returns complete issue states")
		}
	}
	return nil
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
	closingIssues := brief.Issues
	if brief.Ticket != nil {
		closingIssues = []dispatch.IssueRef{*brief.Ticket}
	}
	issues := make([]string, 0, len(closingIssues))
	for _, ref := range closingIssues {
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
	plan, err := task.AdmittedPlan()
	if err != nil {
		return dispatch.Brief{}, admittedPlanFailure(err)
	}
	var contents []byte
	if plan != nil {
		contents = plan.Inputs[0].Bytes
	} else {
		contents, err = os.ReadFile(path)
		if err != nil {
			return dispatch.Brief{}, err
		}
	}
	brief, err := dispatch.ParseHistoricalBriefText(string(contents))
	if err != nil {
		return dispatch.Brief{}, axi.Failure("brief_invalid", "could not parse the Task Brief", false, err.Error())
	}
	return brief, nil
}

func issueReferences(brief dispatch.Brief) []dispatch.IssueRef {
	if brief.Ticket != nil {
		return []dispatch.IssueRef{*brief.Ticket}
	}
	return brief.Issues
}

func ticketReferences(brief dispatch.Brief) []dispatch.IssueRef {
	if brief.Ticket == nil {
		return nil
	}
	return []dispatch.IssueRef{*brief.Ticket}
}

func issueLinkLines(brief dispatch.Brief, member string) []string {
	closingIssues := issueReferences(brief)
	lines := make([]string, 0, len(closingIssues)+len(brief.Refs))
	for _, ref := range closingIssues {
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
	return "## Issue Link\n\n" + strings.Join(lines, "\n") + "\n"
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
	section := "## Issue Link\n\n" + strings.Join(missing, "\n") + "\n"
	if strings.TrimSpace(body) == "" {
		return section, true
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section, true
}
