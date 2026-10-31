package app

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// applyDecision performs only the action the User already chose. Repeating it
// after a crash is safe: discarded refs and torn-down Tasks stay absent.
func (s *Service) applyDecision(out *axi.Context, args []string) error {
	if len(args) != 1 {
		return axi.Usage("apply requires one answered Decision id")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id < 1 {
		return axi.Usage("apply requires a positive Decision id")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(out.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(out.Context, db, project); err != nil {
		return err
	}
	decision, err := db.Decision(out.Context, project.ID, id)
	if err != nil {
		return err
	}
	cfg, err := s.prepareTaskProject(out.Context, db, project, decision.TaskID)
	if err != nil {
		return err
	}
	if decision, err = db.Decision(out.Context, project.ID, id); err != nil {
		return err
	}
	if decision.AnsweredAt == 0 {
		return axi.Failure("decision_refused", "Decision has not been answered by the User", false)
	}
	task, err := db.TaskByID(out.Context, project.ID, decision.TaskID)
	if err != nil {
		return err
	}
	switch decision.Kind {
	case "dependency":
		if task.State != store.StateQueued {
			return axi.Failure("decision_refused", "dependency Decision no longer applies to this queued Task", false)
		}
		switch decision.Answer {
		case "cancel":
			if err := db.Transition(out.Context, task.ID, store.StateQueued, store.StateTornDown, "cli", "Canceled before Worker dispatch"); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		case "drop-prerequisite-and-run":
			parts := strings.Split(decision.Origin, ":")
			if len(parts) != 4 || parts[0] != "dependency" || parts[1] != taskIDString(task.Seq) {
				return axi.Failure("decision_refused", "dependency Decision binding is invalid", false)
			}
			plan, err := task.AdmittedPlan()
			if err != nil || plan == nil {
				return axi.Failure("decision_refused", "queued Task has no admitted plan", false)
			}
			revised, err := plan.WithoutDependency(parts[2], parts[3])
			if err != nil {
				return axi.Failure("decision_refused", err.Error(), false)
			}
			if err := db.ReviseTaskPlan(out.Context, task, revised); err != nil {
				return err
			}
			if _, err := s.refillQueuedTasks(out.Context, db, project, cfg); err != nil {
				return err
			}
			updated, err := db.TaskByID(out.Context, project.ID, task.ID)
			if err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: string(updated.State)}, {Key: "plan_revision", Value: revised.Revision}})
		default:
			return axi.Failure("decision_refused", "unknown dependency Decision answer", false)
		}
	case "merge_ready":
		if decision.Answer == "discard" && task.State == store.StateTornDown {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		if decision.Answer != "discard" {
			state := "waiting"
			if decision.Answer == "merge" {
				state = "merge_ready"
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: state}, {Key: "help", Value: []any{"Run `posse merge " + taskIDString(task.Seq) + " --execute` to Merge this Task, or answer `discard` to tear it down"}}})
		}
		if !canDiscardFromDecision(decision.Kind, task.State) || task.Launches != decision.TaskLaunches {
			return axi.Failure("decision_refused", "merge-ready Decision no longer applies to this Task state or Worker launch", false)
		}
		if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
			return err
		}
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
	case "worker_exited":
		if decision.Answer == "discard" && task.State == store.StateTornDown {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		if task.Launches != decision.TaskLaunches || (task.State != store.StateDone && task.State != store.StateMerging) {
			return axi.Failure("decision_refused", "worker-exited Decision no longer applies to this Task state or Worker launch", false)
		}
		if decision.Answer == "relaunch" {
			if _, err := s.relaunchTask(out.Context, db, home, project, cfg, task, ""); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "working"}})
		}
		if decision.Answer == "discard" {
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
	case "recovery":
		if decision.Answer == "relaunch" {
			if task.State == store.StateWorking || task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: string(task.State)}})
			}
			if _, err := s.relaunchTask(out.Context, db, home, project, cfg, task, ""); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "working"}})
		}
		if decision.Answer == "preserve" {
			plan, err := task.AdmittedPlan()
			if err != nil {
				return admittedPlanFailure(err)
			}
			if plan == nil || plan.Execution.Mode != "in-place" {
				return axi.Failure("decision_refused", "preserve is available only for in-place recovery", false)
			}
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, false, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}, {Key: "source_preserved", Value: true}})
		}
		if decision.Answer == "discard" {
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if !canDiscardFromDecision(decision.Kind, task.State) {
				return axi.Failure("decision_refused", "recovery Decision cannot discard a Task in state "+string(task.State), false)
			}
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
	case "quota":
		if decision.Answer == "discard" && task.State == store.StateTornDown {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		if task.Launches != decision.TaskLaunches || !store.QuotaDecisionState(task.State) {
			return axi.Failure("decision_refused", "quota Decision no longer applies to this Task state or Worker launch", false)
		}
		switch decision.Answer {
		case "relaunch":
			if _, err := s.relaunchTask(out.Context, db, home, project, cfg, task, ""); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "working"}})
		case "discard":
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		default:
			return axi.Failure("decision_refused", "unknown quota Decision answer", false)
		}
	case "review_cap":
		taskRef, expectedHead, ok := parseReviewCapOrigin(decision.Origin)
		if !ok || taskRef != taskIDString(task.Seq) || task.Type != "change" {
			return axi.Failure("decision_refused", "Review cap Decision binding is invalid", false)
		}
		actual, err := currentReviewCapHead(out.Context, db, project, task)
		if err != nil {
			return err
		}
		if actual.Head != expectedHead {
			return axi.Failure("decision_refused", "the reviewed candidate head changed after this Review cap Decision; refresh the cap Notice before applying it", false)
		}
		switch decision.Answer {
		case "accept-head":
			heads := make([]string, len(actual.Targets))
			for i, target := range actual.Targets {
				heads[i] = target.Commit
			}
			if err := db.RecordReviewDisposition(out.Context, task.ID, decision.ID, heads, decision.UserQuote, userDispositionBlockAt(task)); err != nil {
				return err
			}
			if err := queueSettledReviewStep(out.Context, db, project, task, "User accepted the candidate head at the Review cap of Task "+taskIDString(task.Seq)); err != nil {
				return err
			}
			if len(actual.Targets) > 1 {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "accepted-head"}, {Key: "heads", Value: actual.Targets}})
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "accepted-head"}, {Key: "head", Value: expectedHead}})
		case "final-fix":
			queued, err := s.queueReviewFix(out.Context, db, project, task, nil, true)
			if err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "final-fix-queued"}, {Key: "message", Value: queued.MessageID}, {Key: "patterns", Value: queued.Selected}})
		case "rethink":
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "rethink"}, {Key: "help", Value: []any{"Create a new Change Brief with `supersedes: " + taskIDString(task.Seq) + "`, then run `posse start --brief <file>`; inherited findings stay visible without being reported again.", "The capped Task remains unchanged; use the normal discard or Orphan path when appropriate."}}})
		case "discard":
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		default:
			return axi.Failure("decision_refused", "unknown Review cap Decision answer", false)
		}
	case "review":
		if decision.Answer == "ignore" {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "ignored"}})
		}
		target := task
		if task.ReviewsTaskID != 0 {
			target, err = db.TaskByID(out.Context, project.ID, task.ReviewsTaskID)
			if err != nil {
				return err
			}
		}
		if target.State == store.StateTornDown || target.State == store.StateMerged {
			return axi.Failure("decision_refused", "review target is already finished", false)
		}
		// The answer authorizes sharing the findings, not merging a Change Task.
		file := "findings.toon"
		if task.Type == "review" {
			file = "report.md"
		}
		path := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), file)
		message := "User chose " + decision.Answer + " for findings on " + task.Title + ". Read " + path + " and carry out the chosen response before reporting done."
		if err := db.QueueDecisionMessage(out.Context, decision, target.ID, message); err != nil {
			return err
		}
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "queued"}, {Key: "task", Value: taskIDString(target.Seq)}})
	case "orphan":
		if strings.HasPrefix(decision.Origin, "orphan:unrecoverable:") {
			if decision.Answer == "repair" {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "help", Value: []any{"Repair the Sandbox, then run `posse teardown " + taskIDString(task.Seq) + "` to retry Teardown"}}})
			}
			if task.State == store.StateTornDown {
				if err := db.ObsoleteAppliedOrphanDecision(out.Context, decision.ID); err != nil {
					return err
				}
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if !store.IsApprovedDiscardSourceState(task.State) {
				return axi.Failure("decision_refused", "Task cannot be discarded in state "+string(task.State), false)
			}
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			if err := db.ObsoleteAppliedOrphanDecision(out.Context, decision.ID); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		if decision.Answer == "discard" {
			target := project.Root
			ref := strings.TrimPrefix(decision.Origin, "orphan:")
			if project.IsWorkspace() {
				name, branch, ok := strings.Cut(ref, ":")
				if !ok {
					return fmt.Errorf("invalid workspace Orphan origin")
				}
				member, err := projectTarget(out.Context, db, project, name)
				if err != nil {
					return err
				}
				target, ref = member.Root, branch
			}
			branchRef := "refs/heads/" + ref
			sha, err := gitOutput(out.Context, target, "rev-parse", "--verify", branchRef)
			if err == nil {
				if _, err = gitOutput(out.Context, target, "update-ref", "-d", branchRef, sha); err != nil {
					return err
				}
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "orphan_discarded"}})
		}
		branch := strings.TrimPrefix(decision.Origin, "orphan:")
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "orphan", Value: branch}, {Key: "help", Value: []any{"Write a Change Brief for the follow-up and run `posse start --brief <file> [--name <display-name>] --from-orphan " + strconv.FormatInt(id, 10) + "`"}}})
	case "pr_closed":
		if decision.Answer == "discard" {
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if !strings.HasPrefix(decision.Origin, "pr_closed:invalid:") {
				if err := s.pollProjectPullRequests(out.Context, db, project, cfg, true); err != nil {
					return err
				}
				task, err = db.TaskByID(out.Context, project.ID, task.ID)
				if err != nil {
					return err
				}
				if task.State == store.StateMerged || task.State == store.StateTornDown {
					return axi.Failure("decision_refused", "Task has merged; use normal Teardown so unmerged work is preserved", false)
				}
				url := closedPRDecisionURL(decision.Origin)
				state := ""
				if !project.IsWorkspace() {
					observation, observationErr := db.LatestPRObservation(out.Context, task.ID)
					if observationErr != nil {
						return observationErr
					}
					if observation.PRURL == url {
						state = observation.State
					}
				} else {
					repos, reposErr := db.TaskRepos(out.Context, task.ID)
					if reposErr != nil {
						return reposErr
					}
					for _, repo := range repos {
						if repo.PRURL != url {
							continue
						}
						observation, observationErr := db.LatestMemberPRObservation(out.Context, task.ID, repo.Repo)
						if observationErr != nil {
							return observationErr
						}
						state = observation.State
						break
					}
				}
				if state != "CLOSED" {
					return axi.Failure("decision_refused", "the PR is no longer verified closed; inspect its current state before discarding", true)
				}
			}
			if err := s.requireNoLiveTaskAgent(out.Context, project, task); err != nil {
				return err
			}
			if !canDiscardFromDecision(decision.Kind, task.State) {
				return axi.Failure("decision_refused", "closed-PR Decision cannot discard a Task in state "+string(task.State), false)
			}
			if _, err := s.teardownTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		url := closedPRDecisionURL(decision.Origin)
		target := project.Root
		memberName := ""
		if project.IsWorkspace() {
			members, err := workspaceMembers(out.Context, db, project, task)
			if err != nil {
				return err
			}
			for _, member := range members {
				if member.repo.PRURL == url {
					target, memberName = member.target.Root, member.repo.Repo
					break
				}
			}
			if memberName == "" {
				return axi.Failure("decision_refused", "closed Member PR is no longer recorded on this Task", false)
			}
		}
		var observation store.PRObservation
		if project.IsWorkspace() {
			observation, err = db.LatestMemberPRObservation(out.Context, task.ID, memberName)
		} else {
			observation, err = db.LatestPRObservation(out.Context, task.ID)
		}
		invalidURL := store.IsNotFound(err)
		if err != nil && !invalidURL {
			return err
		}
		if err == nil && observation.State == "CLOSED" {
			forge, err := forgeForRepository(out.Context, target, cfg, memberName)
			if err != nil {
				return err
			}
			if forge.Kind == "github" {
				if output, err := commandOutputArgs(out.Context, target, "gh", "pr", "reopen", url); err != nil {
					return axi.Failure("pr_reopen_failed", "could not reopen pull request", true, output)
				}
			} else {
				return axi.Failure("pr_reopen_failed", "reopen the merge request on the forge before applying the Decision", true)
			}
		}
		if err := s.pollProjectPullRequests(out.Context, db, project, cfg, true); err != nil {
			return err
		}
		task, err = db.TaskByID(out.Context, project.ID, task.ID)
		if err != nil {
			return err
		}
		if task.State == store.StateMerging || task.State == store.StateDone {
			if err := db.Transition(out.Context, task.ID, task.State, store.StateWorking, "lead", "User chose to reopen and relaunch"); err != nil {
				return err
			}
			task.State = store.StateWorking
		}
		if task.State == store.StateMerged || task.State == store.StateTornDown {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: string(task.State)}})
		}
		relaunched, err := s.relaunchTask(out.Context, db, home, project, cfg, task, "")
		if err != nil {
			return err
		}
		if invalidURL {
			if _, err := s.herdrCall(out.Context, "agent.prompt", map[string]any{"target": relaunched.Pane, "text": "The previous URL " + url + " was not a pull request. Publish a real pull request for this Task and report its new URL with `posse signal done`."}); err != nil {
				return err
			}
		}
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "working"}})
	}
	return axi.Failure("decision_refused", "this Decision has no apply action", false)
}

func canDiscardFromDecision(kind string, state store.State) bool {
	if !store.IsApprovedDiscardSourceState(state) {
		return false
	}
	switch kind {
	case "recovery":
		return state == store.StateFailed || state == store.StateLost
	case "worker_exited":
		return state == store.StateDone || state == store.StateMerging
	case "merge_ready":
		return state == store.StateDone || state == store.StateMerging
	case "pr_closed":
		return state != store.StateReported && state != store.StateMerged
	case "orphan":
		return true
	case "quota":
		return store.QuotaDecisionState(state)
	default:
		return false
	}
}
