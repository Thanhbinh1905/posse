//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchRepairRestoresHeldProjectWithoutExecutorRecovery(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial poll-only process not running: %v", watchPIDs(f.root))
	}
	taskBefore := seedWatchRepairTask(t, f, client)

	configPath := filepath.Join(f.home, "config.toml")
	originalConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	heldConfig := strings.Replace(string(originalConfig), "auto_teardown = \"finished\"", "auto_teardown = \"finished\"\nauto_recover = false", 1)
	if heldConfig == string(originalConfig) {
		t.Fatal("fixture config has no defaults.auto_teardown line")
	}
	if err := os.WriteFile(configPath, []byte(heldConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	panes := watchPanes(t, client)
	if len(panes) != 1 {
		t.Fatalf("initial poll-only panes = %d, want 1", len(panes))
	}
	for _, pid := range watchPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("poll-only process survived SIGKILL: %v", watchPIDs(f.root))
	}
	if _, err := client.Call(context.Background(), "tab.close", map[string]any{"tab_id": panes[0].TabID}); err != nil {
		t.Fatalf("close missing-watcher pane: %v", err)
	}
	if len(watchPanes(t, client)) != 0 {
		t.Fatal("poll-only pane remained after closing its tab")
	}

	beforeProject, err := f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err := f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Model the held state after a Herdr restart without restarting the fixture's Lead or Worker.
	if err := f.db.SetProjectServerStartedAt(context.Background(), f.project.ID, "stale-herdr-generation"); err != nil {
		t.Fatal(err)
	}
	beforeServerGeneration, err := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts := watchRestoreCounts(t, f)
	beforeAudit := watchAuditRows(t, f)
	leadSession := watchLeadSession(t, client, beforeProject.LeadPaneID)

	doctor := runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !strings.Contains(doctor, "Project Watch shop") || !strings.Contains(doctor, "poll-only pane missing") || !strings.Contains(doctor, `"check":"Herdr"`) {
		t.Fatalf("doctor did not report the Project watcher separately from machine health: %s", doctor)
	}
	if len(watchPanes(t, client)) != 0 || len(watchPIDs(f.root)) != 0 {
		t.Fatal("doctor repaired the missing watcher as a side effect")
	}

	recovery := runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	if !strings.Contains(recovery, "held_projects") || !strings.Contains(recovery, "shop") {
		t.Fatalf("recover --all did not leave the held Project alone: %s", recovery)
	}
	if len(watchPanes(t, client)) != 0 || len(watchPIDs(f.root)) != 0 {
		t.Fatal("recover --all restored a watcher for the held Project")
	}

	repair := exec.Command(f.binary, "watch", "--repair")
	repair.Dir, repair.Env = f.repo, f.env
	if output, err := repair.CombinedOutput(); err != nil {
		t.Fatalf("watch repair failed: %v\n%s", err, output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPanes(t, client)) == 1 && len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("repair did not restore exactly one poll-only pane and process: panes=%v pids=%v", watchPanes(t, client), watchPIDs(f.root))
	}
	assertWatchRestoreState(t, f, client, beforeProject, beforeRecovery, beforeServerGeneration, beforeCounts, beforeAudit, configPath, configBefore, leadSession, taskBefore)

	paneID := watchPanes(t, client)[0].PaneID
	if output := runPosse(t, f.binary, f.repo, f.env, "watch", "--repair"); !strings.Contains(output, paneID) {
		t.Fatalf("idempotent repair did not retain pane %s: %s", paneID, output)
	}
	if panes := watchPanes(t, client); len(panes) != 1 || panes[0].PaneID != paneID {
		t.Fatalf("repeat repair changed the poll-only pane: %#v, want %s", panes, paneID)
	}
	if pids := watchPIDs(f.root); len(pids) != 1 {
		t.Fatalf("repeat repair left %d poll-only processes: %v", len(pids), pids)
	}
	assertWatchRestoreState(t, f, client, beforeProject, beforeRecovery, beforeServerGeneration, beforeCounts, beforeAudit, configPath, configBefore, leadSession, taskBefore)

	for _, pid := range watchPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("poll-only process survived SIGKILL with its pane restored: %v", watchPIDs(f.root))
	}
	deadPaneID := watchPanes(t, client)[0].PaneID
	doctor = runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !strings.Contains(doctor, "poll-only pane="+deadPaneID) || !strings.Contains(doctor, "poll-only processes=0") {
		t.Fatalf("doctor did not distinguish a pane with a dead poller: %s", doctor)
	}
	if panes := watchPanes(t, client); len(panes) != 1 || panes[0].PaneID != deadPaneID {
		t.Fatalf("doctor changed the dead poll-only pane: %#v", panes)
	}

	beforeProject, err = f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err = f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeServerGeneration, err = f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts = watchRestoreCounts(t, f)
	beforeAudit = watchAuditRows(t, f)
	leadSession = watchLeadSession(t, client, beforeProject.LeadPaneID)
	if output := runPosse(t, f.binary, f.repo, f.env, "watch", "--repair"); !strings.Contains(output, "pane") {
		t.Fatalf("repair of a dead poller did not report its pane: %s", output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPanes(t, client)) == 1 && len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("repair did not replace the dead poller: panes=%v pids=%v", watchPanes(t, client), watchPIDs(f.root))
	}
	if paneID := watchPanes(t, client)[0].PaneID; paneID == deadPaneID {
		t.Fatalf("repair reused the dead poller's pane %s", deadPaneID)
	}
	assertWatchRestoreState(t, f, client, beforeProject, beforeRecovery, beforeServerGeneration, beforeCounts, beforeAudit, configPath, configBefore, leadSession, taskBefore)
}

func seedWatchRepairTask(t *testing.T, f *prLifecycleFixture, client *herdr.Client) store.Task {
	t.Helper()
	ctx := context.Background()
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.Call(ctx, "tab.create", map[string]any{"workspace_id": f.project.HerdrWorkspaceID, "cwd": f.repo, "label": "posse:shop:t1", "focus": false})
	if err != nil {
		t.Fatalf("create fixture Worker pane: %v", err)
	}
	var opened struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil || opened.RootPane.PaneID == "" {
		t.Fatalf("decode fixture Worker pane: %s (%v)", raw, err)
	}
	if _, err := client.Run(ctx, "pane", "report-agent", opened.RootPane.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark fixture Worker working: %v", err)
	}
	taskWorktree := filepath.Join(f.root, "posse", "pool", "sandbox-1")
	gitTest(t, f.env, f.repo, "worktree", "add", "--detach", taskWorktree, "HEAD")
	session := "watch-repair-session"
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Preserve Worker state", State: store.StateSpawning, MergeMode: "pr",
		HerdrWorkspaceID: f.project.HerdrWorkspaceID, PaneID: opened.RootPane.PaneID, PaneLabel: "posse:shop:t1", WorktreePath: taskWorktree,
		AgentName: "posse-shop-t1-2", AgentSession: session, AgentSessionKind: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "fixture Worker is active"); err != nil {
		t.Fatal(err)
	}
	for launch := 0; launch < 2; launch++ {
		if _, err := f.db.NextTaskLaunchWithIdentity(ctx, taskID, "deep", "", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.UpdateTaskObservation(ctx, taskID, opened.RootPane.PaneID, f.project.HerdrWorkspaceID, session, "claude", 0, 0, snapshot.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func watchAuditRows(t *testing.T, f *prLifecycleFixture) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for _, table := range []string{"signals", "transitions", "notices", "task_launch_identities"} {
		rows, err := f.db.QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatalf("read %s audit rows: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatalf("read %s columns: %v", table, err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s row: %v", table, err)
			}
			parts := make([]string, len(columns))
			for index, column := range columns {
				parts[index] = column + "=" + fmt.Sprintf("%#v", values[index])
			}
			result[table] = append(result[table], strings.Join(parts, ","))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("read %s rows: %v", table, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close %s rows: %v", table, err)
		}
	}
	return result
}

func watchRestoreCounts(t *testing.T, f *prLifecycleFixture) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64)
	for name, query := range map[string]string{
		"tasks":             `SELECT COUNT(*) FROM tasks`,
		"signals":           `SELECT COUNT(*) FROM signals`,
		"transitions":       `SELECT COUNT(*) FROM transitions`,
		"notices":           `SELECT COUNT(*) FROM notices`,
		"launches":          `SELECT COALESCE(SUM(launches),0) FROM tasks`,
		"sessions":          `SELECT COUNT(*) FROM tasks WHERE agent_session<>''`,
		"launch_identities": `SELECT COUNT(*) FROM task_launch_identities`,
	} {
		var count int64
		if err := f.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		counts[name] = count
	}
	return counts
}

func assertWatchRestoreState(t *testing.T, f *prLifecycleFixture, client *herdr.Client, projectBefore store.Project, recoveryBefore store.ProjectRecovery, serverGeneration string, counts map[string]int64, audit map[string][]string, configPath string, configBefore []byte, leadSession string, taskBefore store.Task) {
	t.Helper()
	project, err := f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != projectBefore.LeadPaneID || project.HerdrWorkspaceID != projectBefore.HerdrWorkspaceID || project.LeadLabel != projectBefore.LeadLabel || project.LeadLaunches != projectBefore.LeadLaunches {
		t.Fatalf("repair changed Lead location or launches: before=%#v after=%#v", projectBefore, project)
	}
	if got := watchLeadSession(t, client, project.LeadPaneID); got != leadSession {
		t.Fatalf("repair changed Lead session: before=%q after=%q", leadSession, got)
	}
	task, err := f.db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.PaneID != taskBefore.PaneID || task.HerdrWorkspaceID != taskBefore.HerdrWorkspaceID || task.PaneLabel != taskBefore.PaneLabel || task.AgentSession != taskBefore.AgentSession || task.AgentSessionKind != taskBefore.AgentSessionKind || task.AgentName != taskBefore.AgentName || task.Launches != taskBefore.Launches || task.State != taskBefore.State {
		t.Fatalf("repair changed Worker pane/session/launch state: before=%#v after=%#v", taskBefore, task)
	}
	state, err := f.db.WatchRecovery(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	panes := watchPanes(t, client)
	if len(panes) != 1 || state.PaneID != panes[0].PaneID || panes[0].WorkspaceID != f.project.HerdrWorkspaceID {
		t.Fatalf("recorded Watch does not match its unique labeled pane: runtime=%#v panes=%#v", state, panes)
	}
	pids := watchPIDs(f.root)
	processPane := ""
	if len(pids) == 1 {
		processPane = watchProcessPaneID(pids[0])
	}
	if len(pids) != 1 || processPane != panes[0].PaneID {
		t.Fatalf("poll-only process does not match recorded pane %s: pids=%v pane=%q", panes[0].PaneID, pids, processPane)
	}
	afterRecovery, err := f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRecovery != recoveryBefore {
		t.Fatalf("repair changed recovery generation/owner: before=%#v after=%#v", recoveryBefore, afterRecovery)
	}
	afterServerGeneration, err := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil || afterServerGeneration != serverGeneration {
		t.Fatalf("repair changed server generation: %q -> %q (%v)", serverGeneration, afterServerGeneration, err)
	}
	if after := watchRestoreCounts(t, f); !equalWatchRestoreCounts(after, counts) {
		notices, _ := f.db.Notices(context.Background(), project.ID, false)
		t.Fatalf("repair changed executor or event counts: before=%v after=%v notices=%#v", counts, after, notices)
	}
	if after := watchAuditRows(t, f); !equalWatchAuditRows(after, audit) {
		t.Fatalf("repair changed Signals, transitions, Notices or launch identities: before=%v after=%v", audit, after)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatalf("repair changed Project config: err=%v", err)
	}
	if pids := watchPIDs(f.root); len(pids) != 1 {
		t.Fatalf("poll-only process count = %d, want 1: %v", len(pids), pids)
	}
}

func watchProcessPaneID(pid int) string {
	environ, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, entry := range strings.Split(string(environ), "\x00") {
		if strings.HasPrefix(entry, "HERDR_PANE_ID=") {
			return strings.TrimPrefix(entry, "HERDR_PANE_ID=")
		}
	}
	return ""
}

func equalWatchAuditRows(left, right map[string][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for table, rows := range left {
		if strings.Join(rows, "\n") != strings.Join(right[table], "\n") {
			return false
		}
	}
	return true
}

func watchLeadSession(t *testing.T, client *herdr.Client, paneID string) string {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return string(pane.AgentSession)
		}
	}
	t.Fatalf("Lead pane %s is missing", paneID)
	return ""
}

func equalWatchRestoreCounts(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
