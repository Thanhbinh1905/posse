package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
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

// reviewCapRule says which rounds count toward max, so the budget is clear
// before the first Review Round.
func reviewCapRule(task store.Task, max int) string {
	if max < 1 {
		return "no Review Loop"
	}
	if !taskUsesSharedReviewRepairCap(task) {
		return fmt.Sprintf("only Review Rounds count toward max_rounds; %d allows %d Review Rounds", max, max)
	}
	return fmt.Sprintf("Review fix rounds share max_rounds with Review Rounds, CI fix rounds do not; %d allows at most %d Review Rounds with a Review fix round between each", max, (max+1)/2)
}

// reviewCapBinding is what a cap Decision binds: the one candidate head of a
// single repository, or a digest of every changed workspace Member's head.
type reviewCapBinding struct {
	Head    string
	Targets []store.ReviewTarget
}

func newReviewCapBinding(targets []store.ReviewTarget) (reviewCapBinding, error) {
	if len(targets) == 0 {
		return reviewCapBinding{}, axi.Failure("review_cap_unsupported", "Review cap Decisions require at least one changed candidate", false)
	}
	targets = slices.Clone(targets)
	slices.SortFunc(targets, func(a, b store.ReviewTarget) int { return strings.Compare(a.Repository, b.Repository) })
	if len(targets) == 1 {
		return reviewCapBinding{Head: targets[0].Commit, Targets: targets}, nil
	}
	hash := sha256.New()
	for _, target := range targets {
		fmt.Fprintf(hash, "%s\x00%s\n", target.Repository, target.Commit)
	}
	return reviewCapBinding{Head: hex.EncodeToString(hash.Sum(nil)), Targets: targets}, nil
}

func (b reviewCapBinding) label() string {
	if len(b.Targets) < 2 {
		return shortSHA(b.Head)
	}
	heads := make([]string, len(b.Targets))
	for i, target := range b.Targets {
		heads[i] = target.Repository + "@" + shortSHA(target.Commit)
	}
	return strings.Join(heads, ", ")
}

func reviewCapHead(ctx context.Context, db *store.DB, project store.Project, task store.Task) (reviewCapBinding, error) {
	targets, err := changeCandidateTargets(ctx, db, project, task)
	if err != nil {
		return reviewCapBinding{}, err
	}
	return newReviewCapBinding(targets)
}

func (s *Service) reviewCapFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task, used, max int) error {
	binding, err := reviewCapHead(ctx, db, project, task)
	if err != nil {
		return err
	}
	return reviewCapExhausted(ctx, db, project, task, binding, used, max)
}

// reviewCapExhausted raises the cap Notice for binding and refuses the caller.
func reviewCapExhausted(ctx context.Context, db *store.DB, project store.Project, task store.Task, binding reviewCapBinding, used, max int) error {
	if err := createReviewCapNotice(ctx, db, project, task, binding, used, max); err != nil {
		return err
	}
	return axi.Failure("review_cap_reached", fmt.Sprintf("Review and repair cap reached for t%d at %s (%d of %d Review/repair rounds used)", task.Seq, binding.label(), used, max), false,
		"Wait for the User's `review_cap` Decision and apply its `accept-head`, `final-fix`, `rethink` or `discard` choice")
}

func (s *Service) raiseReviewCapNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, binding reviewCapBinding, used, max int) error {
	return createReviewCapNotice(ctx, db, project, task, binding, used, max)
}

func createReviewCapNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, binding reviewCapBinding, used, max int) error {
	head := binding.Head
	if err := db.ObsoleteStaleReviewCapDecisions(ctx, task.ID, reviewCapOrigin(task, head)); err != nil {
		return err
	}
	data := marshalJSON(map[string]any{"head_sha": head, "targets": binding.Targets, "rounds_used": used, "max_rounds": max})
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM notices n
		WHERE n.task_id=? AND n.kind='review_cap_reached'
		AND n.id=(SELECT MAX(latest.id) FROM notices latest WHERE latest.task_id=n.task_id AND latest.kind='review_cap_reached')
		AND json_valid(n.data_json) AND json_extract(n.data_json,'$.head_sha')=?
		AND (NOT EXISTS(SELECT 1 FROM decisions d WHERE d.project_id=n.project_id AND d.origin='notice:'||n.id)
			OR EXISTS(SELECT 1 FROM decisions d WHERE d.project_id=n.project_id AND d.origin='notice:'||n.id AND d.answered_at=0 AND d.obsolete_at=0))
	)`, task.ID, head).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "review_cap_reached", Summary: fmt.Sprintf("%s: Review and repair cap reached at head %s (%d of %d Review/repair rounds)", task.Title, binding.label(), used, max), DataJSON: data})
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

func currentReviewCapHead(ctx context.Context, db *store.DB, project store.Project, task store.Task) (reviewCapBinding, error) {
	return reviewCapHead(ctx, db, project, task)
}

func userDispositionBlockAt(task store.Task) string {
	plan, err := task.AdmittedPlan()
	if err == nil && plan != nil && plan.ValidationLoops != nil {
		return plan.ValidationLoops.Review.BlockAt
	}
	return "P1"
}

func reviewCapNoticeData(notice store.Notice) (reviewCapBinding, int, int, error) {
	var data struct {
		Head    string               `json:"head_sha"`
		Targets []store.ReviewTarget `json:"targets"`
		Used    int                  `json:"rounds_used"`
		Max     int                  `json:"max_rounds"`
	}
	if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
		return reviewCapBinding{}, 0, 0, err
	}
	if data.Head == "" || data.Used < 1 || data.Max < 1 {
		return reviewCapBinding{}, 0, 0, fmt.Errorf("invalid Review cap Notice")
	}
	return reviewCapBinding{Head: data.Head, Targets: data.Targets}, data.Used, data.Max, nil
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
	binding, err := reviewCapHead(ctx, db, project, change)
	if err != nil {
		return err
	}
	return createReviewCapNotice(ctx, db, project, change, binding, used, max)
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
	if project.IsWorkspace() {
		for _, member := range plan.Execution.Members {
			if member.Review != "full-pipeline" {
				continue
			}
			candidate, err := db.LatestValidationCandidate(ctx, task.ID, member.Name)
			if store.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if candidate.State != store.ValidationCandidateActive {
				continue
			}
			lane, err := resolveCandidateLane(ctx, db, project, task, member.Name)
			if err != nil {
				return err
			}
			if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, candidate); err != nil {
				return err
			}
		}
	} else if fullPipelineSelected(task, "") {
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
	evidence, err := db.ReviewEvidenceFor(ctx, task.ID, targets)
	if err != nil {
		return err
	}
	if evidence.Complete {
		return nil
	}
	binding, err := newReviewCapBinding(targets)
	if err != nil {
		return err
	}
	return createReviewCapNotice(ctx, db, project, task, binding, used, max)
}

func requireFrozenReviewLoop(plan *taskplan.Plan) error {
	if plan == nil || plan.ValidationLoops == nil || !plan.ValidationLoops.Review.Enabled {
		return axi.Failure("review_loop_disabled", "Change Task has no enabled, frozen Review Loop", false)
	}
	return nil
}
