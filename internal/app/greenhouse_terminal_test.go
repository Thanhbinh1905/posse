package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseReviewRecordsCancelledTerminalRunAsFailure(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateWorking)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=1`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), f.project.ID, "fixture", "lead-pane", "lead"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(context.Background(), f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	grant := "shop/t1/shop/r1"
	reason := "executor exceeded its 10-minute limit"
	f.service.greenhouseRound = func(context.Context, greenhouse.Round) (greenhouse.RoundResult, error) {
		return greenhouse.RoundResult{Grant: grant, RunID: "cancelled-run"}, &greenhouse.TerminalReviewError{Status: "cancelled", Reason: reason}
	}
	binDir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, output, stderr := f.run("_greenhouse", "review", "t1")
	if code == 0 {
		t.Fatalf("cancelled Greenhouse review unexpectedly succeeded: %s %s", output, stderr)
	}
	requests, err := f.db.GreenhouseReviewRequests(context.Background(), f.project.ID)
	if err != nil || len(requests) != 1 || requests[0].Outcome != "failed" || requests[0].Grant != grant || requests[0].RequestedHead != head {
		t.Fatalf("cancelled Greenhouse request ledger = %+v, %v; CLI exit=%d stdout=%s stderr=%s", requests, err, code, output, stderr)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "greenhouse_review_failed" || !strings.Contains(notices[0].Summary, `status "cancelled"`) || !strings.Contains(notices[0].Summary, reason) {
		t.Fatalf("cancelled Greenhouse failure Notice = %+v", notices)
	}
}
