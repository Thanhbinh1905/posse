//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Only command timing is changed. The Rider, Landing and prune use the real CLI.
func TestPrunePreservesBranchCheckedOutAfterFinalCheck(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed work\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	task := f.mustTask(t, "t1")
	merged := f.mergeOnLocalOrigin(t, task, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merged, task.GatedSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	task = f.mustTask(t, "t1")
	if task.State != store.StateTornDown {
		t.Fatalf("Task did not tear down: %#v", task)
	}
	ref := "refs/heads/" + task.Branch
	gitTest(t, f.env, f.repo, "update-ref", ref, task.GatedSHA)
	dry := runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune")
	if !strings.Contains(dry, task.Branch) {
		t.Fatalf("safe leftover branch was not planned: %s", dry)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(f.root, "branch-shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, "prune-before-branch-delete")
	resume := marker + ".continue"
	shim := "#!/bin/sh\nif [ \"$1\" = -C ] && [ \"$2\" = " + shellQuote(f.repo) + " ] && { { [ \"$3\" = branch ] && [ \"$4\" = -D ]; } || { [ \"$3\" = update-ref ] && [ \"$4\" = -d ]; }; }; then printf ready > " + shellQuote(marker) + "; while [ ! -f " + shellQuote(resume) + " ]; do sleep 0.02; done; fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	prune := exec.Command(f.binary, "remuda", "prune", "--yes")
	prune.Dir, prune.Env = f.repo, setEnv(f.leadEnv, "PATH", shimDir+":"+t202EnvValue(f.leadEnv, "PATH"))
	output := filepath.Join(f.root, "branch-prune-output")
	out, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	prune.Stdout, prune.Stderr = out, out
	if err := prune.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(resume, []byte("resume"), 0o600)
		_ = prune.Process.Kill()
		_ = prune.Wait()
	}()
	if !waitForCondition(10*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		contents, _ := os.ReadFile(output)
		t.Fatalf("prune did not reach deletion after its final checks: %s", contents)
	}
	// Ordinary Git checkout does not participate in Posse's Mount-state lock.
	foreign := filepath.Join(f.root, "foreign-worktree")
	gitTest(t, f.env, f.repo, "worktree", "add", foreign, task.Branch)
	if err := os.WriteFile(filepath.Join(foreign, "uncommitted-foreign-work.txt"), []byte("foreign work must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resume, []byte("resume"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneErr := prune.Wait()
	contents, _ := os.ReadFile(output)
	t.Logf("prune exit=%v output=%s", pruneErr, contents)
	if pruneErr != nil {
		t.Fatalf("prune failed instead of reporting the checked-out branch as skipped: %v %s", pruneErr, contents)
	}
	got, refErr := gitCommand(f.env, f.repo, "rev-parse", "--verify", ref)
	head, headErr := gitCommand(f.env, foreign, "rev-parse", "--verify", "HEAD")
	status, statusErr := gitCommand(f.env, foreign, "status", "--short", "--branch")
	t.Logf("foreign HEAD=%q err=%v; status=%q err=%v", head, headErr, status, statusErr)
	if refErr != nil || strings.TrimSpace(got) != task.GatedSHA || headErr != nil || strings.TrimSpace(head) != task.GatedSHA {
		t.Fatalf("REPRO: prune deleted a branch checked out after its final check, leaving foreign HEAD unborn: expected=%s ref=%q refErr=%v HEAD=%q headErr=%v", task.GatedSHA, got, refErr, head, headErr)
	}
	if !strings.Contains(string(contents), "skipped") {
		t.Fatalf("prune did not report concurrent checkout as skipped: %s", contents)
	}
	if statusErr != nil || !strings.Contains(status, "uncommitted-foreign-work.txt") {
		t.Fatalf("foreign worktree changes did not survive: status=%q err=%v", status, statusErr)
	}
}
