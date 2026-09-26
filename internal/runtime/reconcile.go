package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

const ReconcileBudget = 3 * time.Second
const AgentAbsentGrace = 2 * time.Minute

type RunResult struct {
	Snapshot           herdr.Snapshot
	Notices            []store.Notice
	GenerationMismatch bool
}

type ProgressSource interface {
	ReadPane(context.Context, herdr.Adapter, string, int) (string, error)
	WorktreeFingerprint(context.Context, string) (string, error)
}

type SystemProgress struct{}

func Run(ctx context.Context, db *store.DB, adapter herdr.Adapter, projectID int64, stallAfter, idleAfter time.Duration, now time.Time, progress ProgressSource) (result RunResult, returnedErr error) {
	budgetCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
	defer cancel()
	snapshot, err := adapter.Snapshot(budgetCtx)
	if err != nil {
		return RunResult{}, err
	}
	defer func() {
		if snapshot.ServerStartedAt == "" {
			return
		}
		// Leave a changed generation pending until recover --all handles it.
		// A Herdr event can run before the startup hook and must not consume the
		// restart signal that the hook uses to relaunch Workers and the Lead.
		if err := db.RememberProjectServerStartedAt(ctx, projectID, snapshot.ServerStartedAt); err != nil {
			if returnedErr == nil {
				returnedErr = err
			} else {
				returnedErr = errors.Join(returnedErr, err)
			}
		}
	}()
	if now.IsZero() {
		now = time.Now()
	}
	result = RunResult{Snapshot: snapshot}
	// Recovery is the only operation that advances the recorded generation.
	// A plugin event may have captured a snapshot before a server restart and
	// resumed after recovery. Do not evaluate stalls against that old layout.
	matches, err := snapshotMatchesProjectGeneration(budgetCtx, db, projectID, snapshot)
	if err != nil || !matches {
		result.GenerationMismatch = err == nil && !matches
		return result, err
	}
	notices, err := ReconcileSnapshot(budgetCtx, db, projectID, snapshot, now, idleAfter)
	result.Notices = append(result.Notices, notices...)
	if err != nil {
		return result, err
	}
	if stallAfter > 0 {
		if progress == nil {
			progress = SystemProgress{}
		}
		stalled, err := evaluateStallsSnapshot(budgetCtx, db, adapter, projectID, snapshot, stallAfter, now, progress)
		result.Notices = append(result.Notices, stalled...)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func snapshotMatchesProjectGeneration(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot) (bool, error) {
	generation, err := db.ProjectServerStartedAt(ctx, projectID)
	return generation == "" || snapshot.ServerStartedAt == "" || generation == snapshot.ServerStartedAt, err
}

func ReconcileSnapshot(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot, now time.Time, idleAfterValues ...time.Duration) ([]store.Notice, error) {
	idleAfter := time.Duration(0)
	if len(idleAfterValues) > 0 {
		idleAfter = idleAfterValues[0]
	}
	if now.IsZero() {
		now = time.Now()
	}
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	previousServerStartedAt, err := db.ProjectServerStartedAt(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// A snapshot from another Herdr generation cannot identify current panes.
	// On a new restart, the startup recovery hook owns relaunch and advances
	// this generation; until then, do not overwrite recorded pane ids.
	if previousServerStartedAt != "" && snapshot.ServerStartedAt != "" && previousServerStartedAt != snapshot.ServerStartedAt {
		return nil, nil
	}
	tasks, err := db.LiveTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var notices []store.Notice
	var failures []error
	for _, task := range tasks {
		if task.State == store.StateSpawning && task.PaneID == "" {
			continue
		}
		pane, found := findPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if !found {
			if task.State == store.StateDone || task.State == store.StateLanding {
				if err := db.CreateWorkerExitedNotice(ctx, task, now); err != nil && !errors.Is(err, store.ErrStateRace) {
					failures = append(failures, err)
				}
				continue
			}
			agentServerRestarted := task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt
			if agentServerRestarted {
				absentSince := task.AgentAbsentSince
				if absentSince == 0 {
					absentSince = now.UnixMilli()
				}
				if now.Sub(time.UnixMilli(absentSince)) >= AgentAbsentGrace {
					if err := markLost(ctx, db, task, "Rider pane and label are absent from the Herdr snapshot", now, &notices); err != nil && !errors.Is(err, store.ErrStateRace) {
						failures = append(failures, err)
					}
					continue
				}
				if err := db.UpdateTaskObservation(ctx, task.ID, task.PaneID, task.HerdrWorkspaceID, sessionref.Sanitize("", task.AgentSession), absentSince, 0, task.AgentServerStartedAt); err != nil {
					if !isStateRace(err) {
						failures = append(failures, err)
					}
				}
				continue
			}
			if err := markLost(ctx, db, task, "Rider pane and label are absent from the Herdr snapshot", now, &notices); err != nil {
				if !errors.Is(err, store.ErrStateRace) {
					failures = append(failures, err)
				}
			}
			continue
		}
		absentSince := task.AgentAbsentSince
		agentPresent := pane.Agent != ""
		session := sessionref.Sanitize(pane.Agent, task.AgentSession)
		if len(pane.AgentSession) > 0 && string(pane.AgentSession) != "null" {
			if reported := sessionref.Sanitize(pane.Agent, string(pane.AgentSession)); reported != "" {
				session = reported
			}
		}
		if !agentPresent {
			if absentSince == 0 {
				absentSince = now.UnixMilli()
			}
			resumePending := hasRecordedSession(task.AgentSession) && task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt
			if !resumePending && now.Sub(time.UnixMilli(absentSince)) >= AgentAbsentGrace {
				if task.State == store.StateDone || task.State == store.StateLanding {
					if err := db.CreateWorkerExitedNotice(ctx, task, now); err != nil && !errors.Is(err, store.ErrStateRace) {
						failures = append(failures, err)
					}
					continue
				}
				if err := markLost(ctx, db, task, "Rider has been absent for two minutes", now, &notices); err != nil {
					if !errors.Is(err, store.ErrStateRace) {
						failures = append(failures, err)
					}
				}
				continue
			}
		} else {
			absentSince = 0
		}
		serverStartedAt := task.AgentServerStartedAt
		if agentPresent && snapshot.ServerStartedAt != "" {
			serverStartedAt = snapshot.ServerStartedAt
		}
		idleSince := task.IdleSince
		if task.State == store.StateWorking && agentPresent && (pane.AgentStatus == "idle" || pane.AgentStatus == "done") {
			if idleSince == 0 {
				idleSince = now.UnixMilli()
			}
		} else {
			idleSince = 0
		}
		if err := db.UpdateTaskObservation(ctx, task.ID, pane.PaneID, pane.WorkspaceID, session, absentSince, idleSince, serverStartedAt); err != nil {
			if !isStateRace(err) {
				failures = append(failures, fmt.Errorf("update Task %s observation: %w", taskID(task.Seq), err))
			}
			continue
		}
		if task.State == store.StateWorking && idleAfter > 0 && idleSince > 0 && now.Sub(time.UnixMilli(idleSince)) >= idleAfter {
			exists, err := db.HasNotice(ctx, task.ProjectID, task.ID, "worker_idle", task.Launches)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if !exists {
				notice, err := createNoticeWithData(ctx, db, task.ProjectID, task.ID, "worker_idle", task.Title+" is idle without a Signal", fmt.Sprintf(`{"launch":%d}`, task.Launches), now)
				if err != nil {
					failures = append(failures, err)
				} else {
					notices = append(notices, notice)
				}
			}
		}
		if task.State == store.StateWorking && pane.AgentStatus == "blocked" {
			if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateBlocked, "herdr", "Herdr reports Rider blocked"); err != nil {
				if errors.Is(err, store.ErrStateRace) {
					continue
				}
				failures = append(failures, err)
				continue
			}
			notice, err := createNotice(ctx, db, project.ID, task.ID, "worker_blocked", task.Title+" is blocked", now)
			if err != nil {
				failures = append(failures, err)
			} else {
				notices = append(notices, notice)
			}
		} else if task.State == store.StateBlocked && pane.AgentStatus != "blocked" && agentPresent {
			if err := db.Transition(ctx, task.ID, store.StateBlocked, store.StateWorking, "herdr", "Herdr reports Rider unblocked"); err != nil {
				if errors.Is(err, store.ErrStateRace) {
					continue
				}
				failures = append(failures, err)
			}
		}
	}
	if project.LeadPaneID != "" || project.LeadLabel != "" {
		pane, found := findPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel)
		if !found {
			absentSince := project.LeadAbsentSince
			if absentSince == 0 {
				absentSince = now.UnixMilli()
			}
			if err := db.UpdateProjectObservation(ctx, project.ID, project.LeadPaneID, project.HerdrWorkspaceID, absentSince); err != nil {
				failures = append(failures, err)
			}
		} else {
			absentSince := project.LeadAbsentSince
			if pane.Agent == "" {
				if absentSince == 0 {
					absentSince = now.UnixMilli()
				}
			} else {
				absentSince = 0
			}
			if err := db.UpdateProjectObservation(ctx, project.ID, pane.PaneID, pane.WorkspaceID, absentSince); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return notices, errors.Join(failures...)
}

func hasRecordedSession(session string) bool {
	return sessionref.Sanitize("", session) != ""
}

func findPane(panes []herdr.Pane, paneID, label string) (herdr.Pane, bool) {
	return herdr.FindPane(panes, paneID, label)
}

func markLost(ctx context.Context, db *store.DB, task store.Task, reason string, now time.Time, notices *[]store.Notice) error {
	if err := db.Transition(ctx, task.ID, task.State, store.StateLost, "cli", reason); err != nil {
		return err
	}
	notice, err := createNotice(ctx, db, task.ProjectID, task.ID, "task_lost", task.Title+" lost its Rider", now)
	if err != nil {
		return err
	}
	*notices = append(*notices, notice)
	return nil
}

func isStateRace(err error) bool {
	return errors.Is(err, store.ErrStateRace) || (err != nil && strings.Contains(err.Error(), "changed concurrently"))
}

func createNotice(ctx context.Context, db *store.DB, projectID, taskID int64, kind, summary string, now time.Time) (store.Notice, error) {
	return createNoticeWithData(ctx, db, projectID, taskID, kind, summary, "{}", now)
}

func createNoticeWithData(ctx context.Context, db *store.DB, projectID, taskID int64, kind, summary, dataJSON string, now time.Time) (store.Notice, error) {
	notice := store.Notice{ProjectID: projectID, TaskID: taskID, Kind: kind, Summary: summary, DataJSON: dataJSON, CreatedAt: now.UnixMilli()}
	id, err := db.CreateNotice(ctx, notice)
	if err != nil {
		return store.Notice{}, err
	}
	notice.ID = id
	return notice, nil
}

func taskID(seq int) string { return fmt.Sprintf("t%d", seq) }
