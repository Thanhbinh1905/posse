package app

import (
	"fmt"
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
	cfg, err := s.prepareProject(out.Context, db, project)
	if err != nil {
		return err
	}
	decision, err := db.Decision(out.Context, project.ID, id)
	if err != nil {
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
	case "leftover":
		if strings.HasPrefix(decision.Origin, "leftover:unrecoverable:") {
			if decision.Answer == "repair" {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "help", Value: []any{"Repair the Mount, then run `posse unsaddle " + taskIDString(task.Seq) + "` to retry the Leftover snapshot"}}})
			}
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if _, err := s.unsaddleTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		if decision.Answer == "discard" {
			target := project.Root
			ref := strings.TrimPrefix(decision.Origin, "leftover:")
			if project.IsWorkspace() {
				name, branch, ok := strings.Cut(ref, ":")
				if !ok {
					return fmt.Errorf("invalid workspace Leftover origin")
				}
				member, err := s.projectTarget(out.Context, db, project, name)
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
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "leftover_discarded"}})
		}
		branch := strings.TrimPrefix(decision.Origin, "leftover:")
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "leftover", Value: branch}, {Key: "help", Value: []any{"Write a Ship Brief for the follow-up and run `posse ride --brief <file> --name <title-slug> --from-leftover " + strconv.FormatInt(id, 10) + "`"}}})
	case "pr_closed":
		if decision.Answer == "discard" {
			if task.State == store.StateTornDown {
				return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
			}
			if task.State == store.StateLanding || task.State == store.StateDone {
				if err := db.Transition(out.Context, task.ID, task.State, store.StateWorking, "lead", "User chose to discard closed PR"); err != nil {
					return err
				}
				task.State = store.StateWorking
			}
			if task.State == store.StateWorking || task.State == store.StateNeedsDecision || task.State == store.StateBlocked {
				if err := db.Transition(out.Context, task.ID, task.State, store.StateLost, "cli", "User approved discard after closed PR"); err != nil {
					return err
				}
				task.State = store.StateLost
			}
			if task.State != store.StateLost && task.State != store.StateFailed && task.State != store.StateStalled {
				return axi.Failure("decision_refused", "Task cannot be discarded in state "+string(task.State), false)
			}
			if _, err := s.unsaddleTask(out.Context, db, project, cfg, task, true, decision.UserQuote); err != nil {
				return err
			}
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "torn-down"}})
		}
		url := strings.TrimPrefix(decision.Origin, "pr_closed:")
		target := project.Root
		memberName := ""
		if project.IsWorkspace() {
			members, err := s.workspaceMembers(out.Context, db, project, task)
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
		if err != nil && !store.IsNotFound(err) {
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
		if task.State == store.StateLanding || task.State == store.StateDone {
			if err := db.Transition(out.Context, task.ID, task.State, store.StateWorking, "lead", "User chose to reopen and relaunch"); err != nil {
				return err
			}
			task.State = store.StateWorking
		}
		if task.State == store.StateLanded || task.State == store.StateTornDown {
			return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: string(task.State)}})
		}
		if _, err := s.relaunchTask(out.Context, db, home, project, cfg, task, ""); err != nil {
			return err
		}
		return out.Print(axi.Object{{Key: "decision", Value: id}, {Key: "state", Value: "working"}})
	}
	return axi.Failure("decision_refused", "this Decision has no apply action", false)
}
