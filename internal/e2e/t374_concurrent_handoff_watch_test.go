//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTwoConcurrentHandoffWatchesForOneDestination(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	ctx := context.Background()
	noticeID, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "concurrent handoff"})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	outputs := make([][]byte, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := range outputs {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			cmd := exec.Command(f.binary, "watch", "--json", "--quiet-routine", "--handoff", "--destination", "pi:concurrent-session", "--timeout", "3000")
			cmd.Dir, cmd.Env = f.repo, f.leadEnv
			<-start
			outputs[i], errs[i] = cmd.CombinedOutput()
		}(i)
	}
	close(start)
	group.Wait()

	for i := range outputs {
		if errs[i] != nil && !strings.Contains(string(outputs[i]), `"state":"timeout"`) {
			t.Errorf("Watch %d failed: %v\n%s", i, errs[i], outputs[i])
		}
		if strings.Contains(string(outputs[i]), "internal_error") || strings.Contains(string(outputs[i]), "Task state changed concurrently") {
			t.Errorf("Watch %d leaked a state race: %s", i, outputs[i])
		}
	}
	var deliveries []string
	for _, output := range outputs {
		var payload struct {
			State   string `json:"state"`
			Notices []struct {
				ID int64 `json:"id"`
			} `json:"notices"`
			Delivery struct {
				DeliveryID string `json:"delivery_id"`
			} `json:"delivery"`
		}
		if err := json.Unmarshal(output, &payload); err != nil {
			if strings.Contains(string(output), `"state":"timeout"`) {
				continue
			}
			t.Fatalf("decode Watch response %s: %v", output, err)
		}
		if len(payload.Notices) > 0 {
			if len(payload.Notices) != 1 || payload.Notices[0].ID != noticeID || payload.Delivery.DeliveryID == "" {
				t.Fatalf("unexpected delivery batch: %+v", payload)
			}
			deliveries = append(deliveries, payload.Delivery.DeliveryID)
		}
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected exactly one Watch to hand off the Notice, got delivery IDs %v and outputs %q", deliveries, outputs)
	}
	pending, err := f.db.UndeliveredNotices(ctx, f.project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("handoff should hold the Notice exactly once: pending=%#v err=%v", pending, err)
	}
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notice_delivery_receipts WHERE project_id=? AND destination=?`, f.project.ID, "pi:concurrent-session").Scan(&count); err != nil || count != 1 {
		t.Fatalf("delivery receipts = %d, %v; want one", count, err)
	}
}
