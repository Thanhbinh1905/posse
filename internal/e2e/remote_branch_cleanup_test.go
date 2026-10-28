//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// TestMergedPRTeardownDeletesRemoteBranch answers the forge read the way gh
// 2.46 does, without headRepository.nameWithOwner, as the Project Watch saw it
// when merged branches stayed on origin (#644).
func TestMergedPRTeardownDeletesRemoteBranch(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	// GitHub always names its default branch; the bare fixture origin does not.
	gitTest(t, f.env, f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	brief := filepath.Join(f.root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR lifecycle change\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	if err := os.WriteFile(filepath.Join(f.root, "gh-legacy-repository"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := f.mustTask(t, "t1")
	if head := strings.TrimSpace(gitTest(t, f.env, f.remote, "rev-parse", "refs/heads/"+candidate.Branch)); head != candidate.GatedSHA {
		t.Fatalf("remote Task branch head=%s, gated head=%s", head, candidate.GatedSHA)
	}
	merge := f.mergeOnLocalOrigin(t, candidate, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	if task := f.mustTask(t, "t1"); task.State != store.StateTornDown {
		t.Fatalf("merged PR did not tear down: %#v", task)
	}
	if remote := strings.TrimSpace(gitTest(t, f.env, f.remote, "for-each-ref", "refs/heads/"+candidate.Branch)); remote != "" {
		var cause string
		_ = f.db.QueryRow(`SELECT cause FROM remote_branch_cleanups WHERE task_id=?`, candidate.ID).Scan(&cause)
		t.Fatalf("Teardown kept the merged remote branch %s (recorded cause %q)", remote, cause)
	}
}
