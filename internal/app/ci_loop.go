package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func (s *Service) processCIObservation(ctx context.Context, db *store.DB, project store.Project, task store.Task, observation store.PRObservation, failures []failedCheck, forge repositoryForge, member string) error {
	if err := settleCIReruns(ctx, db, forge, task.ID, member, observation); err != nil {
		return err
	}
	if task.Type != "change" || observation.State != "OPEN" || len(failures) == 0 || observation.HeadSHA == "" || !ciTaskCanReceiveFix(task) {
		return nil
	}
	if member == "" && (task.MergeMode != "pr" || task.GatedSHA != observation.HeadSHA) {
		return nil
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return err
	}
	if plan == nil || plan.ValidationLoops == nil || !ciPlanAllowsMember(plan, member) {
		return nil
	}
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	if current.Type != "change" || !ciTaskCanReceiveFix(current) {
		return nil
	}
	prURL := observation.PRURL
	if member == "" {
		if current.MergeMode != "pr" || current.PRURL != prURL || current.GatedSHA != observation.HeadSHA {
			return nil
		}
	} else {
		var currentPR, gatedSHA, mergeMode string
		if err := db.QueryRowContext(ctx, `SELECT pr_url,gated_sha,landing_mode FROM task_repos WHERE task_id=? AND repo=?`, task.ID, member).Scan(&currentPR, &gatedSHA, &mergeMode); err != nil {
			if store.IsNotFound(err) {
				return nil
			}
			return err
		}
		if mergeMode != "pr" || currentPR != prURL || gatedSHA != observation.HeadSHA {
			return nil
		}
	}
	task = current
	checks := failures
	if forge.Kind == "gitlab" {
		checks, err = expandGitLabFailureChecks(ctx, forge, checks)
		if err != nil {
			return err
		}
	}
	if len(checks) == 0 {
		return nil
	}

	fixChecks := make([]failedCheck, 0, len(checks))
	cancelledChecks := make([]failedCheck, 0, len(checks))
	for _, check := range checks {
		if check.Cancelled {
			cancelledChecks = append(cancelledChecks, check)
		} else {
			fixChecks = append(fixChecks, check)
		}
	}
	rerunFailures := map[string]string{}
	groups, unrerunnable := groupCIReruns(forge.Kind, cancelledChecks)
	fixChecks = append(fixChecks, unrerunnable...)
	for _, group := range groups {
		reservationChecks := make([]store.CIRerunCheck, 0, len(group.checks))
		for _, check := range group.checks {
			reservationChecks = append(reservationChecks, store.CIRerunCheck{Name: check.Name, RunID: check.RunID, JobID: check.JobID, URL: check.URL})
		}
		reserved, err := db.ReserveCIRerunRequests(ctx, project.ID, task.ID, member, prURL, observation.HeadSHA, forge.Kind, reservationChecks, plan.ValidationLoops.CI.RerunCancelled)
		if err != nil {
			return err
		}
		if !reserved {
			fixChecks = append(fixChecks, group.checks...)
			continue
		}
		requestErr := requestCIRerun(ctx, forge.Root, prURL, forge, group)
		for _, check := range group.checks {
			if err := recordCIRerun(ctx, db, store.CIRerun{ProjectID: project.ID, TaskID: task.ID, Repo: member, PRURL: prURL, HeadSHA: observation.HeadSHA, CheckName: check.Name, RunID: check.RunID, JobID: check.JobID, Reason: "cancelled"}, requestErr); err != nil {
				return err
			}
			if requestErr != nil {
				fixChecks = append(fixChecks, check)
				rerunFailures[check.Name] = requestErr.Error()
			}
		}
	}
	if len(fixChecks) == 0 {
		return nil
	}
	logs := make(map[string]string, len(fixChecks))
	tails := make(map[string]string, len(fixChecks))
	for _, check := range fixChecks {
		log, err := failedCILog(ctx, forge.Root, forge, prURL, check)
		logs[check.Name] = log
		tail := log
		if err != nil {
			tail = strings.TrimSpace(tail + "\nCould not fetch the failed-log tail: " + err.Error())
		}
		if rerunFailure := rerunFailures[check.Name]; rerunFailure != "" {
			tail = strings.TrimSpace(tail + "\nRerun command failed: " + rerunFailure)
		}
		tails[check.Name] = boundedLogTail(tail, 200)
	}
	fixChecks, err = s.rerunKnownFlakes(ctx, db, project, task, plan, observation, forge, member, fixChecks, logs, tails)
	if err != nil || len(fixChecks) == 0 {
		return err
	}
	return s.queueCIFixRound(ctx, db, project, task, plan, observation, forge, prURL, member, fixChecks, tails)
}

func ciTaskCanReceiveFix(task store.Task) bool {
	return task.State == store.StateDone || task.State == store.StateMerging
}

func ciPlanAllowsMember(plan *taskplan.Plan, member string) bool {
	if member == "" {
		return plan.Delivery == "pr"
	}
	for _, planned := range plan.Execution.Members {
		if planned.Name == member {
			return planned.Delivery == "pr"
		}
	}
	return false
}

type ciRerunGroup struct {
	runID  string
	jobID  string
	checks []failedCheck
}

func groupCIReruns(provider string, checks []failedCheck) ([]ciRerunGroup, []failedCheck) {
	groups := make([]ciRerunGroup, 0)
	indexes := map[string]int{}
	unrerunnable := make([]failedCheck, 0)
	for _, check := range checks {
		if strings.TrimSpace(check.Name) == "" {
			check.Name = "required check rollup"
		}
		if check.RunID == "" || (provider == "gitlab" && check.JobID == "") {
			unrerunnable = append(unrerunnable, check)
			continue
		}
		key := check.RunID
		if provider == "gitlab" {
			key = check.JobID
		}
		index, found := indexes[key]
		if !found {
			index = len(groups)
			indexes[key] = index
			groups = append(groups, ciRerunGroup{runID: check.RunID})
		}
		groups[index].checks = append(groups[index].checks, check)
	}
	return groups, unrerunnable
}

func requestCIRerun(ctx context.Context, root, prURL string, forge repositoryForge, group ciRerunGroup) error {
	if forge.Kind == "gitlab" {
		if len(group.checks) != 1 || group.checks[0].JobID == "" {
			return fmt.Errorf("GitLab cancelled check has no retryable job ID")
		}
		output, err := commandOutputArgs(ctx, root, "glab", "ci", "retry", group.checks[0].JobID, "--repo", "https://"+forge.Host+"/"+forge.Path)
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
		}
		return nil
	}
	ref, err := pullRequestReferenceFromURL(prURL)
	if err != nil {
		return err
	}
	args := []string{"run", "rerun", "--failed", group.runID}
	if group.jobID != "" {
		args = []string{"run", "rerun", "--job", group.jobID}
	}
	output, err := commandOutputArgs(ctx, root, "gh", append(args, "--repo", githubRepositorySelector(ref))...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func failedCILog(ctx context.Context, root string, forge repositoryForge, prURL string, check failedCheck) (string, error) {
	if forge.Kind == "gitlab" {
		if check.JobID == "" {
			return "", fmt.Errorf("GitLab failed check has no job ID")
		}
		return gitlabAPI(ctx, forge, "jobs/"+check.JobID+"/trace")
	}
	ref, err := pullRequestReferenceFromURL(prURL)
	if err != nil {
		return "", err
	}
	args := []string{"run", "view"}
	if check.JobID != "" {
		args = append(args, "--job", check.JobID)
	} else if check.RunID != "" {
		args = append(args, check.RunID)
	} else {
		return "", fmt.Errorf("GitHub failed check has no run or job ID")
	}
	args = append(args, "--log-failed", "--repo", githubRepositorySelector(ref))
	return commandOutputArgs(ctx, root, "gh", args...)
}

func (s *Service) queueCIFixRound(ctx context.Context, db *store.DB, project store.Project, task store.Task, plan *taskplan.Plan, observation store.PRObservation, forge repositoryForge, prURL, member string, checks []failedCheck, tails map[string]string) error {
	names := uniqueFailedCheckNames(checks)
	if len(names) == 0 {
		return nil
	}
	checkSet := member + "\x00" + strings.Join(names, "\x00")
	digest := sha256.Sum256([]byte(checkSet))
	checkSetKey := hex.EncodeToString(digest[:])
	dedupeKey := "ci:" + observation.HeadSHA + ":" + checkSetKey
	instruction := renderCIFixInstruction(task, plan, observation, forge, prURL, member, checks, tails)
	result, err := db.QueueCIFixInstruction(ctx, task.ID, observation.HeadSHA, prURL, dedupeKey, instruction, member, plan.ValidationLoops.CI.MaxFixRounds)
	if errors.Is(err, store.ErrStaleValidationHead) || errors.Is(err, store.ErrStateRace) {
		return nil
	}
	memberLabel := ""
	if member != "" {
		memberLabel = member + ": "
	}
	exhausted := func(reason string) error {
		data := marshalJSON(map[string]any{"url": prURL, "head_sha": observation.HeadSHA, "check_set": checkSetKey, "checks": names, "repository": member})
		_, noticeErr := db.CreateValidationNoticeOnce(ctx, store.Notice{
			ProjectID: project.ID, TaskID: task.ID, Kind: "ci_fix_exhausted",
			Summary:  task.Title + ": " + memberLabel + reason + " for gated head " + observation.HeadSHA + ": " + strings.Join(names, ", "),
			DataJSON: data,
		}, prURL, observation.HeadSHA, checkSetKey, member)
		return noticeErr
	}
	if errors.Is(err, store.ErrValidationFixBudgetExhausted) {
		return exhausted("CI fix-round budget exhausted")
	}
	if err != nil {
		return err
	}
	if result.Queued {
		data := marshalJSON(map[string]any{"url": prURL, "head_sha": observation.HeadSHA, "check_set": checkSetKey, "checks": names, "repository": member})
		_, err = db.CreateValidationNoticeOnce(ctx, store.Notice{
			ProjectID: project.ID, TaskID: task.ID, Kind: "ci_fix_queued",
			Summary:  task.Title + ": " + memberLabel + "queued a CI fix round for gated head " + observation.HeadSHA + ": " + strings.Join(names, ", "),
			DataJSON: data,
		}, prURL, observation.HeadSHA, checkSetKey, member)
	}
	return err
}

func renderCIFixInstruction(task store.Task, plan *taskplan.Plan, observation store.PRObservation, forge repositoryForge, prURL, member string, checks []failedCheck, tails map[string]string) string {
	var body strings.Builder
	memberContext := ""
	if member != "" {
		memberContext = " for workspace member " + member
	}
	if isFullPipelineTask(task, member) {
		fmt.Fprintf(&body, "Required CI checks failed for PR head %s%s, which matches the gated head. Fix each failure on the author Task branch, commit, signal done, then submit a private candidate and complete Review -> test -> docs -> lint before promotion. Do not push the fix directly.\n\n", observation.HeadSHA, memberContext)
	} else {
		fmt.Fprintf(&body, "Required CI checks failed for PR head %s%s, which matches the gated head. Fix each failure, commit the changes, publish the updated head, and signal done again.\n\n", observation.HeadSHA, memberContext)
	}
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			name = "unnamed required check"
		}
		fmt.Fprintf(&body, "- Check: %s\n%s  Log URL: %s\n  Failed-log tail (up to 200 lines):\n```text\n%s\n```\n", name, ciCheckIdentity(forge, prURL, check), defaultValue(check.URL, "not supplied"), strings.TrimSpace(tails[name]))
	}
	if label, text := briefFixerSection(plan); text != "" {
		fmt.Fprintf(&body, "\n%s:\n%s\n", label, text)
	}
	if isFullPipelineTask(task, member) {
		if member != "" {
			fmt.Fprintf(&body, "\nThis PR is %s for workspace member %s. Submit the committed author head with `posse publish --repo %s \"<summary>\"` from the Task Sandbox. Then review and validate the private candidate, and run `posse publish --repo %s` from that candidate checkout to promote it. Preserve the member's frozen target branch.", prURL, member, member, member)
		} else {
			fmt.Fprintf(&body, "\nThis PR is %s. Submit the committed author head with `posse publish \"<summary>\"` from the Task Sandbox. Then review and validate the private candidate, and run `posse publish` from that candidate checkout to promote it. Preserve the Task's frozen target branch.", prURL)
		}
	} else if member != "" {
		fmt.Fprintf(&body, "\nThis PR is %s for workspace member %s. Publish the updated head with `posse publish --repo %s \"<summary>\"` and report done without `--pr`. Preserve the member's frozen target branch.", prURL, member, member)
	} else {
		fmt.Fprintf(&body, "\nThis PR is %s. Publish the updated head with `posse publish \"<summary>\"` and report done with the PR URL. Preserve the Task's frozen target branch.", prURL)
	}
	return strings.TrimSpace(body.String())
}

func ciCheckIdentity(forge repositoryForge, prURL string, check failedCheck) string {
	var out strings.Builder
	if check.RunID != "" {
		fmt.Fprintf(&out, "  Run: %s\n", check.RunID)
	}
	if check.JobID != "" {
		fmt.Fprintf(&out, "  Job: %s\n", check.JobID)
	}
	if command := ciLogCommand(forge, prURL, check); command != "" {
		fmt.Fprintf(&out, "  Log command: `%s`\n", command)
	}
	return out.String()
}

func ciLogCommand(forge repositoryForge, prURL string, check failedCheck) string {
	if forge.Kind == "gitlab" {
		if check.JobID == "" {
			return ""
		}
		return "glab ci trace " + check.JobID
	}
	ref, err := pullRequestReferenceFromURL(prURL)
	if err != nil {
		return ""
	}
	repo := githubRepositorySelector(ref)
	switch {
	case check.JobID != "":
		return "gh run view --job " + check.JobID + " --log-failed --repo " + repo
	case check.RunID != "":
		return "gh run view " + check.RunID + " --log-failed --repo " + repo
	}
	return ""
}

func briefFixerSection(plan *taskplan.Plan) (label, text string) {
	if plan == nil {
		return "", ""
	}
	for _, input := range plan.Inputs {
		if input.Role != "brief" {
			continue
		}
		brief, err := dispatch.ParseHistoricalBriefText(string(input.Bytes))
		if err != nil {
			continue
		}
		if intent := briefSection(brief.Body, "Intent"); intent != "" {
			return "Intent (User words verbatim; Goal and Lead assumptions are Lead text)", intent
		}
		return "Change Brief (verbatim)", strings.TrimSpace(brief.Body)
	}
	return "", ""
}

func uniqueFailedCheckNames(checks []failedCheck) []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			name = "required check rollup"
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func boundedLogTail(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || limit <= 0 {
		return value
	}
	lines := strings.Split(value, "\n")
	if len(lines) <= limit {
		return value
	}
	return strings.Join(lines[len(lines)-limit:], "\n")
}

func githubRepositorySelector(ref pullRequestReference) string {
	selector := ref.Owner + "/" + ref.Repo
	if !strings.EqualFold(ref.Host, "github.com") {
		selector = ref.Host + "/" + selector
	}
	return selector
}

func githubActionsRunID(detailsURL string) string {
	parsed, err := url.Parse(detailsURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+2 < len(parts); index++ {
		if parts[index] == "actions" && parts[index+1] == "runs" {
			if id, err := strconv.ParseInt(parts[index+2], 10, 64); err == nil && id > 0 {
				return strconv.FormatInt(id, 10)
			}
		}
	}
	return ""
}

func gitlabPipelineID(pipelineURL string) string {
	parsed, err := url.Parse(pipelineURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] == "pipelines" {
			if id, err := strconv.ParseInt(parts[index+1], 10, 64); err == nil && id > 0 {
				return strconv.FormatInt(id, 10)
			}
		}
	}
	return ""
}

func forgeIDString(value any) string {
	switch id := value.(type) {
	case string:
		return strings.TrimSpace(id)
	case float64:
		if id > 0 && id == float64(int64(id)) {
			return strconv.FormatInt(int64(id), 10)
		}
	case json.Number:
		parsed, err := id.Int64()
		if err == nil && parsed > 0 {
			return strconv.FormatInt(parsed, 10)
		}
	case int:
		if id > 0 {
			return strconv.Itoa(id)
		}
	case int64:
		if id > 0 {
			return strconv.FormatInt(id, 10)
		}
	}
	return ""
}

type gitlabPipelineJob struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
}

func expandGitLabFailureChecks(ctx context.Context, forge repositoryForge, checks []failedCheck) ([]failedCheck, error) {
	result := make([]failedCheck, 0, len(checks))
	for _, check := range checks {
		if check.Name != "pipeline" || check.RunID == "" || check.JobID != "" {
			result = append(result, check)
			continue
		}
		output, err := gitlabAPI(ctx, forge, "pipelines/"+check.RunID+"/jobs?per_page=100")
		if err != nil {
			result = append(result, check)
			continue
		}
		var jobs []gitlabPipelineJob
		if err := json.Unmarshal([]byte(output), &jobs); err != nil {
			result = append(result, check)
			continue
		}
		added := 0
		for _, job := range jobs {
			status := strings.ToLower(job.Status)
			if job.ID < 1 || !oneOfString(status, "failed", "canceled", "cancelled") {
				continue
			}
			name := strings.TrimSpace(job.Name)
			if name == "" {
				name = "job " + strconv.FormatInt(job.ID, 10)
			}
			result = append(result, failedCheck{Name: name, URL: defaultValue(job.WebURL, check.URL), Cancelled: oneOfString(status, "canceled", "cancelled"), RunID: check.RunID, JobID: strconv.FormatInt(job.ID, 10)})
			added++
		}
		if added == 0 {
			result = append(result, check)
		}
	}
	return result, nil
}
