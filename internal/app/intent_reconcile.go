package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reconcileIntents(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot) error {
	intents, err := db.Intents(ctx, project.ID)
	if err != nil {
		return err
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intentProcessAlive(intent) {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, intent.TaskID)
		if err != nil {
			return err
		}
		claimed, err := db.ClaimIntent(ctx, intent.ID, intent.ProcessID, os.Getpid(), intent.Step)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		intent.ProcessID = os.Getpid()
		switch intent.Command {
		case "ride":
			err = s.recoverRideIntent(ctx, db, project, cfg, task, intent, snapshot)
		case "land --open-pr":
			err = s.recoverOpenPullRequestIntent(ctx, db, project, task, intent)
		case "land --merge":
			err = s.recoverLandIntent(ctx, db, project, cfg, task, intent)
		case workspaceLandIntent:
			err = s.recoverWorkspaceLandIntent(ctx, db, project, task, intent)
		case "unsaddle":
			if task.State == store.StateTornDown {
				err = db.FinishIntent(ctx, intent.ID, intent.ProcessID)
			} else {
				var payload struct {
					Discard bool `json:"discard"`
				}
				if decodeErr := json.Unmarshal([]byte(intent.PayloadJSON), &payload); decodeErr != nil {
					err = decodeErr
				} else if payload.Discard {
					if task.State != store.StateFailed && task.State != store.StateLost && task.State != store.StateStalled {
						err = fmt.Errorf("discard recovery refused Task in state %s", task.State)
					} else {
						err = fmt.Errorf("discard was interrupted; recovery will not continue the approved teardown")
					}
				} else if task.State != store.StateLanded && task.State != store.StateReported {
					err = fmt.Errorf("completion teardown recovery refused Task in state %s", task.State)
				} else {
					_, err = s.unsaddleTask(ctx, db, project, cfg, task, false, "")
				}
			}
		case "relaunch":
			if strings.HasPrefix(intent.Step, "done:task.working") && task.State == store.StateWorking {
				err = db.FinishIntent(ctx, intent.ID, intent.ProcessID)
			} else {
				_, err = s.relaunchTask(ctx, db, home, project, cfg, task, "")
			}
		default:
			err = fmt.Errorf("unknown interrupted command %q", intent.Command)
		}
		if err != nil {
			if noticeErr := s.recordIntentStuck(ctx, db, project, task, intent, err); noticeErr != nil {
				return noticeErr
			}
			if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); finishErr != nil && finishErr != store.ErrIntentOwned {
				return finishErr
			}
		}
	}
	return nil
}

func (s *Service) recoverOpenPullRequestIntent(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent) error {
	if task.LandingMode != "pr" || task.GatedSHA == "" || task.Branch == "" {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	output, err := runOutputStep(ctx, db, intent, "pr.recover_lookup", project.Root, "gh", "pr", "list", "--state", "open", "--base", project.DefaultBranch, "--head", task.Branch, "--json", "url,headRefName,headRefOid", "--limit", "5")
	if err != nil {
		return err
	}
	prURL, head, found, err := openPullRequestForBranch(output, task.Branch)
	if err != nil {
		return err
	}
	if !found {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	if head != task.GatedSHA {
		return pullRequestHeadMismatch(head, task.GatedSHA)
	}
	if err := validatePullRequestOrigin(ctx, project.Root, prURL); err != nil {
		return err
	}
	if err := db.UpdateTaskLanding(ctx, task.ID, prURL, ""); err != nil {
		return err
	}
	if task.State == store.StateDone {
		if err := db.Transition(ctx, task.ID, store.StateDone, store.StateLanding, "cli", "Recovered the open pull request for the gated Task branch"); err != nil {
			return err
		}
	}
	if err := createPROpenedNotice(ctx, db, project, task, prURL, task.GatedSHA, task.Title+": pull request opened"); err != nil {
		return err
	}
	return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
}

func (s *Service) recordIntentStuck(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent, cause error) error {
	data := marshalJSON(map[string]any{"intent_id": intent.ID, "command": intent.Command, "step": intent.Step})
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND kind='intent_stuck' AND data_json=?`, project.ID, data).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	summary := fmt.Sprintf("%s %s stopped at %s: %s", taskDisplayName(task), intent.Command, intent.Step, truncate(cause.Error(), 180))
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "intent_stuck", Summary: summary, DataJSON: data})
	return err
}

func (s *Service) recoverRideIntent(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, snapshot herdr.Snapshot) error {
	if task.State != store.StateSpawning {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
	if !found {
		if err := s.failSpawn(ctx, db, project, task.ID, task.Title, "ride was interrupted before a Rider pane appeared"); err != nil {
			return err
		}
		return nil
	}
	if pane.Agent == "" {
		if err := s.failSpawn(ctx, db, project, task.ID, task.Title, "ride was interrupted before its Rider started"); err != nil {
			return err
		}
		return nil
	}
	if !strings.HasPrefix(intent.Step, "done:agent.prompt") && !strings.HasPrefix(intent.Step, "done:task.working") && pane.AgentStatus != "working" {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
		if _, err := os.Stat(launchPath); err != nil {
			if failErr := s.failSpawn(ctx, db, project, task.ID, task.Title, "ride was interrupted before launch.md was written"); failErr != nil {
				return failErr
			}
			return nil
		}
		if err := s.deliverLaunchPrompt(ctx, pane.PaneID, "Read "+launchPath+" and follow it."); err != nil {
			if failErr := s.failSpawn(ctx, db, project, task.ID, task.Title, "interrupted ride could not resume its Rider: "+err.Error()); failErr != nil {
				return failErr
			}
			return nil
		}
	}
	if err := s.refreshWorkerDisplay(ctx, db, project, task.ID, taskKind(cfg, task)); err != nil {
		return err
	}
	if err := db.Transition(ctx, task.ID, store.StateSpawning, store.StateWorking, "cli", "Recovered an interrupted ride after the Rider started"); err != nil && err != store.ErrStateRace {
		return err
	}
	s.relabelProjectTabs(ctx, db, project)
	return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
}

func (s *Service) recoverLandIntent(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent) error {
	if task.State == store.StateTornDown || task.State == store.StateLanded {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	if task.State == store.StateDone {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	if task.State != store.StateLanding {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	if task.LandingMode == "pr" || task.LandingMode == "no-mistakes" {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	merged := false
	if task.GatedSHA != "" {
		_, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", task.GatedSHA, "refs/heads/"+project.DefaultBranch)
		merged = err == nil
	}
	if !merged {
		if err := db.Transition(ctx, task.ID, store.StateLanding, store.StateDone, "cli", "Interrupted Land did not complete its merge"); err != nil {
			return err
		}
		if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
			return err
		}
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	landedRef, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+project.DefaultBranch)
	if err != nil {
		return err
	}
	if err := db.UpdateTaskLanding(ctx, task.ID, task.PRURL, landedRef); err != nil {
		return err
	}
	if err := db.Transition(ctx, task.ID, store.StateLanding, store.StateLanded, "cli", "Local fast-forward completed at "+landedRef); err != nil {
		return err
	}
	task.State = store.StateLanded
	task.LandedRef = landedRef
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		return err
	}
	if shouldAutoUnsaddleLanded(cfg) {
		_, err = s.unsaddleTask(ctx, db, project, cfg, task, false, "")
		return err
	}
	return nil
}
