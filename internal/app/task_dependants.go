package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func queuedTaskDependants(ctx context.Context, db *store.DB, projectID int64, predecessor store.Task) ([]store.Task, error) {
	tasks, err := db.Tasks(ctx, projectID, true)
	if err != nil {
		return nil, err
	}
	predecessorID := taskIDString(predecessor.Seq)
	dependants := make([]store.Task, 0)
	for _, task := range tasks {
		if task.State != store.StateQueued {
			continue
		}
		plan, err := task.AdmittedPlan()
		if err != nil {
			return nil, admittedPlanFailure(err)
		}
		if plan == nil {
			continue
		}
		for _, dependency := range plan.Dependencies {
			if dependency.TaskID != predecessorID {
				continue
			}
			unmet, err := db.UnmetTaskDependencies(ctx, projectID, []dispatch.Dependency{dependency})
			if err != nil {
				return nil, err
			}
			if len(unmet) > 0 {
				dependants = append(dependants, task)
				break
			}
		}
	}
	return dependants, nil
}

func (s *Service) holdQueuedDependants(ctx context.Context, db *store.DB, project store.Project, predecessor store.Task) ([]string, error) {
	dependants, err := queuedTaskDependants(ctx, db, project.ID, predecessor)
	if err != nil || len(dependants) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(dependants))
	for _, dependant := range dependants {
		dependentRef := taskIDString(dependant.Seq)
		ids = append(ids, dependentRef)
		if err := db.ObsoleteDependencyDecision(ctx, dependant.ID, dependentRef, taskIDString(predecessor.Seq)); err != nil {
			return nil, err
		}
	}
	origin := "teardown-dependants:" + taskIDString(predecessor.Seq) + ":" + strings.Join(ids, ",")
	_, err = db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID,
		TaskID:    predecessor.ID,
		Origin:    origin,
		Kind:      "teardown_dependants",
		Question:  fmt.Sprintf("Tearing down %s left queued dependants %s. Re-point them to a replacement Task, or cascade their teardown?", taskIDString(predecessor.Seq), strings.Join(ids, ", ")),
		Options:   []string{"re-point", "cascade"},
	})
	return ids, err
}

func teardownDecisionDependants(ctx context.Context, db *store.DB, project store.Project, decision store.Decision) (store.Task, []store.Task, error) {
	predecessor, err := db.TaskByID(ctx, project.ID, decision.TaskID)
	if err != nil {
		return store.Task{}, nil, err
	}
	parts := strings.Split(decision.Origin, ":")
	if len(parts) != 3 || parts[0] != "teardown-dependants" || parts[1] != taskIDString(predecessor.Seq) {
		return store.Task{}, nil, fmt.Errorf("teardown dependant Decision binding is invalid")
	}
	dependants := make([]store.Task, 0)
	for _, taskRef := range strings.Split(parts[2], ",") {
		dependant, err := db.Task(ctx, project.ID, taskRef)
		if err != nil {
			return store.Task{}, nil, fmt.Errorf("queued dependant %s no longer exists", taskRef)
		}
		if dependant.State != store.StateQueued {
			continue
		}
		plan, err := dependant.AdmittedPlan()
		if err != nil {
			return store.Task{}, nil, admittedPlanFailure(err)
		}
		if plan == nil {
			continue
		}
		for _, dependency := range plan.Dependencies {
			if dependency.TaskID == taskIDString(predecessor.Seq) {
				dependants = append(dependants, dependant)
				break
			}
		}
	}
	return predecessor, dependants, nil
}

func validateTeardownRepoint(ctx context.Context, db *store.DB, project store.Project, decision store.Decision, replacement store.Task) error {
	predecessor, dependants, err := teardownDecisionDependants(ctx, db, project, decision)
	if err != nil {
		return err
	}
	if replacement.ID == predecessor.ID {
		return fmt.Errorf("replacement Task must differ from the torn-down predecessor")
	}
	for _, dependant := range dependants {
		plan, err := dependant.AdmittedPlan()
		if err != nil {
			return admittedPlanFailure(err)
		}
		for _, dependency := range plan.Dependencies {
			if dependency.TaskID != taskIDString(predecessor.Seq) {
				continue
			}
			if replacement.Seq >= dependant.Seq {
				return fmt.Errorf("replacement Task %s must precede dependant %s", taskIDString(replacement.Seq), taskIDString(dependant.Seq))
			}
			validType := dependency.Milestone == "landed" && replacement.Type == "change" || dependency.Milestone == "reported" && (replacement.Type == "research" || replacement.Type == "review") || dependency.Milestone == "accepted" && replacement.Type == "research" && (replacement.ReportKind == "spec" || replacement.ReportKind == "tickets")
			if !validType {
				return fmt.Errorf("replacement Task %s cannot satisfy %s", taskIDString(replacement.Seq), dependency.Milestone)
			}
		}
	}
	return nil
}

func (s *Service) applyTeardownDependantsDecision(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, decision store.Decision) error {
	predecessor, dependants, err := teardownDecisionDependants(ctx, db, project, decision)
	if err != nil {
		return err
	}
	if len(dependants) == 0 {
		return nil
	}
	if decision.Answer == "cascade" {
		allQueued, err := db.Tasks(ctx, project.ID, true)
		if err != nil {
			return err
		}
		plans := make(map[int64]*taskplan.Plan, len(allQueued))
		for _, task := range allQueued {
			if task.State != store.StateQueued {
				continue
			}
			plan, err := task.AdmittedPlan()
			if err != nil {
				return admittedPlanFailure(err)
			}
			plans[task.ID] = plan
		}
		byRef := make(map[string]store.Task, len(allQueued))
		for _, task := range allQueued {
			byRef[taskIDString(task.Seq)] = task
		}
		closure := make([]store.Task, 0, len(dependants))
		seen := make(map[int64]bool, len(dependants))
		frontier := dependants
		for len(frontier) > 0 {
			next := make([]store.Task, 0)
			for _, current := range frontier {
				if seen[current.ID] {
					continue
				}
				seen[current.ID] = true
				closure = append(closure, current)
				for _, candidate := range allQueued {
					if seen[candidate.ID] || candidate.State != store.StateQueued || plans[candidate.ID] == nil {
						continue
					}
					for _, dependency := range plans[candidate.ID].Dependencies {
						if dependency.TaskID == taskIDString(current.Seq) {
							next = append(next, byRef[taskIDString(candidate.Seq)])
							break
						}
					}
				}
			}
			frontier = next
		}
		raced, err := db.CascadeTeardownQueued(ctx, closure, "Cascade teardown approved for predecessor "+taskIDString(predecessor.Seq))
		if err != nil {
			return err
		}
		if len(raced) > 0 {
			taskRefs := make([]string, 0, len(raced))
			for _, taskID := range raced {
				task, err := db.TaskByID(ctx, project.ID, taskID)
				if err != nil {
					return err
				}
				taskRefs = append(taskRefs, taskIDString(task.Seq))
			}
			return fmt.Errorf("cascade skipped dependants after concurrent state changes: %s", strings.Join(taskRefs, ", "))
		}
		return nil
	}
	if decision.Answer != "re-point" || !strings.HasPrefix(decision.AnswerReason, "to=") {
		return fmt.Errorf("re-point requires a replacement Task ID")
	}
	replacementID := strings.TrimPrefix(decision.AnswerReason, "to=")
	replacement, err := db.Task(ctx, project.ID, replacementID)
	if err != nil {
		return fmt.Errorf("replacement Task %s does not exist", replacementID)
	}
	if err := validateTeardownRepoint(ctx, db, project, decision, replacement); err != nil {
		return err
	}
	updates := make([]store.TaskPlanRevision, 0, len(dependants))
	for _, dependant := range dependants {
		plan, err := dependant.AdmittedPlan()
		if err != nil {
			return admittedPlanFailure(err)
		}
		if plan == nil {
			return fmt.Errorf("queued dependant %s has no admitted plan", taskIDString(dependant.Seq))
		}
		revised := *plan
		for _, dependency := range plan.Dependencies {
			if dependency.TaskID != taskIDString(predecessor.Seq) {
				continue
			}
			revised, err = revised.RepointDependency(dependency.TaskID, taskIDString(replacement.Seq), dependency.Milestone)
			if err != nil {
				return err
			}
		}
		updates = append(updates, store.TaskPlanRevision{Task: dependant, Plan: revised})
	}
	if err := db.ReviseTaskPlans(ctx, updates); err != nil {
		return err
	}
	_, err = s.refillQueuedTasks(ctx, db, project, cfg, false)
	return err
}
