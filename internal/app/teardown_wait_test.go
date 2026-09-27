package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentCLIReportsTeardownInProgressInsteadOfLanded(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateLanding, "cli", "PR ready"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateLanding, store.StateLanded, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "unsaddle", "mount.stop", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := f.db.IntentByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if err := waitForActiveTeardowns(limited, f.db, f.project.ID); err == nil || !strings.Contains(err.Error(), "Teardown is in progress") {
		t.Fatalf("half-torn-down Task was presented as landed: %v", err)
	}
	if err := f.db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		t.Fatal(err)
	}
	if err := waitForActiveTeardowns(ctx, f.db, f.project.ID); err != nil {
		t.Fatal(err)
	}
}
