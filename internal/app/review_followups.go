package app

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
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
	areas, err := reviewFollowupAreas(ctx.Context, db, project)
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(areas))
	for _, area := range areas {
		if len(area.Findings) > 0 {
			rows = append(rows, area)
		}
	}
	return ctx.Print(axi.Object{{Key: "areas", Value: rows}, {Key: "help", Value: []any{
		"File or reuse one open follow-up issue per area, list the area's findings in it, then run `posse review followups defer --area <area> [--repo <repository>] --issue <url>`",
	}}})
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
	areas, err := reviewFollowupAreas(ctx.Context, db, project)
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
			if err := db.ApplyReviewDisposition(ctx.Context, id, seqs, nil, "defer", "", issue, "", "", actor, userDispositionBlockAt(area.tasks[id]), false); err != nil {
				return axi.Failure("review_disposition_refused", err.Error(), false)
			}
			deferred += len(seqs)
		}
	}
	return ctx.Print(axi.Object{{Key: "area", Value: areaName}, {Key: "issue", Value: issue}, {Key: "deferred", Value: deferred}})
}

func reviewFollowupAreas(ctx context.Context, db *store.DB, project store.Project) ([]reviewFollowupArea, error) {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return nil, err
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
		for _, finding := range findings {
			key := [2]string{finding.Repository, path.Dir(finding.Path)}
			area := areas[key]
			if area == nil {
				area = &reviewFollowupArea{Repository: key[0], Area: key[1], selected: map[int64][]int{}, tasks: map[int64]store.Task{}}
				areas[key] = area
			}
			if finding.FollowUpURL != "" {
				area.LastIssue = finding.FollowUpURL
				continue
			}
			if finding.InheritedFrom > 0 || !oneOfString(finding.State, "open", "deferred") || reviewFindingBlocks(finding, plan) {
				continue
			}
			area.Findings = append(area.Findings, map[string]any{
				"task": taskIDString(task.Seq), "id": "F-" + strconv.Itoa(finding.FindingSeq), "severity": finding.Severity,
				"state": finding.State, "path": finding.Path, "symbol": finding.Symbol, "title": finding.Title,
			})
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
