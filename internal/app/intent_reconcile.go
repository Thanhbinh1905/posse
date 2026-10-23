package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reconcileIntents(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot) error {
	return s.reconcileIntentsMode(ctx, db, project, cfg, snapshot, true, true)
}

// The hidden watch only resumes deferred PR creation after the Lead is running.
// Other interrupted commands belong to their CLI boundary, not passive polling.
func (s *Service) reconcileWatchIntents(ctx context.Context, db *store.DB, project store.Project) error {
	intents, err := db.Intents(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.Command != "merge --open-pr" || intentProcessAlive(intent) {
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
		if err := s.recoverOpenPullRequestIntent(ctx, db, project, task, intent); err != nil {
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

func (s *Service) reconcileIntentsMode(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot, allowLaunch, allowNetwork bool) error {
	intents, err := db.Intents(ctx, project.ID)
	if err != nil {
		return err
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if !allowNetwork && intent.Command == "merge --open-pr" {
			continue
		}
		if intentProcessAlive(intent) || !allowLaunch && (intent.Command == "relaunch" || intent.Command == "start") {
			continue
		}
		if intent.Command == "relaunch" {
			var payload struct {
				AutomaticRecovery bool `json:"automatic_recovery"`
			}
			if err := json.Unmarshal([]byte(intent.PayloadJSON), &payload); err != nil {
				return err
			}
			// Automatic recovery owns its crashed intent and retry budget;
			// an unrelated explicit relaunch still uses normal intent recovery.
			if payload.AutomaticRecovery {
				continue
			}
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
		case "start":
			err = s.recoverStartIntent(ctx, db, project, cfg, task, intent, snapshot)
		case "merge --open-pr":
			err = s.recoverOpenPullRequestIntent(ctx, db, project, task, intent)
		case "merge --execute":
			err = s.recoverMergeIntent(ctx, db, project, cfg, task, intent)
		case workspaceMergeIntent:
			err = s.recoverWorkspaceMergeIntent(ctx, db, project, task, intent)
		case "teardown":
			if task.State == store.StateTornDown {
				err = db.FinishIntent(ctx, intent.ID, intent.ProcessID)
			} else {
				var payload discardIntentPayload
				if decodeErr := json.Unmarshal([]byte(intent.PayloadJSON), &payload); decodeErr != nil {
					err = decodeErr
				} else if payload.Discard {
					if !store.IsApprovedDiscardSourceState(task.State) {
						err = fmt.Errorf("discard recovery refused Task in state %s", task.State)
					} else {
						quote, quoteErr := db.LatestApprovalQuote(ctx, task.ID, "discard")
						if quoteErr != nil && !store.IsNotFound(quoteErr) {
							err = quoteErr
						} else if strings.TrimSpace(quote) == "" {
							quote = payload.UserQuote
						}
						if err == nil && strings.TrimSpace(quote) == "" {
							err = fmt.Errorf("discard recovery has no recorded User approval quote")
						} else if err == nil && payload.Worker == nil {
							// Legacy or pre-binding crashes have no retained stop authority.
							err = s.requireNoLiveTaskAgent(ctx, project, task)
						}
						if err == nil {
							_, err = s.teardownTask(ctx, db, project, cfg, task, true, quote)
						}
					}
				} else if task.State != store.StateMerged && task.State != store.StateReported {
					err = fmt.Errorf("completion teardown recovery refused Task in state %s", task.State)
				} else {
					_, err = s.teardownTask(ctx, db, project, cfg, task, false, "")
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
	if task.MergeMode != "pr" || task.GatedSHA == "" || task.Branch == "" {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	output, err := runOutputStep(ctx, db, intent, "pr.recover_lookup", project.Root, "gh", "pr", "list", "--state", "open", "--base", taskTargetBranch(project, task), "--head", task.Branch, "--json", "url,headRefName,headRefOid", "--limit", "5")
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
	if err := db.UpdateTaskMerge(ctx, task.ID, prURL, ""); err != nil {
		return err
	}
	if task.State == store.StateDone {
		if err := db.Transition(ctx, task.ID, store.StateDone, store.StateMerging, "cli", "Recovered the open pull request for the gated Task branch"); err != nil {
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

func (s *Service) recoverStartIntent(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, snapshot herdr.Snapshot) error {
	if task.State != store.StateSpawning {
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
	if !found {
		if err := s.failSpawn(ctx, db, project, task.ID, task.Title, "start was interrupted before a Worker pane appeared"); err != nil {
			return err
		}
		return nil
	}
	if pane.Agent == "" {
		if err := s.failSpawn(ctx, db, project, task.ID, task.Title, "start was interrupted before its Worker started"); err != nil {
			return err
		}
		return nil
	}
	if !strings.HasPrefix(intent.Step, "done:agent.prompt") && !strings.HasPrefix(intent.Step, "done:task.working") && pane.AgentStatus != "working" && taskKind(cfg, task) != "codex" && taskKind(cfg, task) != "opencode" {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
		if _, err := os.Stat(launchPath); err != nil {
			if failErr := s.failSpawn(ctx, db, project, task.ID, task.Title, "start was interrupted before launch.md was written"); failErr != nil {
				return failErr
			}
			return nil
		}
		if err := s.deliverLaunchPrompt(ctx, pane.PaneID, "Read "+launchPath+" and follow it."); err != nil {
			var focused *axi.Error
			if errors.As(err, &focused) && focused.Code == "pane_focused" {
				return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
			}
			if failErr := s.failSpawn(ctx, db, project, task.ID, task.Title, "interrupted start could not resume its Worker: "+err.Error()); failErr != nil {
				return failErr
			}
			return nil
		}
	}
	if err := s.refreshWorkerDisplay(ctx, db, project, task.ID, taskKind(cfg, task)); err != nil {
		return err
	}
	if err := db.Transition(ctx, task.ID, store.StateSpawning, store.StateWorking, "cli", "Recovered an interrupted start after the Worker started"); err != nil && err != store.ErrStateRace {
		return err
	}
	s.relabelProjectTabs(ctx, db, project)
	return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
}

func (s *Service) recoverMergeIntent(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent) error {
	finish := func(evidence string) error {
		if err := db.ReconcileResourceLeasesForIntent(ctx, intent.ID, evidence); err != nil {
			return err
		}
		return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
	}
	if task.State == store.StateTornDown || task.State == store.StateMerged {
		return finish("verified Task is already terminal")
	}
	if task.State == store.StateDone {
		return finish("verified Task is done and no local merge is pending")
	}
	if task.State != store.StateMerging {
		return finish("verified Task is not in local merging state")
	}
	if task.MergeMode == "pr" || task.MergeMode == "no-mistakes" {
		return finish("verified remote merging has no local Git ref effect")
	}
	merged := false
	if task.GatedSHA != "" {
		targetRef := "refs/heads/" + taskTargetBranch(project, task)
		_, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", task.GatedSHA, targetRef)
		merged = err == nil
	}
	if !merged {
		if err := db.Transition(ctx, task.ID, store.StateMerging, store.StateDone, "cli", "Interrupted Merge did not complete its merge"); err != nil {
			return err
		}
		if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
			return err
		}
		return finish("re-read target ref and confirmed the interrupted local merge did not merge")
	}
	mergedRef, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+taskTargetBranch(project, task))
	if err != nil {
		return err
	}
	if err := db.UpdateTaskMerge(ctx, task.ID, task.PRURL, mergedRef); err != nil {
		return err
	}
	if err := db.Transition(ctx, task.ID, store.StateMerging, store.StateMerged, "cli", "Local fast-forward completed at "+mergedRef); err != nil {
		return err
	}
	task.State = store.StateMerged
	task.MergedRef = mergedRef
	if err := finish("re-read target ref and recorded the interrupted local merge as merged"); err != nil {
		return err
	}
	if shouldAutoTeardownMerged(cfg) {
		_, err = s.teardownTask(ctx, db, project, cfg, task, false, "")
		return err
	}
	return nil
}
