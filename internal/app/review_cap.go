package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func planUsesSharedReviewRepairCap(plan *taskplan.Plan) bool {
	return plan != nil && (plan.Review == "full-pipeline" || workspaceHasFullPipeline(plan))
}

func taskUsesSharedReviewRepairCap(task store.Task) bool {
	plan, err := task.AdmittedPlan()
	return err == nil && planUsesSharedReviewRepairCap(plan)
}

func reviewCapUsed(task store.Task, usage store.ReviewCapUsage) int {
	if taskUsesSharedReviewRepairCap(task) {
		return usage.Total()
	}
	return usage.ReviewRounds
}

func reviewCapHead(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	targets, err := changeCandidateTargets(ctx, db, project, task)
	if err != nil {
		return "", err
	}
	if len(targets) != 1 {
		return "", axi.Failure("review_cap_unsupported", "Review cap Decisions currently require one repository candidate", false)
	}
	return targets[0].Commit, nil
}

func (s *Service) reviewCapFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task, used, max int) error {
	head, err := reviewCapHead(ctx, db, project, task)
	if err != nil {
		return err
	}
	if err := createReviewCapNotice(ctx, db, project, task, head, used, max); err != nil {
		return err
	}
	return axi.Failure("review_cap_reached", fmt.Sprintf("Review and repair cap reached for t%d at %s (%d of %d Review/repair rounds used)", task.Seq, shortSHA(head), used, max), false,
		"Wait for the User's `review_cap` Decision and apply its `accept-head`, `final-fix`, `rethink` or `discard` choice")
}

func (s *Service) raiseReviewCapNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, head string, used, max int) error {
	return createReviewCapNotice(ctx, db, project, task, head, used, max)
}

func createReviewCapNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, head string, used, max int) error {
	if err := db.ObsoleteStaleReviewCapDecisions(ctx, task.ID, reviewCapOrigin(task, head)); err != nil {
		return err
	}
	data := marshalJSON(map[string]any{"head_sha": head, "rounds_used": used, "max_rounds": max})
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='review_cap_reached' AND json_valid(data_json) AND json_extract(data_json,'$.head_sha')=?)`, task.ID, head).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "review_cap_reached", Summary: fmt.Sprintf("%s: Review and repair cap reached at head %s (%d of %d Review/repair rounds)", task.Title, head, used, max), DataJSON: data})
	return err
}

func reviewCapOrigin(task store.Task, head string) string {
	return "review_cap:" + taskIDString(task.Seq) + ":" + head
}

func parseReviewCapOrigin(origin string) (string, string, bool) {
	parts := strings.Split(origin, ":")
	if len(parts) != 3 || parts[0] != "review_cap" || !strings.HasPrefix(parts[1], "t") || len(parts[2]) != 40 && len(parts[2]) != 64 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func currentReviewCapHead(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	return reviewCapHead(ctx, db, project, task)
}

func userDispositionBlockAt(task store.Task) string {
	plan, err := task.AdmittedPlan()
	if err == nil && plan != nil && plan.ValidationLoops != nil {
		return plan.ValidationLoops.Review.BlockAt
	}
	return "P1"
}

func reviewCapNoticeData(notice store.Notice) (string, int, int, error) {
	var data struct {
		Head string `json:"head_sha"`
		Used int    `json:"rounds_used"`
		Max  int    `json:"max_rounds"`
	}
	if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
		return "", 0, 0, err
	}
	if data.Head == "" || data.Used < 1 || data.Max < 1 {
		return "", 0, 0, fmt.Errorf("invalid Review cap Notice")
	}
	return data.Head, data.Used, data.Max, nil
}

func (s *Service) checkReviewCapAfterReviewDone(ctx context.Context, db *store.DB, project store.Project, reviewTask store.Task) error {
	if reviewTask.Type != "review" || reviewTask.ReviewsTaskID < 1 {
		return nil
	}
	verdict, err := db.ReviewVerdict(ctx, reviewTask.ID)
	if store.IsNotFound(err) || verdict.Verdict == store.ReviewPass {
		return nil
	}
	if err != nil {
		return err
	}
	change, err := db.TaskByID(ctx, project.ID, reviewTask.ReviewsTaskID)
	if err != nil {
		return err
	}
	plan, err := change.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if requireFrozenReviewLoop(plan) != nil {
		return nil
	}
	usage, err := db.ReviewCapUsage(ctx, change.ID)
	if err != nil {
		return err
	}
	max := plan.ValidationLoops.MaxReviewRounds()
	used := reviewCapUsed(change, usage)
	if used < max {
		return nil
	}
	head, err := reviewCapHead(ctx, db, project, change)
	if err != nil {
		return err
	}
	return createReviewCapNotice(ctx, db, project, change, head, used, max)
}

func (s *Service) checkReviewCapAfterDone(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	if task.Type != "change" {
		return nil
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if err := requireFrozenReviewLoop(plan); err != nil {
		return nil
	}
	if fullPipelineSelected(task, "") {
		candidate, err := db.LatestValidationCandidate(ctx, task.ID, "")
		if err != nil {
			return err
		}
		if candidate.State == store.ValidationCandidateActive {
			if err := s.ensureCandidateCheckout(ctx, db, home, task, candidate); err != nil {
				return err
			}
		}
	}
	targets, err := changeCandidateTargets(ctx, db, project, task)
	if err != nil {
		return err
	}
	if err := ensureReviewCarry(ctx, db, project, task, targets); err != nil {
		return err
	}
	max := plan.ValidationLoops.MaxReviewRounds()
	usage, err := db.ReviewCapUsage(ctx, task.ID)
	if err != nil {
		return err
	}
	used := reviewCapUsed(task, usage)
	if used < max {
		return nil
	}
	qualifies, err := reviewVerdictQualifies(ctx, db, task)
	if err != nil {
		return err
	}
	evidence, err := db.ReviewEvidenceFor(ctx, task.ID, targets, qualifies)
	if err != nil {
		return err
	}
	if evidence.Complete {
		return nil
	}
	if len(targets) != 1 {
		return axi.Failure("review_cap_unsupported", "Review cap Decisions currently require one repository candidate", false)
	}
	return createReviewCapNotice(ctx, db, project, task, targets[0].Commit, used, max)
}

func requireFrozenReviewLoop(plan *taskplan.Plan) error {
	if plan == nil || plan.ValidationLoops == nil || !plan.ValidationLoops.Review.Enabled {
		return axi.Failure("review_loop_disabled", "Change Task has no enabled, frozen Review Loop", false)
	}
	return nil
}
