//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestMidTaskQuotaNoticeReachesPiPastHeldReceipt(t *testing.T) {
	fixture := newQuotaFixture(t, [][2]string{
		{"defaults.stall_after", "1s"},
		{"profiles.primary.kind", "claude"},
		{"profiles.primary.model", "quota-silent"},
	})
	task := fixture.startWithoutEmittingQuota(t)
	fixture.waitLog(t, "primary claude "+filepath.Clean(task.WorktreePath))
	if data, err := os.ReadFile(filepath.Join(task.WorktreePath, "quota-work.txt")); err != nil || string(data) != "work before the quota ran out\n" {
		t.Fatalf("mid-task progress missing: %q %v", data, err)
	}
	ctx := context.Background()
	db := fixture.base.db
	project := fixture.base.project
	oldID, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "old held completion"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{oldID}, "pi:historical-session", "old-generation", "old-owner", now, 100)
	if err != nil || !claimed {
		t.Fatalf("old claim: %v %v", claimed, err)
	}
	if err := db.MarkNoticeDeliveryPrinted(ctx, old.DeliveryID, old.OwnerToken, now); err != nil {
		t.Fatal(err)
	}
	if err := db.ResolveNoticeDelivery(ctx, project.ID, old.DeliveryID, old.OwnerToken, "uncertain", now); err != nil {
		t.Fatal(err)
	}
	watch := exec.Command(fixture.base.binary, "watch", "--poll-only", "--timeout", "30000")
	watch.Dir, watch.Env = fixture.base.repo, fixture.base.leadEnv
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Process.Kill(); _ = watch.Wait() }()
	fixture.emitQuota(t, 1)
	notice := fixture.waitNotice(t, "quota_exhausted")
	env := setEnv(fixture.base.leadEnv, "POSSE_E2E_PI_EXTENSION", filepath.Join(moduleRoot(t), "internal/app/lead_pi_extension.ts"))
	env = setEnv(env, "POSSE_E2E_EXPECT_NOTICE", "quota_exhausted")
	command := exec.Command("node", filepath.Join(moduleRoot(t), "internal/e2e/testdata/pi_notice_busy_followup.mjs"))
	command.Dir, command.Env = fixture.base.repo, env
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("quota adapter: %v\n%s", err, output)
	}
	current := fixture.base.mustTask(t, "t1")
	if current.Launches != task.Launches || current.WorktreePath != task.WorktreePath {
		t.Fatalf("quota changed launch without approval: %+v", current)
	}
	held, err := db.NoticeDelivery(ctx, old.DeliveryID)
	if err != nil || held.State != "uncertain" {
		t.Fatalf("old receipt changed: %+v %v", held, err)
	}
	delivered, err := db.NoticesByIDs(ctx, project.ID, []int64{notice.ID})
	if err != nil || delivered[0].DeliveredAt == 0 {
		t.Fatalf("quota acceptance missing: %+v %v", delivered, err)
	}
}
