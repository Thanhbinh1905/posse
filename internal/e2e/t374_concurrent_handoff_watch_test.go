//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
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
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Worker Signal received"); err != nil {
		t.Fatal(err)
	}
	noticeID, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: taskID, Kind: "task_done", Summary: "concurrent handoff"})
	if err != nil {
		t.Fatal(err)
	}
	secondErr := second.Wait()
	if secondErr == nil || !strings.Contains(secondOutput.String(), `"code":"watch_already_active"`) {
		t.Fatalf("second Watch was not refused with watch_already_active: err=%v output=%s", secondErr, secondOutput.String())
	}
	var refusal struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(secondOutput.Bytes(), &refusal); err != nil || refusal.Code != "watch_already_active" {
		t.Fatalf("decode active-Watch refusal %s: %+v, %v", secondOutput.String(), refusal, err)
	}
	fields := strings.Fields(refusal.Message)
	activePID, err := strconv.Atoi(strings.TrimRight(fields[len(fields)-1], ")."))
	if err != nil || activePID != first.Process.Pid {
		t.Fatalf("active Watch PID was not named: first pid=%d refusal=%q parse err=%v", first.Process.Pid, refusal.Message, err)
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
	pending, err := f.db.UndeliveredNotices(ctx, f.project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("handoff should hold the Notice once: pending=%#v err=%v", pending, err)
	}
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notice_delivery_receipts WHERE project_id=? AND destination=?`, f.project.ID, "pi:concurrent-session").Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery receipts = %d, %v; want one", count, err)
	}
}
