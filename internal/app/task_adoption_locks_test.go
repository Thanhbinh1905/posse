package app

import (
	"context"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAdoptedHeldSandboxReconcilesLockAndReleases(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "launch"
		if interrupted {
			name = "interrupted admission"
		}
		t.Run(name, func(t *testing.T) {
			f, brief := newTaskAdoptionFixture(t)
			ctx := context.Background()
			if err := lockSandbox(ctx, f.repo, f.worktree, f.task.Seq); err != nil {
				t.Fatal(err)
			}
			if code, out, stderr := f.run("start", "--brief", brief, "--profile", "maintenance"); code != 0 {
				t.Fatalf("adoption failed: %d %s %s", code, out, stderr)
			}
			next, err := f.db.Task(ctx, f.project.ID, "t2")
			if err != nil {
				t.Fatal(err)
			}
			if !interrupted {
				reason, err := sandboxLockReason(ctx, f.repo, f.worktree)
				if err != nil || reason != "posse: held by t2" {
					t.Fatalf("launch did not transfer lock: %q %v", reason, err)
				}
			}
			if interrupted {
				if err := unlockSandbox(ctx, f.repo, f.worktree); err != nil {
					t.Fatal(err)
				}
				if err := lockSandbox(ctx, f.repo, f.worktree, f.task.Seq); err != nil {
					t.Fatal(err)
				}
			}
			if err := ensureHeldSandboxLocks(ctx, f.db, f.project); err != nil {
				t.Fatalf("reconcile transferred Sandbox: %v", err)
			}
			reason, err := sandboxLockReason(ctx, f.repo, f.worktree)
			if err != nil || reason != "posse: held by t2" {
				t.Fatalf("adopted lock=%q err=%v", reason, err)
			}
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", "MERGED")
			if _, err := f.db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,merge_commit,observed_at) VALUES(?,?,?,?,'MERGED',?,1)`, f.project.ID, next.ID, next.PRURL, f.headSHA, f.headSHA); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET state='merged',gated_sha=?,landed_ref=? WHERE id=?`, f.headSHA, f.headSHA, next.ID); err != nil {
				t.Fatal(err)
			}
			if code, out, stderr := f.run("teardown", "t2"); code != 0 {
				t.Fatalf("tear down adopting Task: %d %s %s", code, out, stderr)
			}
			sandbox, err := f.db.SandboxByTask(ctx, next.ID)
			if !store.IsNotFound(err) {
				t.Fatalf("released Sandbox ownership: %+v, %v", sandbox, err)
			}
			reason, err = sandboxLockReason(ctx, f.repo, f.worktree)
			if err != nil || reason != "" {
				t.Fatalf("released lock=%q err=%v", reason, err)
			}
		})
	}
}
