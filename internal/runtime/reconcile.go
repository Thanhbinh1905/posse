package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

const ReconcileBudget = store.ObservationWriteBudget
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
	defer func() { returnedErr = reconcileError(returnedErr) }()
	snapshot, err := adapter.Snapshot(budgetCtx)
	if err != nil {
		return RunResult{}, err
	}
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
	if err := reconcileWorkDeferred(budgetCtx); err != nil {
		return result, err
	}
	if snapshot.ServerStartedAt != "" {
		writeCtx, cancelWrite := context.WithTimeout(budgetCtx, ReconcileBudget)
		err := db.RememberProjectServerStartedAt(writeCtx, projectID, snapshot.ServerStartedAt)
		cancelWrite()
		if err != nil {
			returnedErr = fmt.Errorf("remember Project server observation: %w", err)
		}
	}
	notices, err := ReconcileSnapshot(budgetCtx, db, projectID, snapshot, now, idleAfter)
	result.Notices = append(result.Notices, notices...)
	if err != nil {
		return result, errors.Join(returnedErr, err)
	}
	if stallAfter > 0 {
		if progress == nil {
			progress = SystemProgress{}
		}
		stalled, err := evaluateStallsSnapshot(budgetCtx, db, adapter, projectID, snapshot, stallAfter, now, progress)
		result.Notices = append(result.Notices, stalled...)
		if err != nil {
			return result, errors.Join(returnedErr, err)
		}
	}
	return result, returnedErr
}

func snapshotMatchesProjectGeneration(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot) (bool, error) {
	generation, err := db.ProjectServerStartedAt(ctx, projectID)
	return generation == "" || snapshot.ServerStartedAt == "" || generation == snapshot.ServerStartedAt, err
}

func ReconcileSnapshot(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot, now time.Time, idleAfterValues ...time.Duration) (notices []store.Notice, returnedErr error) {
	defer func() { returnedErr = reconcileError(returnedErr) }()
	ctx, cancelBudget := context.WithTimeout(ctx, ReconcileBudget)
	defer cancelBudget()
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
	var failures []error
	if project.LeadPaneID != "" || project.LeadLabel != "" {
		if err := reconcileWorkDeferred(ctx); err != nil {
			return notices, err
		}
		projectCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
		err := reconcileLeadObservation(projectCtx, db, project, snapshot, now)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("update Project observation: %w", err))
		}
	}
	// Prefer observations left stale by the previous bounded pass.
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].UpdatedAt < tasks[j].UpdatedAt })
	for _, task := range tasks {
		if err := reconcileWorkDeferred(ctx); err != nil {
			return notices, errors.Join(append(failures, err)...)
		}
		// Each admitted Task gets a fresh short context. Never start work with
		// an expired pass context, or give it a budget beyond the pass deadline.
		taskCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
		taskNotices, err := reconcileTask(taskCtx, db, project, task, snapshot, now, idleAfter)
		cancel()
		notices = append(notices, taskNotices...)
		if err != nil && !isStateRace(err) {
			failures = append(failures, err)
		}
	}
	return notices, errors.Join(failures...)
}

func reconcileError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: reconciliation work deferred: %v", store.ErrBusy, err)
	}
	return err
}

// Leave enough time for one SQLite lock wait and its cleanup. Work that does
// not fit is retried on the next pass rather than consuming an expired context.
func reconcileWorkDeferred(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("%w: reconciliation budget spent; remaining work deferred", store.ErrBusy)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < store.ObservationWriteBusyTimeout+25*time.Millisecond {
		return fmt.Errorf("%w: reconciliation budget spent; remaining work deferred", store.ErrBusy)
	}
	return nil
}

func reconcileTask(ctx context.Context, db *store.DB, project store.Project, task store.Task, snapshot herdr.Snapshot, now time.Time, idleAfter time.Duration) ([]store.Notice, error) {
	var notices []store.Notice
	if task.State == store.StateSpawning && task.PaneID == "" {
		return nil, nil
	}
	observationFailure := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("update Task %s observation: %w", taskID(task.Seq), err)
	}
	pane, found := findPane(snapshot.Panes, task.PaneID, task.PaneLabel)
	if !found {
		if task.State == store.StateDone || task.State == store.StateLanding {
			return nil, db.CreateWorkerExitedNotice(ctx, task, now)
		}
		groupedRider := !project.IsWorkspace() && project.HerdrWorkspaceID != "" && task.HerdrWorkspaceID != "" && task.HerdrWorkspaceID != project.HerdrWorkspaceID
		restartPending := task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt
		absentSince := task.AgentAbsentSince
		if absentSince == 0 {
			absentSince = now.UnixMilli()
		}
		if (!groupedRider && !restartPending) || now.Sub(time.UnixMilli(absentSince)) >= AgentAbsentGrace {
			err := markLost(ctx, db, task, "Rider pane and label are absent from the Herdr snapshot", now, &notices)
			return notices, err
		}
		return nil, observationFailure(db.UpdateTaskObservation(ctx, task.ID, task.PaneID, task.HerdrWorkspaceID, sessionref.Sanitize("", task.AgentSession), absentSince, 0, task.AgentServerStartedAt))
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
				return nil, db.CreateWorkerExitedNotice(ctx, task, now)
			}
			err := markLost(ctx, db, task, "Rider has been absent for two minutes", now, &notices)
			return notices, err
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
		return nil, observationFailure(err)
	}
	episode, err := db.TaskModelErrorEpisode(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if task.State == store.StateWorking && episode.Launch == task.Launches && episode.Status == "active" && store.ModelErrorNudgeExpired(episode, now) {
		notice := store.Notice{
			ProjectID: project.ID,
			TaskID:    task.ID,
			Kind:      "model_stream_error",
			Summary:   fmt.Sprintf("%s: a continue attempt was interrupted or its delivery is uncertain; Posse did not replay it (episode %d, %d/%d continue nudges)", task.Title, episode.Episode, episode.Attempts, 3),
			DataJSON:  fmt.Sprintf(`{"episode":%d,"launch":%d,"agent":%q,"kind":"model_stream_error","attempts":%d,"max_attempts":3,"interrupted":true}`, episode.Episode, episode.Launch, episode.Agent, episode.Attempts),
		}
		created, err := db.InterruptExpiredModelErrorNudge(ctx, task.ID, now.UnixMilli(), notice)
		if err != nil {
			return nil, err
		}
		if created != nil {
			notices = append(notices, *created)
		}
		episode, err = db.TaskModelErrorEpisode(ctx, task.ID)
		if err != nil {
			return nil, err
		}
	}
	if task.State == store.StateWorking && idleAfter > 0 && idleSince > 0 && now.Sub(time.UnixMilli(idleSince)) >= idleAfter {
		exists, err := db.HasNotice(ctx, task.ProjectID, task.ID, "worker_idle", task.Launches)
		if err != nil {
			return nil, err
		}
		modelErrorHandled := episode.Launch == task.Launches && episode.Status != "" && episode.Status != "resolved" && !store.ModelErrorAwaitingTurn(episode) && (episode.Status != "active" || episode.NextAttemptAt > 0)
		if !exists && !modelErrorHandled {
			notice, err := createNoticeWithData(ctx, db, task.ProjectID, task.ID, "worker_idle", task.Title+" is idle without a Signal", fmt.Sprintf(`{"launch":%d}`, task.Launches), now)
			if err != nil {
				return nil, err
			}
			notices = append(notices, notice)
		}
	}
	if task.State == store.StateWorking && pane.AgentStatus == "blocked" {
		if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateBlocked, "herdr", "Herdr reports Rider blocked"); err != nil {
			return notices, err
		}
		notice, err := createNotice(ctx, db, project.ID, task.ID, "worker_blocked", task.Title+" is blocked", now)
		if err != nil {
			return notices, err
		}
		notices = append(notices, notice)
	} else if task.State == store.StateBlocked && pane.AgentStatus != "blocked" && agentPresent {
		if err := db.Transition(ctx, task.ID, store.StateBlocked, store.StateWorking, "herdr", "Herdr reports Rider unblocked"); err != nil {
			return notices, err
		}
	}
	return notices, nil
}

func reconcileLeadObservation(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot, now time.Time) error {
	paneID, workspaceID := project.LeadPaneID, project.HerdrWorkspaceID
	absentSince := project.LeadAbsentSince
	pane, found := findPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel)
	if found {
		paneID, workspaceID = pane.PaneID, pane.WorkspaceID
	}
	if found && pane.Agent != "" {
		absentSince = 0
	} else if absentSince == 0 {
		absentSince = now.UnixMilli()
	}
	return db.UpdateProjectObservation(ctx, project.ID, paneID, workspaceID, absentSince)
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
