package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) repairLookout(ctx *axi.Context, db *store.DB, home string, project store.Project) error {
	actor := s.configCaller(ctx.Context, db)
	switch actor.role {
	case "worker", "agent":
		return axi.Failure("user_only", "only the User can repair a Project Lookout", false)
	case "lead":
		if s.recordedLeadIsLive(ctx.Context, actor.paneID) {
			return axi.Failure("user_only", "only the User can repair a Project Lookout", false, "Run `posse lookout --repair` from the Project folder outside the live Lead pane")
		}
	}
	if cwdInsideMount(ctx.Context, db) {
		return axi.Failure("user_only", "Lookout repair cannot run from inside a Rider Mount", false, "Run `posse lookout --repair` from the Project folder")
	}
	if project.IsDown() {
		return axi.Failure("project_down", "the Project is stopped; Lookout repair will not restart it", false, "Run `posse up` only when you intend to restart the Lead and recover Riders")
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
		return axi.Failure("lookout_repair_failed", "could not verify the existing Lead workspace; poller was not changed", true, err.Error(), "Restore Herdr, then retry `posse lookout --repair`")
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found || workspaceID != project.HerdrWorkspaceID {
		return axi.Failure("lead_workspace_missing", "the recorded Lead workspace is not open; Lookout repair will not restart the Lead", false, "Restore the Lead workspace, then retry `posse lookout --repair`")
	}
	before, err := db.LookoutPoller(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	beforeCheck, err := s.lookoutPollerHealth(ctx.Context, db, home, project)
	if err != nil {
		return err
	}
	if err := s.ensureLookoutPoller(ctx.Context, db, home, project); err != nil {
		return axi.Failure("lookout_repair_failed", "could not restore the Project poller", true, err.Error(), "Check process permissions and retry `posse lookout --repair`")
	}
	state, check, err := s.waitLookoutPollerVerification(ctx.Context, db, home, project)
	if err != nil {
		return axi.Failure("lookout_repair_failed", "could not verify the restored Project poller", true, err.Error(), "Check process permissions and retry `posse lookout --repair`")
	}
	if check.Status != "ok" {
		return axi.Failure("lookout_repair_failed", "the Project poller did not become healthy", true, check.Detail, "Inspect the Lookout log at "+lookoutPollerLogPath(home, project))
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "pid", Value: state.PID},
		{Key: "state", Value: "running"},
		{Key: "repaired", Value: beforeCheck.Status != "ok" || before.PID != state.PID},
		{Key: "heartbeat_at", Value: time.UnixMilli(state.HeartbeatAt).UTC().Format(time.RFC3339)},
		{Key: "log", Value: lookoutPollerLogPath(home, project)},
		{Key: "help", Value: []any{"The Project poller observes PRs, CI, checkout state and landed-Task Teardown; it never claims Notices"}},
	})
}

func (s *Service) waitLookoutPollerVerification(ctx context.Context, db *store.DB, home string, project store.Project) (store.LookoutPoller, doctorCheck, error) {
	deadline := time.Now().Add(lookoutStartupGracePeriod)
	var state store.LookoutPoller
	var check doctorCheck
	for {
		var err error
		state, err = db.LookoutPoller(ctx, project.ID)
		if err != nil {
			return state, doctorCheck{}, err
		}
		check, err = s.lookoutPollerHealth(ctx, db, home, project)
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

func (s *Service) lookoutPollerHealth(ctx context.Context, db *store.DB, home string, project store.Project) (doctorCheck, error) {
	state, err := db.LookoutPoller(ctx, project.ID)
	if err != nil {
		return doctorCheck{}, err
	}
	if project.IsDown() {
		return doctorCheck{Name: "Project Lookout " + project.Name, Status: "ok", Detail: "Project is down; no poller", Action: ""}, nil
	}
	if project.HerdrWorkspaceID == "" {
		return doctorCheck{Name: "Project Lookout " + project.Name, Status: "ok", Detail: "Lead not started; no poller", Action: ""}, nil
	}
	processes, processErr := systemLookoutProcesses(home)
	if processErr != nil {
		return doctorCheck{Name: "Project Lookout " + project.Name, Status: "warn", Detail: "poller process state unavailable: " + processErr.Error(), Action: "Check process-table access and run `posse lookout --repair`"}, nil
	}
	var found *lookoutProcess
	for index := range processes {
		process := &processes[index]
		if process.PollOnly && process.PollerProjectID == project.ID && process.PID == state.PID && process.PollerToken == state.Token && process.PollerStartedAt == state.StartedAt {
			found = process
			break
		}
	}
	detail := "poller=missing"
	if state.PID > 0 {
		detail = fmt.Sprintf("pid=%d started_at=%s heartbeat_at=%s executable=%s (%d:%d)", state.PID, time.UnixMilli(state.StartedAt).UTC().Format(time.RFC3339), time.UnixMilli(state.HeartbeatAt).UTC().Format(time.RFC3339), state.ExecutablePath, state.ExecutableDevice, state.ExecutableInode)
	}
	healthy := state.PID > 0 && state.Token != "" && found != nil && lookoutPIDRunning(state.PID) && time.Since(time.UnixMilli(state.HeartbeatAt)) <= lookoutPollerStaleAfter
	if found != nil && (found.Executable.Device != state.ExecutableDevice || found.Executable.Inode != state.ExecutableInode) {
		detail += "; executable changed, re-exec pending"
	}
	if found == nil && state.PID > 0 {
		detail += "; matching process not found"
	}
	if time.Since(time.UnixMilli(state.HeartbeatAt)) > lookoutPollerStaleAfter {
		detail += "; heartbeat stale"
	}
	detail += "; log=" + lookoutPollerLogPath(home, project)
	status, action := "ok", ""
	if !healthy {
		status = "warn"
		action = "Run `posse lookout --repair` or another Project command to repair the poller"
	}
	return doctorCheck{Name: "Project Lookout " + project.Name, Status: status, Detail: detail, Action: action}, nil
}

func (s *Service) lookoutPollerRow(ctx context.Context, db *store.DB, home string, project store.Project) map[string]any {
	status := "missing"
	state, err := db.LookoutPoller(ctx, project.ID)
	if project.IsDown() {
		status = "stopped"
	} else if project.HerdrWorkspaceID == "" {
		status = "not_started"
	} else if err != nil {
		status = "unavailable"
	} else if check, checkErr := s.lookoutPollerHealth(ctx, db, home, project); checkErr != nil {
		status = "unavailable"
	} else if check.Status == "ok" {
		status = "running"
	} else {
		status = "unhealthy"
	}
	row := map[string]any{"state": status, "log": lookoutPollerLogPath(home, project)}
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

func (s *Service) lookoutPollerSummary(ctx context.Context, db *store.DB, home string, project store.Project) string {
	row := s.lookoutPollerRow(ctx, db, home, project)
	status, _ := row["state"].(string)
	if pid, ok := row["pid"]; ok {
		return fmt.Sprintf("%s (pid=%v, heartbeat=%v)", status, pid, row["heartbeat_at"])
	}
	return status
}
