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

type observationWriteError struct {
	description string
	err         error
}

func (e *observationWriteError) Error() string {
	return e.description + ": " + e.err.Error()
}

func (e *observationWriteError) Unwrap() error { return e.err }

// IsObservationContention reports whether err contains only retryable
// observation-write contention. A Rider Signal may proceed when unrelated
// Project observations are blocked, but must not hide other reconcile failures.
func IsObservationContention(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		failures := joined.Unwrap()
		if len(failures) == 0 {
			return false
		}
		for _, failure := range failures {
			if !IsObservationContention(failure) {
				return false
			}
		}
		return true
	}
	if observationErr, ok := err.(*observationWriteError); ok {
		return store.IsBusy(observationErr.err)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsObservationContention(wrapped.Unwrap())
	}
	return false
}

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
		writeCtx, cancelWrite := context.WithTimeout(ctx, ReconcileBudget)
		err := db.RememberProjectServerStartedAt(writeCtx, projectID, snapshot.ServerStartedAt)
		cancelWrite()
		if err != nil {
			returnedErr = errors.Join(returnedErr, &observationWriteError{description: "remember Project server observation", err: err})
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
	cancel()
	notices, err := ReconcileSnapshot(ctx, db, projectID, snapshot, now, idleAfter)
	result.Notices = append(result.Notices, notices...)
	if err != nil {
		return result, err
	}
	if stallAfter > 0 {
		if progress == nil {
			progress = SystemProgress{}
		}
		stallCtx, cancelStalls := context.WithTimeout(ctx, ReconcileBudget)
		stalled, err := evaluateStallsSnapshot(stallCtx, db, adapter, projectID, snapshot, stallAfter, now, progress)
		cancelStalls()
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
		// Never derive a Task's budget from another Task's expiring context.
		taskCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
		taskNotices, err := reconcileTask(taskCtx, db, project, task, snapshot, now, idleAfter)
		cancel()
		notices = append(notices, taskNotices...)
		if err != nil && !isStateRace(err) {
			failures = append(failures, err)
		}
	}
	if project.LeadPaneID != "" || project.LeadLabel != "" {
		projectCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
		err := reconcileLeadObservation(projectCtx, db, project, snapshot, now)
		cancel()
		if err != nil {
			failures = append(failures, &observationWriteError{description: "update Project observation", err: err})
		}
	}
	return notices, errors.Join(failures...)
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
		return &observationWriteError{description: fmt.Sprintf("update Task %s observation", taskID(task.Seq)), err: err}
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
	if task.State == store.StateWorking && idleAfter > 0 && idleSince > 0 && now.Sub(time.UnixMilli(idleSince)) >= idleAfter {
		exists, err := db.HasNotice(ctx, task.ProjectID, task.ID, "worker_idle", task.Launches)
		if err != nil {
			return nil, err
		}
		if !exists {
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
