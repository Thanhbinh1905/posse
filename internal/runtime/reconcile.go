package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/heavy"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/sessionref"
	"github.com/thanhbinh1905/posse/internal/store"
)

const ReconcileBudget = store.ObservationWriteBudget
const AgentAbsentGrace = 2 * time.Minute

func latestInterruptedStall(ctx context.Context, db *store.DB, taskID int64) (bool, error) {
	transitions, err := db.TaskTransitions(ctx, taskID, 1)
	if err != nil || len(transitions) == 0 {
		return false, err
	}
	latest := transitions[0]
	return latest.From == string(store.StateWorking) && latest.To == string(store.StateStalled) && latest.Note == store.WorkerInterruptedTransitionNote, nil
}

type herdrAdapterKey struct{}

type RunResult struct {
	Snapshot           herdr.Snapshot
	Notices            []store.Notice
	GenerationMismatch bool
}

type ProgressSource interface {
	ReadScreen(context.Context, herdr.Adapter, string) (string, error)
	WorktreeFingerprint(context.Context, string, bool) (string, error)
}

type SystemProgress struct{}

func Run(ctx context.Context, db *store.DB, adapter herdr.Adapter, projectID int64, stallAfter, idleAfter time.Duration, now time.Time, progress ProgressSource, usageRecorders ...func(context.Context, store.Task) error) (result RunResult, returnedErr error) {
	budgetCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
	defer cancel()
	defer func() { returnedErr = reconcileError(returnedErr) }()
	baselines := captureTurnEndBaselines(budgetCtx, db, projectID)
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
	var usageRecorder func(context.Context, store.Task) error
	if len(usageRecorders) > 0 {
		usageRecorder = usageRecorders[0]
	}
	notices, err := reconcileSnapshot(withTurnEndBaselines(context.WithValue(budgetCtx, herdrAdapterKey{}, adapter), baselines), db, projectID, snapshot, now, idleAfter, usageRecorder)
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
		idle, err := raiseIdleSlotNotice(budgetCtx, db, projectID, stallAfter, now)
		result.Notices = append(result.Notices, idle...)
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

func ReconcileSnapshot(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot, now time.Time, idleAfterValues ...time.Duration) ([]store.Notice, error) {
	idleAfter := time.Duration(0)
	if len(idleAfterValues) > 0 {
		idleAfter = idleAfterValues[0]
	}
	return reconcileSnapshot(ctx, db, projectID, snapshot, now, idleAfter, nil)
}

func reconcileSnapshot(ctx context.Context, db *store.DB, projectID int64, snapshot herdr.Snapshot, now time.Time, idleAfter time.Duration, usageRecorder func(context.Context, store.Task) error) (notices []store.Notice, returnedErr error) {
	defer func() { returnedErr = reconcileError(returnedErr) }()
	ctx, cancelBudget := context.WithTimeout(ctx, ReconcileBudget)
	defer cancelBudget()
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
		taskNotices, err := reconcileTask(taskCtx, db, project, task, snapshot, now, idleAfter, usageRecorder)
		cancel()
		notices = append(notices, taskNotices...)
		if err != nil && !isStateRace(err) {
			failures = append(failures, fmt.Errorf("reconcile Task %s: %w", taskID(task.Seq), err))
		}
	}
	// Task observation writes refresh only Task snapshots. Repair the Project
	// snapshot once per pass, in case an earlier write committed without it.
	if err := reconcileWorkDeferred(ctx); err != nil {
		return notices, errors.Join(append(failures, err)...)
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		failures = append(failures, fmt.Errorf("persist Project snapshot: %w", err))
	}
	return notices, errors.Join(failures...)
}

func reconcileError(err error) error {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		mapped := make([]error, len(causes))
		for index, cause := range causes {
			mapped[index] = reconcileError(cause)
		}
		return errors.Join(mapped...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		prefix := strings.TrimSuffix(err.Error(), cause.Error())
		return fmt.Errorf("%s%w", prefix, reconcileError(cause))
	}
	return fmt.Errorf("%w: reconciliation work deferred: %v", store.ErrBusy, err)
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

func taskTeardownInProgress(ctx context.Context, db *store.DB, task store.Task) (bool, error) {
	intent, err := db.IntentByTask(ctx, task.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return intent.Command == "teardown", err
}

func executorStoppedIntentionally(ctx context.Context, db *store.DB, task store.Task) (bool, error) {
	operation, found, err := db.LatestTaskExecutorPolicy(ctx, task.ID)
	if err != nil || !found {
		return false, err
	}
	if operation.Subject != "suspend" && operation.Subject != "release" {
		return false, nil
	}
	if operation.Outcome == "pending" || operation.Outcome == "failed" {
		intent, err := db.IntentByTask(ctx, task.ID)
		if store.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		step := strings.TrimPrefix(intent.Step, "resuming:")
		if intent.Command != "pr-release" || step != "in_progress:executor.stop" && step != "done:executor.stop" && step != "in_progress:sandbox.release" && step != "done:sandbox.release" {
			return false, nil
		}
	} else if operation.Outcome != "succeeded" {
		return false, nil
	}
	if operation.Baseline != "" {
		return operation.Baseline == strconv.Itoa(task.Launches), nil
	}
	return task.Launches <= 1, nil
}

func reconcileTask(ctx context.Context, db *store.DB, project store.Project, task store.Task, snapshot herdr.Snapshot, now time.Time, idleAfter time.Duration, usageRecorder func(context.Context, store.Task) error) ([]store.Notice, error) {
	var notices []store.Notice
	waitingHeavy := heavy.IsWaitingTask(filepath.Dir(db.Path), project.Name, taskID(task.Seq), task.ContainmentMarker)
	if task.State == store.StateSpawning && task.PaneID == "" {
		return nil, nil
	}
	observationFailure := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("update observation: %w", err)
	}
	pane, found := findPane(snapshot.Panes, task.PaneID, task.PaneLabel)
	status := ""
	if found && pane.Agent != "" {
		status = pane.AgentStatus
	}
	current := func() (string, error) { return repositoryFingerprint(ctx, task.WorktreePath) }
	fingerprints := store.RepositoryFingerprints{TurnEnd: turnEndFingerprint(ctx, task, current), Current: current}
	resumed, err := db.ObserveCompletedWorkerState(ctx, task, pane.PaneID, snapshot.ServerStartedAt, status, fingerprints, now)
	if err != nil {
		return nil, observationFailure(err)
	}
	if resumed != nil {
		notices = append(notices, *resumed)
		task.State = store.StateWorking
		task.IdleSince = 0
	}
	if !found {
		if usageRecorder != nil {
			if err := usageRecorder(ctx, task); err != nil {
				return nil, err
			}
		}
		if task.State == store.StateDone || task.State == store.StateMerging {
			teardownInProgress, err := taskTeardownInProgress(ctx, db, task)
			if err != nil || teardownInProgress {
				return nil, err
			}
			stoppedIntentionally, err := executorStoppedIntentionally(ctx, db, task)
			if err != nil || stoppedIntentionally {
				return nil, err
			}
			return nil, db.CreateWorkerExitedNotice(ctx, task, now)
		}
		groupedWorker := !project.IsWorkspace() && project.HerdrWorkspaceID != "" && task.HerdrWorkspaceID != ""
		restartPending := task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt
		absentSince := task.AgentAbsentSince
		if absentSince == 0 {
			absentSince = now.UnixMilli()
		}
		if (!groupedWorker && !restartPending) || now.Sub(time.UnixMilli(absentSince)) >= AgentAbsentGrace {
			err := markLost(ctx, db, task, "Worker pane and label are absent from the Herdr snapshot", now, &notices)
			return notices, err
		}
		observation := task.Observation()
		observation.AgentSession = sessionref.Sanitize("", task.AgentSession)
		observation.AgentAbsentSince, observation.IdleSince = absentSince, 0
		observation.AgentStatus, observation.AgentStatusSince = store.AgentStatusNoPane, agentStatusSince(task, store.AgentStatusNoPane, now)
		return nil, observationFailure(db.UpdateTaskObservation(ctx, task.ID, observation))
	}
	absentSince := task.AgentAbsentSince
	agentPresent := pane.Agent != ""
	session := task.AgentSession
	sessionKind := task.AgentSessionKind
	// A session without an agent name has no safe owner kind to persist.
	if agentPresent {
		session = sessionref.Sanitize(pane.Agent, task.AgentSession)
		if len(pane.AgentSession) > 0 && string(pane.AgentSession) != "null" {
			if reported := sessionref.Sanitize(pane.Agent, string(pane.AgentSession)); reported != "" {
				session = reported
				sessionKind = sessionref.NormalizeKind(pane.Agent)
			}
		}
	}
	if !agentPresent {
		if absentSince == 0 {
			absentSince = now.UnixMilli()
		}
		resumePending := hasRecordedSession(task.AgentSession) && task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt
		if !resumePending && now.Sub(time.UnixMilli(absentSince)) >= AgentAbsentGrace {
			if task.State == store.StateDone || task.State == store.StateMerging {
				teardownInProgress, err := taskTeardownInProgress(ctx, db, task)
				if err != nil || teardownInProgress {
					return nil, err
				}
				stoppedIntentionally, err := executorStoppedIntentionally(ctx, db, task)
				if err != nil || stoppedIntentionally {
					return nil, err
				}
				return nil, db.CreateWorkerExitedNotice(ctx, task, now)
			}
			err := markLost(ctx, db, task, "Worker has been absent for two minutes", now, &notices)
			return notices, err
		}
	} else {
		absentSince = 0
	}
	serverStartedAt := task.AgentServerStartedAt
	if agentPresent && snapshot.ServerStartedAt != "" && (serverStartedAt == "" || serverStartedAt == snapshot.ServerStartedAt || pane.AgentStatus == "working") {
		serverStartedAt = snapshot.ServerStartedAt
	}
	idleSince := task.IdleSince
	if task.State == store.StateWorking && !waitingHeavy && agentPresent && (pane.AgentStatus == "idle" || pane.AgentStatus == "done") {
		if idleSince == 0 {
			idleSince = now.UnixMilli()
		}
	} else {
		idleSince = 0
	}
	observedStatus := store.AgentStatusNoAgent
	if agentPresent {
		observedStatus = pane.AgentStatus
		if observedStatus == "" {
			observedStatus = "unknown"
		}
	}
	observation := store.TaskObservation{
		PaneID: pane.PaneID, WorkspaceID: pane.WorkspaceID, AgentSession: session, AgentSessionKind: sessionKind,
		AgentAbsentSince: absentSince, IdleSince: idleSince, AgentServerStartedAt: serverStartedAt,
	}
	observation.AgentStatus, observation.AgentStatusSince = observedStatus, agentStatusSince(task, observedStatus, now)
	if usageRecorder != nil {
		observedTask := task
		observedTask.AgentSession, observedTask.AgentSessionKind = observation.AgentSession, observation.AgentSessionKind
		if err := usageRecorder(ctx, observedTask); err != nil {
			return notices, err
		}
	}
	if err := db.UpdateTaskObservation(ctx, task.ID, observation); err != nil {
		return nil, observationFailure(err)
	}
	if task.State == store.StateWorking && task.AgentStatus == "working" && task.AgentServerStartedAt != "" && snapshot.ServerStartedAt != "" && task.AgentServerStartedAt != snapshot.ServerStartedAt && (pane.AgentStatus == "idle" || pane.AgentStatus == "done") {
		notice, err := db.InterruptWorker(ctx, task, snapshot.ServerStartedAt, now)
		if err != nil {
			return notices, err
		}
		notices = append(notices, notice)
		task.State = store.StateStalled
	}
	if task.State == store.StateStalled {
		interrupted, err := latestInterruptedStall(ctx, db, task.ID)
		if err != nil {
			return notices, err
		}
		if interrupted {
			if err := db.EnsureWorkerInterruptedNotice(ctx, task, snapshot.ServerStartedAt, now); err != nil {
				return notices, err
			}
			if pane.AgentStatus == "working" {
				if err := db.Transition(ctx, task.ID, store.StateStalled, store.StateWorking, "cli", "Worker resumed after its interrupted turn"); err != nil {
					return notices, err
				}
			}
		}
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
	if task.State == store.StateWorking && !waitingHeavy && idleAfter > 0 && idleSince > 0 && now.Sub(time.UnixMilli(idleSince)) >= idleAfter {
		exists, err := db.HasWorkerIdleNotice(ctx, task.ProjectID, task.ID, task.Launches, idleSince)
		if err != nil {
			return nil, err
		}
		modelErrorHandled := episode.Launch == task.Launches && episode.Status != "" && episode.Status != "resolved" && !store.ModelErrorAwaitingTurn(episode) && (episode.Status != "active" || episode.NextAttemptAt > 0)
		quotaHandled := false
		if !exists && !modelErrorHandled {
			// A quota Notice already explains why this launch went idle.
			_, err := db.TaskQuotaExhaustion(ctx, task.ID, task.Launches)
			if err != nil && !store.IsNotFound(err) {
				return nil, err
			}
			quotaHandled = err == nil
		}
		if !exists && !modelErrorHandled && !quotaHandled {
			summary := task.Title + " is idle without a Signal"
			failure, err := codexIdleFailure(ctx, db, task, pane, now)
			if err != nil {
				return notices, err
			}
			if failure != "" {
				summary = failure
			}
			notice, err := createNoticeWithData(ctx, db, task.ProjectID, task.ID, "worker_idle", summary, fmt.Sprintf(`{"launch":%d,"idle_since":%d}`, task.Launches, idleSince), now)
			if err != nil {
				return nil, err
			}
			notices = append(notices, notice)
		}
	}
	if task.State == store.StateWorking && pane.AgentStatus == "blocked" {
		if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateBlocked, "herdr", "Herdr reports Worker blocked"); err != nil {
			return notices, err
		}
		summary, data := task.Title+" is blocked", "{}"
		if dialog := blockedDialogFromContext(ctx, pane.PaneID); dialog != "" {
			encoded, _ := json.Marshal(map[string]string{"dialog": dialog})
			summary, data = summary+" on:\n"+dialog, string(encoded)
		}
		notice, err := createNoticeWithData(ctx, db, project.ID, task.ID, "worker_blocked", summary, data, now)
		if err != nil {
			return notices, err
		}
		notices = append(notices, notice)
	} else if task.State == store.StateBlocked && pane.AgentStatus != "blocked" && agentPresent {
		if err := db.Transition(ctx, task.ID, store.StateBlocked, store.StateWorking, "herdr", "Herdr reports Worker unblocked"); err != nil {
			return notices, err
		}
	}
	return notices, nil
}

const blockedDialogLines = 15

func blockedDialogFromContext(ctx context.Context, paneID string) string {
	adapter, _ := ctx.Value(herdrAdapterKey{}).(herdr.Adapter)
	if adapter == nil {
		return ""
	}
	return BlockedDialog(ctx, adapter, paneID)
}

// BlockedDialog returns the last lines of the blocked pane's screen, which
// hold the dialog prompt and the command it asks about. A failed read
// yields no text so the Notice still reports the block.
func BlockedDialog(ctx context.Context, adapter herdr.Adapter, paneID string) string {
	screen, err := (SystemProgress{}).ReadScreen(ctx, adapter, paneID)
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(screen, "\n") {
		if line = strings.TrimRight(line, " \t\r"); strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > blockedDialogLines {
		lines = lines[len(lines)-blockedDialogLines:]
	}
	return strings.Join(lines, "\n")
}

// agentStatusSince keeps the time Posse first observed status while it holds.
// Herdr idle and done both mean the agent ended its turn, so a flip between
// them continues the same observation.
func agentStatusSince(task store.Task, status string, now time.Time) int64 {
	turnEnded := func(value string) bool { return value == "idle" || value == "done" }
	if task.AgentStatusSince > 0 && (task.AgentStatus == status || turnEnded(task.AgentStatus) && turnEnded(status)) {
		return task.AgentStatusSince
	}
	return now.UnixMilli()
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
	return db.UpdateProjectObservation(ctx, project, paneID, workspaceID, absentSince)
}

func hasRecordedSession(session string) bool {
	return sessionref.Sanitize("", session) != ""
}

func findPane(panes []herdr.Pane, paneID, label string) (herdr.Pane, bool) {
	return herdr.FindPane(panes, paneID, label)
}

func markLost(ctx context.Context, db *store.DB, task store.Task, reason string, now time.Time, notices *[]store.Notice) error {
	transitioned, err := db.TransitionLostUnlessTeardownIntent(ctx, task.ID, task.State, "cli", reason)
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}
	notice, err := createNotice(ctx, db, task.ProjectID, task.ID, "task_lost", task.Title+" lost its Worker", now)
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
