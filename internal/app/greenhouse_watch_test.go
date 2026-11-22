package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDoctorReportsTheRunningGreenhouseExecutable(t *testing.T) {
	home := t.TempDir()
	greenhouseHome := greenhouse.HomeFor(home)
	if err := os.MkdirAll(greenhouseHome, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(greenhouseHome, "socket"))
	if err != nil {
		t.Skipf("Unix sockets are unavailable: %v", err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(greenhouseHome, "pid"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(greenhouse.DaemonState{PID: os.Getpid(), Binary: "/configured/greenhouse", Range: greenhouse.Range{Min: 1, Max: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(greenhouseHome, "posse-daemon.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := greenhouse.RunningExecutable(os.Getpid())
	if err != nil {
		t.Skipf("process executable lookup is unavailable: %v", err)
	}
	_, status, detail, _ := greenhouseDaemonCheck(home)
	if status != "ok" || !strings.Contains(detail, want) || strings.Contains(detail, "/configured/greenhouse") {
		t.Fatalf("Greenhouse daemon check = %q, %q; want running executable %q", status, detail, want)
	}
}

func TestGreenhouseBinaryIsFoundOutsideTheCallerPATH(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, ".local", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(bin, "greenhouse")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	got, err := greenhouseBinary()
	if err != nil || got != binary {
		t.Fatalf("greenhouseBinary() = %q, %v; want %q", got, err, binary)
	}
}

func TestGreenhouseWatchUsesGreenhouseTimestampsAndFallsBackWhenAbsent(t *testing.T) {
	started := "2026-11-20T00:00:00Z"
	stageStarted := "2026-11-20T00:02:00Z"
	stageStartedAt := "2026-11-20T00:01:00Z"
	stageFinished := "2026-11-20T00:03:00Z"
	var snapshot greenhouseWatchSnapshot
	if err := json.Unmarshal([]byte(`{"run":{"started_at":"`+started+`","stage_started_at":"`+stageStartedAt+`","pipeline":{"stages":[{"name":"review","started_at":"`+stageStarted+`","finished_at":"`+stageFinished+`"}]}}}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	runAt, stageAt, finishedAt := greenhouseStageTimestamps(snapshot, "review")
	wantRun := greenhouseTimestampMillis(started)
	wantStage := greenhouseTimestampMillis(stageStartedAt)
	wantFinished := greenhouseTimestampMillis(stageFinished)
	if runAt != wantRun || stageAt != wantStage || finishedAt != wantFinished {
		t.Fatalf("parsed Greenhouse timestamps = %d/%d/%d, want %d/%d/%d", runAt, stageAt, finishedAt, wantRun, wantStage, wantFinished)
	}
	if greenhouseTimestampMillis("not-a-timestamp") != 0 {
		t.Fatal("invalid timestamps should trigger observation-time fallback")
	}
	var pipelineFallback greenhouseWatchSnapshot
	if err := json.Unmarshal([]byte(`{"run":{"started_at":"`+started+`","pipeline":{"stages":[{"name":"review","started_at":"`+stageStarted+`"}]}}}`), &pipelineFallback); err != nil {
		t.Fatal(err)
	}
	_, fallbackStage, _ := greenhouseStageTimestamps(pipelineFallback, "review")
	if fallbackStage != greenhouseTimestampMillis(stageStarted) {
		t.Fatalf("pipeline stage timestamp fallback = %d, want %d", fallbackStage, greenhouseTimestampMillis(stageStarted))
	}
	var legacy greenhouseWatchSnapshot
	if err := json.Unmarshal([]byte(`{"run":{"started_at":"`+started+`","pipeline":{"stages":[{"name":"review","status":"running"}]}}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	legacyRun, legacyStage, legacyFinished := greenhouseStageTimestamps(legacy, "review")
	if legacyRun != wantRun || legacyStage != 0 || legacyFinished != 0 {
		t.Fatalf("timestamp-less stage = %d/%d/%d, want run time and observation fallbacks", legacyRun, legacyStage, legacyFinished)
	}
	watch := store.GreenhouseTaskWatch{Stage: "review", RunObservedAt: greenhouseTimestampMillis(started), StageObservedAt: greenhouseTimestampMillis(stageStarted)}
	if got := greenhouseElapsedSummary(watch, time.UnixMilli(greenhouseTimestampMillis(stageStarted)+3*60*1000)); got != "review 3m · run 5m" {
		t.Fatalf("legacy elapsed summary = %q", got)
	}
}

func TestGreenhouseWatchStageUsesObservedElapsedTime(t *testing.T) {
	started := time.Unix(100, 0)
	snapshot := greenhouseWatchSnapshot{Status: "running", Stage: "pipeline"}
	snapshot.Run.Pipeline = &struct {
		RepairUsed  int `json:"repair_used"`
		RepairLimit int `json:"repair_limit"`
		Stages      []struct {
			Name       string `json:"name"`
			StartedAt  string `json:"started_at"`
			FinishedAt string `json:"finished_at"`
		} `json:"stages"`
		Active *struct {
			Stage string `json:"stage"`
		} `json:"active"`
	}{Active: &struct {
		Stage string `json:"stage"`
	}{Stage: "test"}}
	stage, status := greenhouseStageLabel(snapshot)
	if stage != "test" || status != "running" {
		t.Fatalf("stage/status = %q/%q, want test/running", stage, status)
	}
	if got := greenhouseElapsedLabel(started, started.Add(4*time.Minute)); got != "4m" {
		t.Fatalf("elapsed label = %q, want 4m", got)
	}
}

func TestGreenhouseWatchSkipsDoneSignalsWithoutRunID(t *testing.T) {
	db, project, _ := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Pipeline", MergeMode: "greenhouse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSignal(ctx, taskID, "done", "older pipeline", map[string]string{"greenhouse_run_id": "run-older"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSignal(ctx, taskID, "done", "newer signal without run", map[string]string{"greenhouse_run_id": ""}); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got := greenhouseRunIDFromSignals(ctx, db, task); got != "run-older" {
		t.Fatalf("Greenhouse run ID = %q, want run-older", got)
	}
}

func TestGreenhouseWatchReusesDoneSignalRunID(t *testing.T) {
	db, project, _ := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Pipeline", MergeMode: "greenhouse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSignal(ctx, taskID, "done", "pipeline complete", map[string]string{"greenhouse_run_id": "run-1046"}); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got := greenhouseRunIDFromSignals(ctx, db, task); got != "run-1046" {
		t.Fatalf("Greenhouse run ID = %q, want run-1046", got)
	}
	for _, test := range []struct {
		status string
		want   string
	}{{"ci-ready", "PR green"}, {"needs-decision", "waiting for decision"}, {"failed", "failed: review"}} {
		if err := db.SaveGreenhouseTaskWatch(ctx, store.GreenhouseTaskWatch{TaskID: taskID, RunID: "run-1046", Status: test.status, Stage: "review", RunObservedAt: 1000, StageObservedAt: 1000}); err != nil {
			t.Fatal(err)
		}
		got, err := greenhouseTaskStatus(ctx, db, task, time.UnixMilli(2000))
		if err != nil || got != test.want {
			t.Fatalf("Greenhouse status %q = %q, %v; want %q", test.status, got, err, test.want)
		}
	}
}

func TestGreenhouseWatchFollowsDoneSignalRunAfterWorkerFinishes(t *testing.T) {
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	taskPath := filepath.Join(home, "sandbox")
	if err := os.MkdirAll(taskPath, 0o700); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Pipeline", State: store.StateSpawning, MergeMode: "greenhouse", WorktreePath: taskPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddSignal(ctx, taskID, "done", "pipeline complete", map[string]string{"greenhouse_run_id": "run-1046"}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	inspect := filepath.Join(bin, "greenhouse")
	script := `#!/bin/sh
[ "$GREENHOUSE_HOME" = "` + greenhouse.HomeFor(home) + `" ] || exit 10
[ -z "$POSSE_TASK_ID" ] || exit 11
[ -z "$HERDR_PANE_ID" ] || exit 12
request=$(cat)
case "$request" in
  *run-1046*) printf '%s\n' '{"run_id":"run-1046","run":{"id":"run-1046","status":"ci-ready","stage":"ci"}}' ;;
  *) printf '%s\n' '{}' ;;
esac
`
	if err := os.WriteFile(inspect, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREENHOUSE_HOME", filepath.Join(home, "wrong-greenhouse-home"))
	t.Setenv("POSSE_TASK_ID", "t999")
	t.Setenv("HERDR_PANE_ID", "wrong-pane")
	service := New(home, nil)
	if err := service.observeGreenhouseTasks(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	watch, err := db.GreenhouseTaskWatch(ctx, taskID)
	if err != nil || watch.RunID != "run-1046" || watch.Status != "ci-ready" || watch.Stage != "ci" {
		t.Fatalf("Watch did not follow the completed Signal's run: %+v, %v", watch, err)
	}
}

func TestGreenhouseWatchPersistsRunAndReportsSidebarAndPane(t *testing.T) {
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	ctx := context.Background()
	taskPath := filepath.Join(home, "sandbox")
	if err := os.MkdirAll(taskPath, 0o700); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Pipeline", State: store.StateSpawning, MergeMode: "greenhouse", WorktreePath: taskPath, HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveGreenhouseTaskWatch(ctx, store.GreenhouseTaskWatch{TaskID: taskID, RunID: "run-1", Status: "running", Stage: "review", RunObservedAt: 1795136400000, StageObservedAt: 1795136400000, RunStartedAt: 1795136400000, StageStartedAt: 1795136460000, UpdatedAt: 1795136460000}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	inspect := filepath.Join(bin, "greenhouse")
	if err := os.WriteFile(inspect, []byte("#!/bin/sh\nprintf '%s\\n' '{\"outcome\":\"ok\",\"run_id\":\"run-1\",\"run\":{\"id\":\"run-1\",\"status\":\"running\",\"stage\":\"pipeline\",\"started_at\":\"2026-11-20T01:00:00Z\",\"stage_started_at\":\"2026-11-20T01:01:00Z\",\"pipeline\":{\"active\":{\"stage\":\"lint\"}}}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", CWD: taskPath}}}
	adapter.Results["pane.split"] = json.RawMessage(`{"pane":{"pane_id":"w2:p2"}}`)
	service := New(home, adapter)
	if err := service.observeGreenhouseTasks(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	watch, err := db.GreenhouseTaskWatch(ctx, taskID)
	if err != nil || watch.RunID != "run-1" || watch.Stage != "lint" || watch.Status != "running" || watch.PaneID != "w2:p2" || watch.RunObservedAt == 0 || watch.StageObservedAt == 0 || watch.RunStartedAt != greenhouseTimestampMillis("2026-11-20T01:00:00Z") || watch.StageStartedAt != greenhouseTimestampMillis("2026-11-20T01:01:00Z") {
		t.Fatalf("persisted Greenhouse watch = %+v, %v", watch, err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := greenhouseTaskStatus(ctx, db, task, time.Now()); err != nil || !strings.HasPrefix(status, "lint ") || !strings.Contains(status, "run ") {
		t.Fatalf("Greenhouse Task status = %q, %v", status, err)
	}
	methods := make([]string, 0, len(adapter.Calls))
	for _, call := range adapter.Calls {
		methods = append(methods, call.Method)
	}
	if !greenhouseWatchContains(methods, "workspace.report_metadata") || !greenhouseWatchContains(methods, "pane.split") || !greenhouseWatchContains(methods, "pane.send_input") {
		t.Fatalf("Herdr did not receive live status metadata and a Greenhouse pane: %v", methods)
	}
}

func greenhouseWatchContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
