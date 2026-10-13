package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAckReportedReviewPreservesMountAttachments(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	attachPRFixtureMount(t, f)
	fake := f.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	f.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(f)
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET type='review' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateReported, "cli", "report saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"finished\"\n[remuda]\nclean = \"pristine\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.worktree, "reports", "t1-probes")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("repro evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "change.txt"), []byte("uncommitted edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, ".gitignore"), []byte("ignored-evidence/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(f.worktree, "ignored-evidence")
	if err := os.MkdirAll(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignored, "trace.txt"), []byte("ignored trace"), 0600); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveExplicitAttachment(task, f.home, f.project, "ignored-evidence"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "report.md"), []byte("See reports/t1-probes/probe.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "task_done", Summary: "Review finished"})
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := f.run("ack", fmt.Sprint(id))
	if code != 0 {
		if !strings.Contains(out, "herdr_conditional_close_unavailable") {
			t.Fatalf("ack failed for an unexpected reason: %s %s", out, stderr)
		}
		if content, err := os.ReadFile(filepath.Join(f.worktree, "change.txt")); err != nil || string(content) != "uncommitted edit" {
			t.Fatalf("failed teardown did not preserve Mount edit: %q %v", content, err)
		}
		return
	}
	path := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "reports", "t1-probes", "probe.txt")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "repro evidence" {
		t.Fatalf("attachment lost: %q %v; ack=%s", content, err, out)
	}
	for _, check := range []struct{ path, want string }{{"change.txt", "uncommitted edit"}, {"ignored-evidence/trace.txt", "ignored trace"}} {
		got, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "tasks", "t1", check.path))
		if err != nil || string(got) != check.want {
			t.Fatalf("%s lost: %q %v", check.path, got, err)
		}
	}
	code, out, stderr = f.run("show", "t1")
	if code != 0 || !strings.Contains(out, "reports/t1-probes/probe.txt") || !strings.Contains(out, "ignored-evidence/trace.txt") {
		t.Fatalf("show omitted attachments: %d %s %s", code, out, stderr)
	}
}

func TestScoutDoneSignalCopiesExplicitIgnoredAttachments(t *testing.T) {
	f := newPRLandingFixture(t, "local", store.StateWorking)
	defer f.db.Close()
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET type='scout' WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "report.md"), []byte("See ignored/probe.txt and ignored/log.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, ".gitignore"), []byte("ignored/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.worktree, "ignored")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe.txt", "log.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, stderr := f.run("holler", "done", "Report ready", "--report", "report.md", "--attach", "ignored/probe.txt", "--attach", "ignored/log.txt")
	if code != 0 {
		t.Fatalf("done --attach: %s %s", out, stderr)
	}
	for _, name := range []string{"probe.txt", "log.txt"} {
		saved, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "ignored", name))
		if err != nil || string(saved) != name {
			t.Fatalf("%s missing: %q %v", name, saved, err)
		}
	}
}

func TestClosedPRInvalidObservationSupersedesClosedDecision(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/41", ""); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.RecordPRObservation(ctx, store.PRObservation{ProjectID: f.project.ID, TaskID: task.ID, PRURL: task.PRURL, State: "CLOSED", HeadSHA: f.headSHA}, store.PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	if err := raiseClosedPRDecision(ctx, f.db, f.project, task, task.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := raiseInvalidPRDecision(ctx, f.db, f.project, task); err != nil {
		t.Fatal(err)
	}
	if err := raiseClosedPRDecision(ctx, f.db, f.project, task, task.PRURL); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 1 || !strings.Contains(pending[0].Origin, "invalid:") {
		t.Fatalf("conflicting PR Decisions: %#v %v", pending, err)
	}
}

func TestExistingMergeOnlyLeftoverDecisionBecomesObsolete(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	gitTest(t, f.repo, "branch", "posse/t1-leftover", "main")
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "leftover", Origin: "leftover:posse/t1-leftover", Question: "Open a Task from this Leftover?", Options: []string{"open-task", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.obsoleteEmptyLeftovers(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("merge-only Leftover still pending: %#v %v", pending, err)
	}
	saved, err := f.db.Decision(ctx, f.project.ID, decision.ID)
	if err != nil || saved.ObsoleteAt == 0 {
		t.Fatalf("Decision not marked obsolete: %#v %v", saved, err)
	}
}

func TestMergeOnlyLeftoverDoesNotAskUser(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	gitTest(t, f.repo, "merge", "--no-ff", "--no-edit", "posse/t1")
	if err := os.WriteFile(filepath.Join(f.repo, "upstream.txt"), []byte("upstream"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "upstream.txt")
	gitTest(t, f.repo, "commit", "-m", "upstream")
	gitTest(t, f.worktree, "merge", "--no-edit", "main")
	if err := snapshotUnmergedMemberWork(ctx, f.db, f.project, f.task, ""); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("merge-only Leftover: %#v %v", decisions, err)
	}
}
