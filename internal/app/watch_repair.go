package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) repairWatch(ctx *axi.Context, db *store.DB, home string, project store.Project) error {
	actor := s.configCaller(ctx.Context, db)
	switch actor.role {
	case "worker", "agent":
		return axi.Failure("user_only", "only the User can repair a Project Watch", false)
	case "lead":
		if s.recordedLeadIsLive(ctx.Context, actor.paneID) {
			return axi.Failure("user_only", "only the User can repair a Project Watch", false, "Run `posse watch --repair` from the Project folder outside the live Lead pane")
		}
	}
	if cwdInsideSandbox(ctx.Context, db) {
		return axi.Failure("user_only", "Watch repair cannot run from inside a Worker Sandbox", false, "Run `posse watch --repair` from the Project folder")
	}
	if project.IsDown() {
		return axi.Failure("project_down", "the Project is stopped; Watch repair will not restart it", false, "Run `posse up` only when you intend to restart the Lead and recover Workers")
	}
	if project.HerdrWorkspaceID == "" {
		return axi.Failure("lead_workspace_missing", "the Project has no active Lead workspace to supervise", false, "Run `posse up` from the Project folder to start its Lead")
	}
	if _, err := os.Stat(project.Root); err != nil {
		return axi.Failure("project_missing", fmt.Sprintf("Project %s path is unavailable: %s", project.Name, project.Root), false, "Run `posse project move` to update the Project path")
	}
	if s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true, "Run `posse doctor` to inspect setup")
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return axi.Failure("watch_repair_failed", "could not verify the existing Lead workspace; watch was not changed", true, err.Error(), "Restore Herdr, then retry `posse watch --repair`")
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found || workspaceID != project.HerdrWorkspaceID {
		return axi.Failure("lead_workspace_missing", "the recorded Lead workspace is not open; Watch repair will not restart the Lead", false, "Restore the Lead workspace, then retry `posse watch --repair`")
	}
	before, err := db.WatchRuntime(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	beforeCheck, err := s.watchRuntimeHealth(ctx.Context, db, home, project)
	if err != nil {
		return err
	}
	if err := s.ensureWatchRuntime(ctx.Context, db, home, project); err != nil {
		return axi.Failure("watch_repair_failed", "could not restore the Project watch", true, err.Error(), "Check process permissions and retry `posse watch --repair`")
	}
	state, check, err := s.waitWatchRuntimeVerification(ctx.Context, db, home, project)
	if err != nil {
		return axi.Failure("watch_repair_failed", "could not verify the restored Project watch", true, err.Error(), "Check process permissions and retry `posse watch --repair`")
	}
	if check.Status != "ok" {
		return axi.Failure("watch_repair_failed", "the Project watch did not become healthy", true, check.Detail, "Inspect the Watch log at "+watchRuntimeLogPath(home, project))
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "pid", Value: state.PID},
		{Key: "state", Value: "running"},
		{Key: "repaired", Value: beforeCheck.Status != "ok" || before.PID != state.PID},
		{Key: "heartbeat_at", Value: time.UnixMilli(state.HeartbeatAt).UTC().Format(time.RFC3339)},
		{Key: "log", Value: watchRuntimeLogPath(home, project)},
		{Key: "help", Value: []any{"The Project watch observes PRs, CI, checkout state and merged-Task Teardown; it never claims Notices"}},
	})
}

func (s *Service) waitWatchRuntimeVerification(ctx context.Context, db *store.DB, home string, project store.Project) (store.WatchRuntime, doctorCheck, error) {
	deadline := time.Now().Add(watchStartupGracePeriod)
	var state store.WatchRuntime
	var check doctorCheck
	for {
		var err error
		state, err = db.WatchRuntime(ctx, project.ID)
		if err != nil {
			return state, doctorCheck{}, err
		}
		check, err = s.watchRuntimeHealth(ctx, db, home, project)
		if err != nil {
			return state, check, err
		}
		if check.Status == "ok" || !time.Now().Before(deadline) {
			return state, check, nil
		}
		select {
		case <-ctx.Done():
			return state, check, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Service) watchRuntimeHealth(ctx context.Context, db *store.DB, home string, project store.Project) (doctorCheck, error) {
	state, err := db.WatchRuntime(ctx, project.ID)
	if err != nil {
		return doctorCheck{}, err
	}
	if project.IsDown() {
		return doctorCheck{Name: "Project Watch " + project.Name, Status: "ok", Detail: "Project is down; no watch", Action: ""}, nil
	}
	if project.HerdrWorkspaceID == "" {
		return doctorCheck{Name: "Project Watch " + project.Name, Status: "ok", Detail: "Lead not started; no watch", Action: ""}, nil
	}
	processes, processErr := systemWatchProcesses(home)
	if processErr != nil {
		return doctorCheck{Name: "Project Watch " + project.Name, Status: "warn", Detail: "watch process state unavailable: " + processErr.Error(), Action: "Check process-table access and run `posse watch --repair`"}, nil
	}
	var found *watchProcess
	for index := range processes {
		process := &processes[index]
		if process.PollOnly && process.WatchProjectID == project.ID && process.PID == state.PID && process.WatchToken == state.Token && process.WatchStartedAt == state.StartedAt {
			found = process
			break
		}
	}
	detail := "watch=missing"
	if state.PID > 0 {
		detail = fmt.Sprintf("pid=%d started_at=%s heartbeat_at=%s executable=%s (%d:%d)", state.PID, time.UnixMilli(state.StartedAt).UTC().Format(time.RFC3339), time.UnixMilli(state.HeartbeatAt).UTC().Format(time.RFC3339), state.ExecutablePath, state.ExecutableDevice, state.ExecutableInode)
	}
	healthy := state.PID > 0 && state.Token != "" && found != nil && watchPIDRunning(state.PID) && time.Since(time.UnixMilli(state.HeartbeatAt)) <= watchRuntimeStaleAfter
	if found != nil && (found.Executable.Device != state.ExecutableDevice || found.Executable.Inode != state.ExecutableInode) {
		detail += "; executable changed, re-exec pending"
	}
	if found == nil && state.PID > 0 {
		detail += "; matching process not found"
	}
	if time.Since(time.UnixMilli(state.HeartbeatAt)) > watchRuntimeStaleAfter {
		detail += "; heartbeat stale"
	}
	detail += "; log=" + watchRuntimeLogPath(home, project)
	status, action := "ok", ""
	if !healthy {
		status = "warn"
		action = "Run `posse watch --repair` or another Project command to repair the watch"
	}
	return doctorCheck{Name: "Project Watch " + project.Name, Status: status, Detail: detail, Action: action}, nil
}

func (s *Service) watchRuntimeRow(ctx context.Context, db *store.DB, home string, project store.Project) map[string]any {
	status := "missing"
	state, err := db.WatchRuntime(ctx, project.ID)
	if project.IsDown() {
		status = "stopped"
	} else if project.HerdrWorkspaceID == "" {
		status = "not_started"
	} else if err != nil {
		status = "unavailable"
	} else if check, checkErr := s.watchRuntimeHealth(ctx, db, home, project); checkErr != nil {
		status = "unavailable"
	} else if check.Status == "ok" {
		status = "running"
	} else {
		status = "unhealthy"
	}
	row := map[string]any{"state": status, "log": watchRuntimeLogPath(home, project)}
	if err != nil {
		row["error"] = err.Error()
		return row
	}
	if state.PID > 0 {
		row["pid"] = state.PID
		row["started_at"] = time.UnixMilli(state.StartedAt).UTC().Format(time.RFC3339)
		row["heartbeat_at"] = time.UnixMilli(state.HeartbeatAt).UTC().Format(time.RFC3339)
		row["executable"] = state.ExecutablePath
		row["device"] = state.ExecutableDevice
		row["inode"] = state.ExecutableInode
	}
	return row
}

func (s *Service) watchRuntimeSummary(ctx context.Context, db *store.DB, home string, project store.Project) string {
	row := s.watchRuntimeRow(ctx, db, home, project)
	status, _ := row["state"].(string)
	if pid, ok := row["pid"]; ok {
		return fmt.Sprintf("%s (pid=%v, heartbeat=%v)", status, pid, row["heartbeat_at"])
	}
	return status
}
