package app

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

type reviewFollowupArea struct {
	Repository string           `json:"repository"`
	Area       string           `json:"area"`
	LastIssue  string           `json:"last_issue,omitempty"`
	Findings   []map[string]any `json:"findings"`
	selected   map[int64][]int
	tasks      map[int64]store.Task
}

func (s *Service) reviewFollowups(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review followups", args, map[string]flagSpec{})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("review followups takes no positional arguments")
	}
	db, home, workerReadOnly, err := s.openInspectionDB(ctx.Context)
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	var assigned []dispatch.Followup
	if workerReadOnly {
		worker, err := workerIdentityTask(ctx.Context, db, home)
		if err != nil || worker.ProjectID != project.ID || worker.Type != "research" {
			return workerFollowupsRefusal()
		}
		plan, err := worker.AdmittedPlan()
		if err != nil || plan == nil || len(plan.Inputs) == 0 {
			return workerFollowupsRefusal()
		}
		brief, err := dispatch.ParseStoredBriefText(string(plan.Inputs[0].Bytes))
		if err != nil || len(brief.Followups) == 0 {
			return workerFollowupsRefusal()
		}
		assigned = brief.Followups
	} else if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	areas, err := reviewFollowupAreas(ctx.Context, db, project, assigned)
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(areas))
	for _, area := range areas {
		if len(area.Findings) > 0 {
			rows = append(rows, area)
		}
	}
	output := axi.Object{{Key: "areas", Value: rows}}
	if assigned == nil {
		output = append(output, axi.Field{Key: "help", Value: []any{
			"Close a verified finding with `posse review followups resolve <change> <F-n> <fixed|obsolete> --reason <evidence>`",
			"File or reuse one open follow-up issue per area, list the area's findings in it, then run `posse review followups defer --area <area> [--repo <repository>] --issue <url>`",
		}})
	}
	return ctx.Print(output)
}

func workerFollowupsRefusal() error {
	return axi.Failure("worker_forbidden", "Workers may list only follow-up findings assigned in their admitted Brief for this Project", false)
}

func (s *Service) reviewFollowupsResolve(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review followups resolve", args, map[string]flagSpec{"reason": {}})
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(parsed.Flags["reason"])
	if len(parsed.Positionals) != 3 || reason == "" {
		return axi.Usage("review followups resolve requires <change> <F-n> <fixed|obsolete> --reason <evidence>")
	}
	seq, ok := parseFindingID(parsed.Positionals[1])
	outcome := parsed.Positionals[2]
	if !ok || !oneOfString(outcome, "fixed", "obsolete") {
		return axi.Usage("review followups resolve requires one F-n finding and the outcome fixed or obsolete")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "change" {
		return axi.Failure("review_task_invalid", "follow-up resolution requires a Change Task", false)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	blockAt := "P1"
	if plan != nil && plan.ValidationLoops != nil {
		blockAt = plan.ValidationLoops.Review.BlockAt
	}
	actor := defaultValue(s.configCallerRole(ctx.Context, db), "user")
	if err := db.ApplyReviewDisposition(ctx.Context, task.ID, []int{seq}, nil, "followup_"+outcome, "", "", reason, "", actor, blockAt, false); err != nil {
		return axi.Failure("review_disposition_refused", err.Error(), false)
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "finding", Value: parsed.Positionals[1]}, {Key: "resolution", Value: outcome}, {Key: "reason", Value: reason}})
}

func (s *Service) reviewFollowupsDefer(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("review followups defer", args, map[string]flagSpec{"area": {}, "repo": {}, "issue": {}})
	if err != nil {
		return err
	}
	areaName, repository, issue := strings.TrimSpace(parsed.Flags["area"]), strings.TrimSpace(parsed.Flags["repo"]), strings.TrimSpace(parsed.Flags["issue"])
	if len(parsed.Positionals) != 0 || areaName == "" || issue == "" {
		return axi.Usage("review followups defer requires --area <area> and --issue <url>")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	actor := s.configCallerRole(ctx.Context, db)
	if actor == "" {
		actor = "user"
	}
	areas, err := reviewFollowupAreas(ctx.Context, db, project, nil)
	if err != nil {
		return err
	}
	var matched []reviewFollowupArea
	for _, area := range areas {
		if area.Area == areaName && (repository == "" || area.Repository == repository) && len(area.Findings) > 0 {
			matched = append(matched, area)
		}
	}
	if len(matched) > 1 {
		return axi.Usage("area " + areaName + " exists in more than one repository; add --repo <repository>")
	}
	deferred := 0
	for _, area := range matched {
		taskIDs := make([]int64, 0, len(area.selected))
		for id := range area.selected {
			taskIDs = append(taskIDs, id)
		}
		sort.Slice(taskIDs, func(i, j int) bool { return taskIDs[i] < taskIDs[j] })
		for _, id := range taskIDs {
			seqs := area.selected[id]
			if err := db.ApplyReviewDisposition(ctx.Context, id, seqs, nil, "defer_followup", "", issue, "", "", actor, userDispositionBlockAt(area.tasks[id]), false); err != nil {
				return axi.Failure("review_disposition_refused", err.Error(), false)
			}
			deferred += len(seqs)
		}
	}
	return ctx.Print(axi.Object{{Key: "area", Value: areaName}, {Key: "issue", Value: issue}, {Key: "deferred", Value: deferred}})
}

func reviewFollowupAreas(ctx context.Context, db *store.DB, project store.Project, assigned []dispatch.Followup) ([]reviewFollowupArea, error) {
	var tasks []store.Task
	selected := map[string]map[int]bool{}
	if assigned == nil {
		var err error
		tasks, err = db.Tasks(ctx, project.ID, true)
		if err != nil {
			return nil, err
		}
	} else {
		for _, ref := range assigned {
			if selected[ref.TaskID] == nil {
				task, err := db.Task(ctx, project.ID, ref.TaskID)
				if store.IsNotFound(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				tasks = append(tasks, task)
				selected[ref.TaskID] = map[int]bool{}
			}
			selected[ref.TaskID][ref.Finding] = true
		}
	}
	areas := map[[2]string]*reviewFollowupArea{}
	for _, task := range tasks {
		if task.Type != "change" {
			continue
		}
		plan, err := task.AdmittedPlan()
		if err != nil {
			return nil, admittedPlanFailure(err)
		}
		findings, err := db.ReviewFindingsForTask(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		taskID := taskIDString(task.Seq)
		for _, finding := range findings {
			if assigned != nil && !selected[taskID][finding.FindingSeq] {
				continue
			}
			key := [2]string{finding.Repository, path.Dir(finding.Path)}
			area := areas[key]
			if area == nil {
				area = &reviewFollowupArea{Repository: key[0], Area: key[1], selected: map[int64][]int{}, tasks: map[int64]store.Task{}}
				areas[key] = area
			}
			if assigned == nil && finding.FollowUpURL != "" {
				area.LastIssue = finding.FollowUpURL
				continue
			}
			if finding.InheritedFrom > 0 || !oneOfString(finding.State, "open", "deferred") {
				continue
			}
			blockCheck := finding
			blockCheck.State = "open"
			if reviewFindingBlocks(blockCheck, plan) {
				continue
			}
			row := map[string]any{
				"task": taskID, "id": "F-" + strconv.Itoa(finding.FindingSeq), "severity": finding.Severity,
				"state": finding.State, "path": finding.Path, "symbol": finding.Symbol, "title": finding.Title,
			}
			if assigned != nil {
				row["excerpt"] = finding.Excerpt
				row["rule"] = finding.Rule
				row["root_cause"] = finding.RootCause
				row["fix_guidance"] = finding.FixGuidance
				if finding.FollowUpURL != "" {
					row["follow_up_url"] = finding.FollowUpURL
				}
			}
			area.Findings = append(area.Findings, row)
			area.selected[task.ID] = append(area.selected[task.ID], finding.FindingSeq)
			area.tasks[task.ID] = task
		}
	}
	keys := make([][2]string, 0, len(areas))
	for key := range areas {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
	})
	result := make([]reviewFollowupArea, 0, len(keys))
	for _, key := range keys {
		result = append(result, *areas[key])
	}
	return result, nil
}
