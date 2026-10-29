//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTwoConcurrentHandoffWatchesForOneDestination(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	ctx := context.Background()
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 1, Type: "change", Title: "state transition during handoff Watch", ShortName: "handoff-race"})
	if err != nil {
		t.Fatal(err)
	}

	first := exec.Command(f.binary, "watch", "--json", "--quiet-routine", "--handoff", "--destination", "pi:concurrent-session")
	first.Dir, first.Env = f.repo, f.leadEnv
	var firstOutput bytes.Buffer
	first.Stdout, first.Stderr = &firstOutput, &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if first.ProcessState == nil {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	}()
	time.Sleep(300 * time.Millisecond)

	second := exec.Command(f.binary, "watch", "--json", "--quiet-routine", "--handoff", "--destination", "pi:concurrent-session", "--timeout", "3000")
	second.Dir, second.Env = f.repo, f.leadEnv
	var secondOutput bytes.Buffer
	second.Stdout, second.Stderr = &secondOutput, &secondOutput
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	// On the unfixed branch both processes stay in the wait loop here. The
	// Task transition below races with two real handoff Watch processes.
	time.Sleep(300 * time.Millisecond)
	status := watchStatus(t, f.binary, f.repo, f.leadEnv, "pi:concurrent-session")
	if status["state"] != "active" || status["pid"] != float64(first.Process.Pid) || status["started_at"] == nil {
		t.Fatalf("status for a live handoff Watch = %v, want active with pid %d and start time", status, first.Process.Pid)
	}
	if status := watchStatus(t, f.binary, f.repo, f.leadEnv, "pi:other-session"); status["state"] != "inactive" || status["pid"] != nil {
		t.Fatalf("status for an unwatched destination = %v, want inactive", status)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Worker Signal received"); err != nil {
		t.Fatal(err)
	}
	noticeID, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: taskID, Kind: "task_done", Summary: "concurrent handoff"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second Watch for a live destination failed: %v\n%s", err, secondOutput.String())
	}
	var duplicate map[string]any
	if err := json.Unmarshal(secondOutput.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode already-active result %s: %v", secondOutput.String(), err)
	}
	if duplicate["state"] != "already_active" || duplicate["destination"] != "pi:concurrent-session" || duplicate["pid"] != float64(first.Process.Pid) {
		t.Fatalf("second Watch did not name the live Watch: first pid=%d result=%s", first.Process.Pid, secondOutput.String())
	}
	if _, found := duplicate["notices"]; found {
		t.Fatalf("already-active result looks like a Notice batch: %s", secondOutput.String())
	}
	if _, found := duplicate["delivery"]; found {
		t.Fatalf("already-active result looks like a Notice batch: %s", secondOutput.String())
	}
	startedAt, ok := duplicate["started_at"].(string)
	if _, err := time.Parse(time.RFC3339, startedAt); !ok || err != nil {
		t.Fatalf("already-active result has no start time: %s", secondOutput.String())
	}

	if err := first.Wait(); err != nil {
		t.Fatalf("first Watch failed: %v\n%s", err, firstOutput.String())
	}
	var handoff struct {
		Notices []struct {
			ID int64 `json:"id"`
		} `json:"notices"`
		Delivery struct {
			DeliveryID string `json:"delivery_id"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(firstOutput.Bytes(), &handoff); err != nil || len(handoff.Notices) != 1 || handoff.Notices[0].ID != noticeID || handoff.Delivery.DeliveryID == "" {
		t.Fatalf("first Watch did not hand off exactly the pending Notice: %s; error=%v", firstOutput.String(), err)
	}
	if status := watchStatus(t, f.binary, f.repo, f.leadEnv, "pi:concurrent-session"); status["state"] != "inactive" {
		t.Fatalf("status after the handoff Watch exited = %v, want inactive", status)
	}
	pending, err := f.db.UndeliveredNotices(ctx, f.project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("handoff should hold the Notice once: pending=%#v err=%v", pending, err)
	}
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notice_delivery_receipts WHERE project_id=? AND destination=?`, f.project.ID, "pi:concurrent-session").Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery receipts = %d, %v; want one", count, err)
	}
}

func watchStatus(t *testing.T, binary, dir string, env []string, destination string) map[string]any {
	t.Helper()
	command := exec.Command(binary, "watch", "--json", "--status", "--destination", destination)
	command.Dir, command.Env = dir, env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("watch --status failed: %v\n%s", err, output)
	}
	var status map[string]any
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("decode watch --status %s: %v", output, err)
	}
	return status
}
